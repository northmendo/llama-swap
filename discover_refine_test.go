package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cliKeyEnv  = "LLAMA_SWAP_TEST_CLI_ASSISTANT_KEY"
	cliKey     = "sk-cli-test-provider-key-do-not-leak"
	cliModel   = "test-refiner-model"
	cliModelID = "tiny-q4-k-m" // ModelIDFromFilename("tiny-Q4_K_M.gguf")
)

// cliGGUFBuilder assembles a minimal GGUF v3 KV section in memory
// (mirrors the agentapi test fixture).
type cliGGUFBuilder struct {
	buf bytes.Buffer
}

func (b *cliGGUFBuilder) writeLE(size int, v uint64) {
	var out [8]byte
	binary.LittleEndian.PutUint64(out[:], v)
	b.buf.Write(out[:size])
}

func (b *cliGGUFBuilder) writeString(s string) {
	b.writeLE(8, uint64(len(s)))
	b.buf.WriteString(s)
}

func (b *cliGGUFBuilder) kv(key string, typeCode int, encode func()) {
	b.writeString(key)
	b.writeLE(4, uint64(typeCode))
	encode()
}

func (b *cliGGUFBuilder) stringKV(key, value string) {
	b.kv(key, 8, func() { b.writeString(value) }) // 8 = GGUF string type
}

func (b *cliGGUFBuilder) uint32KV(key string, value uint32) {
	b.kv(key, 4, func() { b.writeLE(4, uint64(value)) }) // 4 = GGUF uint32
}

// cliModelGGUF is a minimal llama chat model with a 4096-token context.
func cliModelGGUF() []byte {
	kv := &cliGGUFBuilder{}
	kv.stringKV("general.architecture", "llama")
	kv.stringKV("general.type", "model")
	kv.uint32KV("llama.context_length", 4096)

	var out bytes.Buffer
	out.WriteString("GGUF")
	var b8 [8]byte
	binary.LittleEndian.PutUint32(b8[:4], 3) // version
	out.Write(b8[:4])
	binary.LittleEndian.PutUint64(b8[:], 1) // tensor count
	out.Write(b8[:])
	binary.LittleEndian.PutUint64(b8[:], 3) // KV count
	out.Write(b8[:])
	out.Write(kv.buf.Bytes())
	return out.Bytes()
}

// newDiscoverEnv writes a scratch layout for `llama-swap discover`:
// engine binary, one .gguf model, and a config wiring discovery +
// draft_assistant at the mock provider's URL.
func newDiscoverEnv(t *testing.T, apiBase string) (cfgPath, modelPath string) {
	t.Helper()
	scratch := t.TempDir()
	modelDir := filepath.Join(scratch, "models")
	require.NoError(t, os.MkdirAll(modelDir, 0o755))

	binary := filepath.Join(scratch, "fake-llama-server")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	modelPath = filepath.Join(modelDir, "tiny-Q4_K_M.gguf")
	require.NoError(t, os.WriteFile(modelPath, cliModelGGUF(), 0o644))

	cfgPath = filepath.Join(scratch, "config.yaml")
	yamlText := fmt.Sprintf(`healthCheckTimeout: 30
engines:
  testbuild:
    label: "test build"
    cmd: %s --flash-attn

models:
  existing:
    engine: testbuild
    args: |
      --port ${PORT}
      --model /does/not/matter.gguf

discovery:
  enabled: true
  folders:
    - %s
  ctxSizeCap: 2048

draft_assistant:
  enabled: true
  api_base: %s
  key_env: %s
  model: %s
  timeout: 5s
  max_tokens: 256
  conventions: "Always keep --flash-attn."
`, binary, modelDir, apiBase, cliKeyEnv, cliModel)
	require.NoError(t, os.WriteFile(cfgPath, []byte(yamlText), 0o600))
	return cfgPath, modelPath
}

// newCLIProvider starts a mock chat-completions server. answerFn renders
// the assistant message from the decoded request, so tests can keep the
// --model path out of the prompt without knowing scratch paths up front.
func newCLIProvider(t *testing.T, answerFn func(prompt string) string, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK || answerFn == nil {
			http.Error(w, "mock provider blew up", http.StatusInternalServerError)
			return
		}
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		prompt := ""
		for _, m := range body.Messages {
			prompt += m.Content + "\n"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": answerFn(prompt)}},
			},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// cliKeepsModelPath answers with a refined stanza that keeps whatever
// --model path the prompt carried. The system prompt also contains the
// literal text "--model with...", so the LAST match wins: the rule
// draft's actual --model line comes after it.
func cliKeepsModelPath(prompt string) string {
	matches := modelPathRe.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return "no path in prompt"
	}
	modelPath := matches[len(matches)-1][1]
	return fmt.Sprintf("```yaml\n%s:\n  engine: testbuild\n  args: |\n    --port ${PORT}\n    --model %s\n    --ctx-size 4096\n    --flash-attn\n```\n", cliModelID, modelPath)
}

var modelPathRe = regexp.MustCompile(`--model (\S+)`)

func TestRunDiscover_RefineSuccess(t *testing.T) {
	t.Setenv(cliKeyEnv, cliKey)
	server := newCLIProvider(t, cliKeepsModelPath, http.StatusOK)
	cfgPath, _ := newDiscoverEnv(t, server.URL)

	var out bytes.Buffer
	code := runDiscover([]string{"-config", cfgPath, "--refine"}, &out)
	require.Equal(t, 0, code, out.String())

	report := out.String()
	assert.Contains(t, report, "refined: yes (draft_assistant)")
	assert.Contains(t, report, "--flash-attn")
	assert.Contains(t, report, "--ctx-size 4096")
	assert.Contains(t, report, "draft_assistant refinement (model "+cliModel+")")
	// The rule draft capped ctx at 2048; the refinement raised it.
	assert.NotContains(t, report, "capped from 4096")

	// The provider key is never printed.
	assert.NotContains(t, report, cliKey)
}

func TestRunDiscover_RefineFallbackOnProvider500(t *testing.T) {
	t.Setenv(cliKeyEnv, cliKey)
	server := newCLIProvider(t, nil, http.StatusInternalServerError)
	cfgPath, _ := newDiscoverEnv(t, server.URL)

	var out bytes.Buffer
	code := runDiscover([]string{"-config", cfgPath, "--refine"}, &out)
	require.Equal(t, 0, code, "a fallback is still a successful scan")

	report := out.String()
	assert.Contains(t, report, "refined: no (fallback to rule-based draft:")
	assert.Contains(t, report, "provider call failed")
	assert.Contains(t, report, "HTTP 500")
	// The rule draft is still printed, with its capped ctx.
	assert.Contains(t, report, "capped from 4096")
	assert.Contains(t, report, "--ctx-size 2048")
	assert.NotContains(t, report, cliKey)
}

func TestRunDiscover_RefineFallbackOnGarbage(t *testing.T) {
	t.Setenv(cliKeyEnv, cliKey)
	server := newCLIProvider(t, func(string) string {
		return "Sorry, I only speak prose today."
	}, http.StatusOK)
	cfgPath, _ := newDiscoverEnv(t, server.URL)

	var out bytes.Buffer
	code := runDiscover([]string{"-config", cfgPath, "--refine"}, &out)
	require.Equal(t, 0, code)

	report := out.String()
	assert.Contains(t, report, "refined: no (fallback to rule-based draft:")
	assert.Contains(t, report, "rejected")
}

func TestRunDiscover_RefineMissingEnvKey(t *testing.T) {
	// key_env deliberately not set: the config error is printed, the
	// rule draft still comes out, and the scan exits 0.
	t.Setenv(cliKeyEnv, "")
	server := newCLIProvider(t, func(string) string { return "never used" }, http.StatusOK)
	cfgPath, _ := newDiscoverEnv(t, server.URL)

	var out bytes.Buffer
	code := runDiscover([]string{"-config", cfgPath, "--refine"}, &out)
	require.Equal(t, 0, code, "refinement problems must not fail the scan")

	report := out.String()
	assert.Contains(t, report, "discover: refine ")
	assert.Contains(t, report, cliKeyEnv)
	assert.Contains(t, report, "is not set")
	assert.Contains(t, report, "refined: no")
	// ... and the rule-based draft is still there.
	assert.Contains(t, report, "draft (paste under models:):")
}

func TestRunDiscover_RefineWithoutSection(t *testing.T) {
	server := newCLIProvider(t, func(string) string { return "never used" }, http.StatusOK)
	cfgPath, _ := newDiscoverEnv(t, server.URL)

	// Strip the draft_assistant section from the config.
	raw, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	idx := strings.Index(string(raw), "draft_assistant:")
	require.Greater(t, idx, 0)
	require.NoError(t, os.WriteFile(cfgPath, raw[:idx], 0o600))

	var out bytes.Buffer
	code := runDiscover([]string{"-config", cfgPath, "--refine"}, &out)
	assert.Equal(t, 1, code)
	assert.Contains(t, out.String(), "--refine requires an enabled draft_assistant section")
}

func TestRunDiscover_RefineJSON(t *testing.T) {
	t.Setenv(cliKeyEnv, cliKey)
	server := newCLIProvider(t, cliKeepsModelPath, http.StatusOK)
	cfgPath, _ := newDiscoverEnv(t, server.URL)

	// The JSON report carries the refinement outcome and the refined
	// draft inside results.
	var out bytes.Buffer
	code := runDiscover([]string{"-config", cfgPath, "--refine", "--json"}, &out)
	require.Equal(t, 0, code, out.String())

	var report struct {
		Found   int `json:"found"`
		Drafted int `json:"drafted"`
		Results []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
			Draft  *struct {
				ModelID string `json:"modelId"`
				Args    string `json:"args"`
				YAML    string `json:"yaml"`
			} `json:"draft"`
		} `json:"results"`
		Refinements []refineNote `json:"refinements"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	require.Equal(t, 1, report.Found)
	require.Equal(t, 1, report.Drafted)
	require.Len(t, report.Results, 1)
	require.NotNil(t, report.Results[0].Draft)

	assert.Len(t, report.Refinements, 1)
	note := report.Refinements[0]
	assert.Equal(t, report.Results[0].Path, note.Path)
	assert.Equal(t, cliModelID, note.ModelID)
	assert.True(t, note.Refined)
	assert.Empty(t, note.Reason)

	assert.Contains(t, report.Results[0].Draft.Args, "--flash-attn")
	assert.Contains(t, report.Results[0].Draft.YAML, "--flash-attn")
	assert.NotContains(t, out.String(), cliKey)
}
