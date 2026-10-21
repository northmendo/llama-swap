package agentapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
)

const (
	assistantKeyEnv = "LLAMA_SWAP_TEST_ASSISTANT_KEY"
	assistantKey    = "sk-agent-test-provider-key-do-not-leak"
	assistantModel  = "test-refiner-model"
)

// refineProvider is a mock OpenAI-compatible chat-completions endpoint.
// answerFn renders the assistant message from the decoded request, so
// tests can mirror what a real model does (keep the --model path from
// the prompt) without knowing scratch paths up front.
type refineProvider struct {
	server *httptest.Server

	mu         sync.Mutex
	hits       int
	lastAuth   string
	lastPrompt string
}

// newRefineProvider starts the mock; answerFn may be nil, which makes
// every request fail with HTTP 500.
func newRefineProvider(t *testing.T, answerFn func(prompt string) (int, string)) *refineProvider {
	t.Helper()
	p := &refineProvider{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		prompt := ""
		for _, m := range body.Messages {
			prompt += m.Content + "\n"
		}

		p.mu.Lock()
		p.hits++
		p.lastAuth = r.Header.Get("Authorization")
		p.lastPrompt = prompt
		p.mu.Unlock()

		if answerFn == nil {
			http.Error(w, "mock provider unavailable", http.StatusInternalServerError)
			return
		}
		status, answer := answerFn(prompt)
		if status < 200 || status > 299 {
			http.Error(w, answer, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": answer}},
			},
		})
	}))
	t.Cleanup(p.server.Close)
	return p
}

// modelPathRe pulls the --model path out of a prompt. The system prompt
// also contains the literal text "--model with...", so the LAST match
// wins: the rule draft's actual --model line comes after it.
var modelPathRe = regexp.MustCompile(`--model (\S+)`)

func lastModelPath(prompt string) string {
	matches := modelPathRe.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// assistantSection is the `draft_assistant:` block appended to the test
// env's config, pointed at the mock provider.
func assistantSection(apiBase string) string {
	return fmt.Sprintf(`
draft_assistant:
  enabled: true
  api_base: %s
  key_env: %s
  model: %s
  timeout: 5s
  max_tokens: 256
  conventions: "Keep --flash-attn on every drafted model."
`, apiBase, assistantKeyEnv, assistantModel)
}

// newAssistantEnv is a test env whose config enables the draft
// assistant against the given mock provider. Call t.Setenv for
// assistantKeyEnv yourself (or not, for the missing-key test).
func newAssistantEnv(t *testing.T, p *refineProvider) *testEnv {
	t.Helper()
	return newTestEnv(t, func(y string) string {
		return y + assistantSection(p.server.URL)
	})
}

// ggufPathOf returns the env's model file.
func ggufPathOf(env *testEnv) string {
	return filepath.Join(env.modelDir, "tiny-Q4_K_M.gguf")
}

func TestAgentAPI_Refine_Success(t *testing.T) {
	t.Setenv(assistantKeyEnv, assistantKey)

	p := newRefineProvider(t, func(prompt string) (int, string) {
		path := lastModelPath(prompt)
		require.NotEmpty(t, path, "prompt must carry the --model path")
		require.True(t, strings.HasSuffix(path, "tiny-Q4_K_M.gguf"),
			"the draft's --model path must be picked, not prompt prose: %q", path)
		answer := "```yaml\ntiny-q4-k-m:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model " + path + "\n    --ctx-size 4096\n    --flash-attn\n```\n"
		return http.StatusOK, answer
	})
	env := newAssistantEnv(t, p)

	ggufPath := ggufPathOf(env)

	var resp refineResponse
	env.getOK(t, "POST", "/agent/v1/refine", map[string]any{"path": ggufPath}, &resp)

	assert.True(t, resp.Refined)
	assert.Empty(t, resp.Reason)
	assert.Equal(t, "tiny-q4-k-m", resp.ModelID)
	assert.Equal(t, "testbuild", resp.Engine)
	assert.Contains(t, resp.Args, "--flash-attn")
	assert.Contains(t, resp.Args, "--model "+ggufPath)
	assert.Contains(t, resp.Args, "--ctx-size 4096")
	assert.Contains(t, resp.YAML, "--flash-attn")
	assert.Contains(t, resp.EngineSource, "draft_assistant")
	// The discovery section caps drafts at 2048; the refinement is
	// allowed to raise it back to the trained 4096.
	assert.Equal(t, int64(4096), resp.CtxSize)

	// The provider call was a proper chat completion with the key from
	// the environment, and the prompt carried conventions + draft.
	p.mu.Lock()
	require.Equal(t, 1, p.hits)
	assert.Equal(t, "Bearer "+assistantKey, p.lastAuth)
	assert.Contains(t, p.lastPrompt, "Keep --flash-attn on every drafted model.")
	assert.Contains(t, p.lastPrompt, "tiny-q4-k-m")
	assert.Contains(t, p.lastPrompt, "SECRET_TOKEN")
	// Engine env VALUES never leave the server.
	assert.NotContains(t, p.lastPrompt, "hush-hush-value")
	p.mu.Unlock()

	// The provider key never appears in any response.
	status, body := env.do(t, "POST", "/agent/v1/refine", map[string]any{"path": ggufPathOf(env)}, testKey)
	assert.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body, assistantKey)
	assert.NotContains(t, env.logBuf.String(), assistantKey)
}

func TestAgentAPI_Refine_FallbackOnGarbage(t *testing.T) {
	t.Setenv(assistantKeyEnv, assistantKey)

	p := newRefineProvider(t, func(string) (int, string) {
		return http.StatusOK, "Happy to help! Unfortunately I cannot produce YAML today."
	})
	env := newAssistantEnv(t, p)

	var resp refineResponse
	env.getOK(t, "POST", "/agent/v1/refine", map[string]any{"path": ggufPathOf(env)}, &resp)

	// Fallback: 200, the untouched rule-based draft, reason surfaced.
	assert.False(t, resp.Refined)
	assert.NotEmpty(t, resp.Reason)
	assert.Contains(t, resp.Reason, "rejected")
	assert.Equal(t, "tiny-q4-k-m", resp.ModelID)
	assert.Equal(t, "testbuild", resp.Engine)
	// The rule draft: capped ctx, no --flash-attn.
	assert.Equal(t, int64(2048), resp.CtxSize)
	assert.True(t, resp.CtxCapped)
	assert.NotContains(t, resp.Args, "--flash-attn")
	assert.Contains(t, resp.Args, "--port ${PORT}")
}

func TestAgentAPI_Refine_FallbackOnProvider500(t *testing.T) {
	t.Setenv(assistantKeyEnv, assistantKey)

	p := newRefineProvider(t, nil) // every call 500s
	env := newAssistantEnv(t, p)

	var resp refineResponse
	env.getOK(t, "POST", "/agent/v1/refine", map[string]any{"path": ggufPathOf(env)}, &resp)

	assert.False(t, resp.Refined)
	assert.Contains(t, resp.Reason, "provider call failed")
	assert.Contains(t, resp.Reason, "HTTP 500")
	// Still a usable rule draft.
	assert.Equal(t, int64(2048), resp.CtxSize)
}

func TestAgentAPI_Refine_MissingEnvKeyIsConfigError(t *testing.T) {
	// key_env is deliberately NOT set: a 422 naming the variable, and
	// the provider is never called.
	t.Setenv(assistantKeyEnv, "")

	p := newRefineProvider(t, func(string) (int, string) {
		t.Error("provider must not be called without a key")
		return http.StatusOK, ""
	})
	env := newAssistantEnv(t, p)

	status, body := env.do(t, "POST", "/agent/v1/refine", map[string]any{"path": ggufPathOf(env)}, testKey)
	assert.Equal(t, http.StatusUnprocessableEntity, status, body)
	assert.Contains(t, body, assistantKeyEnv)
	assert.Contains(t, body, "is not set")

	p.mu.Lock()
	assert.Equal(t, 0, p.hits)
	p.mu.Unlock()
}

func TestAgentAPI_Refine_NotConfigured(t *testing.T) {
	// No draft_assistant section, no DraftRefiner wired: 400 with a
	// pointer to the guide.
	env := newTestEnv(t, nil)

	status, body := env.do(t, "POST", "/agent/v1/refine", map[string]any{"path": ggufPathOf(env)}, testKey)
	assert.Equal(t, http.StatusBadRequest, status, body)
	assert.Contains(t, body, "refinement is not configured")
	assert.Contains(t, body, "draft_assistant")
}

func TestAgentAPI_Refine_SeamWithoutSection(t *testing.T) {
	// The F2 seam still refines when no draft_assistant section exists.
	env := newTestEnv(t, nil)

	original := DraftRefiner
	DraftRefiner = func(_ config.Config, _ discovery.Metadata, draft discovery.Draft) (discovery.Draft, error) {
		draft.Args = draft.Args + "\n--seam-flag"
		draft.EngineSource = "seam"
		return draft, nil
	}
	t.Cleanup(func() { DraftRefiner = original })

	var resp refineResponse
	env.getOK(t, "POST", "/agent/v1/refine", map[string]any{"path": ggufPathOf(env)}, &resp)

	assert.True(t, resp.Refined)
	assert.Empty(t, resp.Reason)
	assert.Contains(t, resp.Args, "--seam-flag")
	assert.Equal(t, "seam", resp.EngineSource)
}

func TestAgentAPI_Refine_BadRequests(t *testing.T) {
	env := newTestEnv(t, nil)

	// Missing path.
	status, body := env.do(t, "POST", "/agent/v1/refine", map[string]any{}, testKey)
	assert.Equal(t, http.StatusBadRequest, status, body)

	// Nonexistent file.
	status, body = env.do(t, "POST", "/agent/v1/refine", map[string]any{"path": "/no/such.gguf"}, testKey)
	assert.Equal(t, http.StatusNotFound, status, body)

	// Refinement never writes the config file.
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	_ = status
	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "refine must not touch the config file")
}
