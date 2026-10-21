package draftassistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
)

const (
	// providerKeyEnv names the environment variable the tests put the
	// fake provider key in, exactly how real deployments do it.
	providerKeyEnv = "LLAMA_SWAP_TEST_PROVIDER_KEY"
	providerKey    = "sk-test-provider-key-never-leak-me"
	testModel      = "test-refiner-model"
)

// writeEngineBinary creates a fake executable engine binary so configs
// with an engines section pass config validation.
func writeEngineBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-llama-server")
	content := "#!/bin/sh\nexit 0\n"
	if strings.Contains(t.Name(), "windows") {
		path += ".exe"
		content = "exit 0\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	return path
}

// mockProvider stands in for the OpenAI-compatible endpoint: an httptest
// server plus a mutex-guarded record of what it was asked.
type mockProvider struct {
	server *httptest.Server

	mu          sync.Mutex
	hits        int
	lastAuth    string
	lastRequest chatRequest
}

// newMockProvider starts a server answering with the given chat content
// and status. answer may be empty when status is not 2xx. delayMS makes
// the handler stall before answering (timeout tests).
func newMockProvider(t *testing.T, status int, answer string, delayMS int) *mockProvider {
	t.Helper()
	m := &mockProvider{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delayMS > 0 {
			time.Sleep(time.Duration(delayMS) * time.Millisecond)
		}
		m.mu.Lock()
		m.hits++
		m.lastAuth = r.Header.Get("Authorization")
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.lastRequest = req
		m.mu.Unlock()

		if status < 200 || status > 299 {
			http.Error(w, "mock provider blew up", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": answer}},
			},
		})
	}))
	t.Cleanup(m.server.Close)
	return m
}

// loadTestConfig loads a real config through the full pipeline: engines,
// one existing model, a discovery-less doc and the draft_assistant
// section pointed at the mock provider.
func loadTestConfig(t *testing.T, apiBase string, mutate func(string) string) config.Config {
	t.Helper()
	scratch := t.TempDir()
	binary := writeEngineBinary(t, scratch)

	yamlText := `healthCheckTimeout: 30
engines:
  testbuild:
    label: "test build"
    cmd: ` + binary + ` --flash-attn
    env:
      - SECRET_TOKEN=hush-hush-value

models:
  existing:
    engine: testbuild
    args: |
      --port ${PORT}
      --model /does/not/matter.gguf

draft_assistant:
  enabled: true
  api_base: ` + apiBase + `
  key_env: ` + providerKeyEnv + `
  model: ` + testModel + `
  timeout: 5s
  max_tokens: 128
  conventions: "Prefer adding --flash-attn when the build supports it."
`
	if mutate != nil {
		yamlText = mutate(yamlText)
	}

	cfgPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(yamlText), 0o600))

	cfg, err := config.LoadConfig(cfgPath)
	require.NoError(t, err)
	return cfg
}

// testDraft builds the rule-based draft the refinement starts from: the
// file path only feeds the model ID, so no GGUF is needed on disk.
func testDraft() (discovery.Metadata, discovery.Draft) {
	meta := discovery.Metadata{
		Path:          "/models/tiny-Q4_K_M.gguf",
		FileSize:      1234,
		GGUFVersion:   3,
		Architecture:  "llama",
		Type:          "model",
		ContextLength: 4096,
		QuantHint:     "Q4_K_M",
	}
	rule := discovery.Draft{
		ModelID:      "tiny-q4-k-m",
		Engine:       "testbuild",
		EngineSource: "default: first defined engine",
		Args:         "--port ${PORT}\n--model /models/tiny-Q4_K_M.gguf\n--ctx-size 4096",
		CtxSize:      4096,
		YAML:         "  tiny-q4-k-m:\n    engine: testbuild\n    args: |\n      --port ${PORT}\n      --model /models/tiny-Q4_K_M.gguf\n      --ctx-size 4096",
	}
	return meta, rule
}

// refinedAnswer is a well-formed LLM answer: fenced, correct model ID,
// known engine, ${PORT} and the model path kept, plus a tuned flag.
const refinedAnswer = "Here is the refined stanza:\n\n```yaml\ntiny-q4-k-m:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model /models/tiny-Q4_K_M.gguf\n    --ctx-size 4096\n    --flash-attn\n```\n"

func TestRefine_Success(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	mock := newMockProvider(t, http.StatusOK, refinedAnswer, 0)
	cfg := loadTestConfig(t, mock.server.URL, nil)
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.NoError(t, err)

	// The refined draft replaced the rule draft and passed validation.
	assert.True(t, outcome.Refined)
	assert.Empty(t, outcome.Reason)
	assert.Equal(t, "tiny-q4-k-m", outcome.Draft.ModelID)
	assert.Equal(t, "testbuild", outcome.Draft.Engine)
	assert.Contains(t, outcome.Draft.Args, "--flash-attn")
	assert.Contains(t, outcome.Draft.Args, "--port ${PORT}")
	assert.Contains(t, outcome.Draft.Args, "--model /models/tiny-Q4_K_M.gguf")
	assert.Equal(t, int64(4096), outcome.Draft.CtxSize)
	assert.Contains(t, outcome.Draft.YAML, "tiny-q4-k-m:")
	assert.Contains(t, outcome.Draft.YAML, "--flash-attn")
	assert.Contains(t, outcome.Draft.EngineSource, "draft_assistant")

	// The stanza renders paste-ready: model key indented two spaces,
	// args as a literal block.
	assert.Contains(t, outcome.Draft.YAML, "\n    args: |")
	assert.True(t, strings.HasPrefix(outcome.Draft.YAML, "  tiny-q4-k-m:"))

	// The provider saw a proper chat-completions request.
	mock.mu.Lock()
	require.Equal(t, 1, mock.hits)
	assert.Equal(t, "Bearer "+providerKey, mock.lastAuth)
	assert.Equal(t, testModel, mock.lastRequest.Model)
	assert.Equal(t, 128, mock.lastRequest.MaxTokens)
	assert.False(t, mock.lastRequest.Stream)
	require.Len(t, mock.lastRequest.Messages, 2)
	mock.mu.Unlock()

	// The prompt carries metadata JSON, the rule draft, the engines
	// (cmd + label) and the conventions.
	user := mock.lastRequest.Messages[1].Content
	assert.Contains(t, user, `"architecture": "llama"`)
	assert.Contains(t, user, `"contextLength": 4096`)
	assert.Contains(t, user, "--ctx-size 4096")
	assert.Contains(t, user, "testbuild")
	assert.Contains(t, user, "fake-llama-server")
	assert.Contains(t, user, "test build")
	assert.Contains(t, user, "Prefer adding --flash-attn when the build supports it.")
	// Engine env VALUES never travel to the provider.
	assert.NotContains(t, user, "hush-hush-value")
	assert.Contains(t, user, "SECRET_TOKEN")
}

func TestRefine_SuccessWithoutFences(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	answer := "tiny-q4-k-m:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model /models/tiny-Q4_K_M.gguf\n    --ctx-size 2048\n"
	mock := newMockProvider(t, http.StatusOK, answer, 0)
	cfg := loadTestConfig(t, mock.server.URL, nil)
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.NoError(t, err)
	assert.True(t, outcome.Refined)
	assert.Equal(t, int64(2048), outcome.Draft.CtxSize)
	// ctx 2048 < trained 4096: reported as capped.
	assert.True(t, outcome.Draft.CtxCapped)
}

func TestRefine_ProseGarbage_Fallback(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	mock := newMockProvider(t, http.StatusOK, "Sure! The best stanza for your model is one with a big context. Hope that helps!", 0)
	cfg := loadTestConfig(t, mock.server.URL, nil)
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.NoError(t, err, "garbage answers fall back, they do not fail")
	assert.False(t, outcome.Refined)
	assert.NotEmpty(t, outcome.Reason, "the reason must be surfaced")
	assert.Contains(t, outcome.Reason, "rejected")
	// The rule draft is returned untouched.
	assert.Equal(t, rule, outcome.Draft)
}

func TestRefine_InvalidYAML_Fallback(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	mock := newMockProvider(t, http.StatusOK, "```yaml\n: - [unclosed\n```", 0)
	cfg := loadTestConfig(t, mock.server.URL, nil)
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.NoError(t, err)
	assert.False(t, outcome.Refined)
	assert.Contains(t, outcome.Reason, "YAML parse error")
	assert.Equal(t, rule, outcome.Draft)
}

func TestRefine_Provider500_Fallback(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	mock := newMockProvider(t, http.StatusInternalServerError, "", 0)
	cfg := loadTestConfig(t, mock.server.URL, nil)
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.NoError(t, err)
	assert.False(t, outcome.Refined)
	assert.Contains(t, outcome.Reason, "provider call failed")
	assert.Contains(t, outcome.Reason, "HTTP 500")
	assert.Equal(t, rule, outcome.Draft)
}

func TestRefine_Timeout_Fallback(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	// The handler stalls past the configured 200ms timeout.
	mock := newMockProvider(t, http.StatusOK, refinedAnswer, 750)
	cfg := loadTestConfig(t, mock.server.URL, func(y string) string {
		return strings.Replace(y, "timeout: 5s", "timeout: 200ms", 1)
	})
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.NoError(t, err)
	assert.False(t, outcome.Refined)
	assert.Contains(t, outcome.Reason, "provider call failed")
	// context deadline exceeded, wrapped in a *url.Error.
	assert.Contains(t, outcome.Reason, "context deadline exceeded")
	assert.Equal(t, rule, outcome.Draft)
}

func TestRefine_MissingEnvKey_IsConfigError(t *testing.T) {
	// The env var named by key_env is not set: a configuration error,
	// surfaced as ErrMisconfigured, and the provider is never called.
	t.Setenv(providerKeyEnv, "")
	mock := newMockProvider(t, http.StatusOK, refinedAnswer, 0)
	cfg := loadTestConfig(t, mock.server.URL, nil)
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMisconfigured), "want ErrMisconfigured, got %v", err)
	assert.Contains(t, err.Error(), providerKeyEnv)
	assert.Contains(t, err.Error(), "is not set")
	assert.Equal(t, RefineOutcome{}, outcome)

	mock.mu.Lock()
	assert.Equal(t, 0, mock.hits, "the provider must not be called without a key")
	mock.mu.Unlock()
}

func TestRefine_SectionNotEnabled_IsConfigError(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	mock := newMockProvider(t, http.StatusOK, refinedAnswer, 0)
	cfg := loadTestConfig(t, mock.server.URL, func(y string) string {
		return strings.Replace(y, "enabled: true", "enabled: false", 1)
	})
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMisconfigured))
	assert.Empty(t, outcome.Reason)

	mock.mu.Lock()
	assert.Equal(t, 0, mock.hits)
	mock.mu.Unlock()
}

func TestRefine_HardGateRejections(t *testing.T) {
	// Every answer below parses as YAML but must be rejected by the
	// gate: wrong model ID, unknown engine, missing ${PORT}, unknown
	// field, multiple models.
	cases := []struct {
		name   string
		answer string
		want   string // substring of the fallback reason
	}{
		{
			name:   "wrong model id",
			answer: "renamed-model:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model /models/tiny-Q4_K_M.gguf\n",
			want:   `expected a stanza for model "tiny-q4-k-m"`,
		},
		{
			name:   "unknown engine",
			answer: "tiny-q4-k-m:\n  engine: no-such-engine\n  args: |\n    --port ${PORT}\n    --model /models/tiny-Q4_K_M.gguf\n",
			want:   "failed config validation",
		},
		{
			name:   "missing PORT",
			answer: "tiny-q4-k-m:\n  engine: testbuild\n  args: |\n    --model /models/tiny-Q4_K_M.gguf\n",
			want:   "failed config validation",
		},
		{
			name:   "engine and cmd together",
			answer: "tiny-q4-k-m:\n  engine: testbuild\n  cmd: /bin/true --port ${PORT}\n  args: |\n    --port ${PORT}\n    --model /models/tiny-Q4_K_M.gguf\n",
			want:   "failed config validation",
		},
		{
			name:   "unknown model field",
			answer: "tiny-q4-k-m:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model /models/tiny-Q4_K_M.gguf\n  bogusField: 1\n",
			want:   "does not match the model-stanza schema",
		},
		{
			name:   "two models",
			answer: "tiny-q4-k-m:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model /a.gguf\nother-model:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model /b.gguf\n",
			want:   "expected exactly one model stanza, got 2",
		},
		{
			name:   "no engine field",
			answer: "tiny-q4-k-m:\n  args: |\n    --port ${PORT}\n",
			want:   "no engine field",
		},
		{
			name:   "no args field",
			answer: "tiny-q4-k-m:\n  engine: testbuild\n",
			want:   "no args field",
		},
		{
			name:   "not a mapping",
			answer: "- just\n- a\n- list\n",
			want:   "not a YAML mapping",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(providerKeyEnv, providerKey)
			mock := newMockProvider(t, http.StatusOK, tc.answer, 0)
			cfg := loadTestConfig(t, mock.server.URL, nil)
			meta, rule := testDraft()

			outcome, err := Refine(context.Background(), cfg, meta, rule)
			require.NoError(t, err)
			assert.False(t, outcome.Refined)
			assert.Contains(t, outcome.Reason, tc.want)
			assert.Equal(t, rule, outcome.Draft, "fallback returns the rule draft untouched")
		})
	}
}

func TestRefine_KeyNeverInOutput(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	// A leaky provider that echoes the Authorization header in its
	// error body: the fallback reason must redact the key.
	echoKey := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "your token "+r.Header.Get("Authorization")+" is bad", http.StatusUnauthorized)
	}
	server := httptest.NewServer(http.HandlerFunc(echoKey))
	t.Cleanup(server.Close)

	cfg := loadTestConfig(t, server.URL, nil)
	meta, rule := testDraft()

	outcome, err := Refine(context.Background(), cfg, meta, rule)
	require.NoError(t, err)
	assert.False(t, outcome.Refined)
	assert.NotContains(t, outcome.Reason, providerKey, "the fallback reason must not contain the API key")
	assert.Contains(t, outcome.Reason, "***")
}

func TestParseStanza_ModelsWrapper(t *testing.T) {
	answer := "```yaml\nmodels:\n  tiny-q4-k-m:\n    engine: testbuild\n    args: |\n      --port ${PORT}\n```\n"
	modelID, model, stanzaYAML, err := parseStanza(answer, "tiny-q4-k-m")
	require.NoError(t, err)
	assert.Equal(t, "tiny-q4-k-m", modelID)
	assert.Equal(t, "testbuild", model.Engine)
	assert.Contains(t, stanzaYAML, "  tiny-q4-k-m:")
	assert.Contains(t, stanzaYAML, "    args: |")
}

func TestParseStanza_EmptyAnswer(t *testing.T) {
	_, _, _, err := parseStanza("```yaml\n```", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestCtxSizeFromArgs(t *testing.T) {
	assert.Equal(t, int64(0), ctxSizeFromArgs("--port ${PORT}"))
	assert.Equal(t, int64(4096), ctxSizeFromArgs("--port ${PORT}\n--ctx-size 4096"))
	assert.Equal(t, int64(8192), ctxSizeFromArgs("--ctx-size=8192 --port ${PORT}"))
}

func TestBuildPrompt_Contents(t *testing.T) {
	t.Setenv(providerKeyEnv, providerKey)
	mock := newMockProvider(t, http.StatusOK, refinedAnswer, 0)
	cfg := loadTestConfig(t, mock.server.URL, nil)
	meta, rule := testDraft()

	system, user := buildPrompts(cfg, meta, rule)
	assert.Contains(t, system, `exactly one key: the model ID "tiny-q4-k-m"`)
	assert.Contains(t, system, "--port ${PORT}")
	assert.Contains(t, user, "## Model file metadata")
	assert.Contains(t, user, "## Rule-based draft to refine")
	assert.Contains(t, user, "## Available engines")
	assert.Contains(t, user, "## User conventions")
	assert.Contains(t, user, `"path": "/models/tiny-Q4_K_M.gguf"`)
}
