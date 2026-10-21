package agentapi

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

const testKey = "unit-test-bearer-key-3f9c"

// writeEngineBinary creates a fake executable engine binary and returns
// its path, so configs with an engines section pass config validation.
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

// ggufKVTypeString/ggufKVTypeUint32 are the GGUF v2/v3 KV type codes the
// fixture writer needs (see internal/discovery/gguf.go).
const (
	ggufKVTypeUint32 = 4
	ggufKVTypeString = 8
)

// ggufBuilder assembles a minimal GGUF KV section in memory.
type ggufBuilder struct {
	buf bytes.Buffer
}

func (b *ggufBuilder) writeLE(size int, v uint64) {
	var out [8]byte
	binary.LittleEndian.PutUint64(out[:], v)
	b.buf.Write(out[:size])
}

func (b *ggufBuilder) writeString(s string) {
	b.writeLE(8, uint64(len(s)))
	b.buf.WriteString(s)
}

func (b *ggufBuilder) kv(key string, typeCode int, encode func()) {
	b.writeString(key)
	b.writeLE(4, uint64(typeCode))
	encode()
}

func (b *ggufBuilder) stringKV(key, value string) {
	b.kv(key, ggufKVTypeString, func() { b.writeString(value) })
}

func (b *ggufBuilder) uint32KV(key string, value uint32) {
	b.kv(key, ggufKVTypeUint32, func() { b.writeLE(4, uint64(value)) })
}

// minimalModelGGUF builds a minimal but complete GGUF model file: a
// llama chat model with a 4096-token context.
func minimalModelGGUF() []byte {
	kv := &ggufBuilder{}
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

// testEnv is one mounted agent API with everything the tests need.
type testEnv struct {
	mux      *http.ServeMux
	server   *httptest.Server
	cfg      config.Config
	logBuf   *bytes.Buffer
	scratch  string // dir holding engine binary, gguf files, config file
	cfgPath  string
	modelDir string
}

// newTestEnv writes a scratch config file, loads it as a real Config,
// and mounts the agent API on it. mutating lets a test tweak the YAML
// (e.g. disable the agent_api section) before it is loaded.
func newTestEnv(t *testing.T, mutate func(yaml string) string) *testEnv {
	t.Helper()

	scratch := t.TempDir()
	modelDir := filepath.Join(scratch, "models")
	require.NoError(t, os.MkdirAll(modelDir, 0o755))

	binary := writeEngineBinary(t, scratch)
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "tiny-Q4_K_M.gguf"), minimalModelGGUF(), 0o644))

	yamlText := fmt.Sprintf(`healthCheckTimeout: 30
# a comment that must survive an apply
engines:
  testbuild:
    label: "test build"
    cmd: %s --flash-attn
    env:
      - SECRET_TOKEN=hush-hush-value
      - PLAIN_NAME=1

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

agent_api:
  enabled: true
  key: %s
`, binary, modelDir, testKey)
	if mutate != nil {
		yamlText = mutate(yamlText)
	}

	cfgPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(yamlText), 0o600))

	cfg, err := config.LoadConfig(cfgPath)
	require.NoError(t, err)

	logBuf := &bytes.Buffer{}
	mux := http.NewServeMux()
	Register(mux, Options{
		Cfg:        cfg,
		ConfigPath: func() string { return cfgPath },
		Logger:     logmon.NewWriter(logBuf),
	})

	env := &testEnv{
		mux:      mux,
		server:   httptest.NewServer(mux),
		cfg:      cfg,
		logBuf:   logBuf,
		scratch:  scratch,
		cfgPath:  cfgPath,
		modelDir: modelDir,
	}
	t.Cleanup(env.server.Close)
	return env
}

// do performs an authenticated JSON request against the mounted API.
func (e *testEnv) do(t *testing.T, method, path string, body any, key string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := e.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(payload)
}

// getOK asserts a 200 and decodes the JSON body into out.
func (e *testEnv) getOK(t *testing.T, method, path string, body any, out any) {
	t.Helper()
	status, payload := e.do(t, method, path, body, testKey)
	require.Equal(t, http.StatusOK, status, "body: %s", payload)
	require.NoError(t, json.Unmarshal([]byte(payload), out))
}

// agentRoutePaths are the mounted routes, used for 404-when-disabled
// and auth-coverage checks.
var agentRoutePaths = []struct {
	method string
	path   string
}{
	{"POST", "/agent/v1/scan"},
	{"GET", "/agent/v1/engines"},
	{"POST", "/agent/v1/draft"},
	{"POST", "/agent/v1/validate"},
	{"POST", "/agent/v1/apply"},
}

func TestAgentAPI_DisabledReturns404(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"no section": func(y string) string {
			return strings.Replace(y, "agent_api:\n  enabled: true\n  key: "+testKey+"\n", "", 1)
		},
		"enabled false": func(y string) string {
			return strings.Replace(y, "agent_api:\n  enabled: true", "agent_api:\n  enabled: false", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, mutate)
			for _, route := range agentRoutePaths {
				// Even with the correct key: nothing is mounted.
				status, body := env.do(t, route.method, route.path, nil, testKey)
				assert.Equal(t, http.StatusNotFound, status, "%s %s: %s", route.method, route.path, body)
			}
		})
	}
}

func TestAgentAPI_AuthRequired(t *testing.T) {
	env := newTestEnv(t, nil)

	for _, route := range agentRoutePaths {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			// Missing, wrong and differently-shaped tokens are all 401,
			// before any route logic runs.
			for _, header := range []struct {
				name string
				auth string
			}{
				{"missing", ""},
				{"wrong key", "Bearer wrong-key"},
				{"empty token", "Bearer "},
				{"wrong scheme", "Basic " + testKey},
				{"garbage", "nonsense"},
			} {
				req, err := http.NewRequest(route.method, env.server.URL+route.path, nil)
				require.NoError(t, err)
				if header.auth != "" {
					req.Header.Set("Authorization", header.auth)
				}
				resp, err := env.server.Client().Do(req)
				require.NoError(t, err)
				payload, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s: %s", header.name, payload)
				assert.NotContains(t, string(payload), testKey, "401 body must not echo the key")
			}

			// The correct key is accepted (engines needs nothing else).
			status, _ := env.do(t, "GET", "/agent/v1/engines", nil, testKey)
			require.Equal(t, http.StatusOK, status)
		})
	}
}

func TestAgentAPI_AuthResponsesUniform(t *testing.T) {
	env := newTestEnv(t, nil)

	// The observable response for a wrong token must not depend on how
	// wrong it is: constant-time comparison means an attacker cannot
	// distinguish "right length" from "wrong length" or learn how many
	// bytes matched. All 401s share status, body and WWW-Authenticate.
	// (A trailing-space variant is not deliverable: net/http trims
	// trailing OWS from header values before sending.)
	var previous string
	for _, token := range []string{
		"",
		"unit-test-bearer-key-3f9",   // prefix of the real key
		"unit-test-bearer-key-3f9c",  // the real key (control: 200)
		"unit-test-bearer-key-3f9cX", // one byte longer
		"unit-test-bearer-key-3f9d",  // same length, last byte wrong
		"x",                          // different length entirely
		strings.Repeat("y", 512),     // absurdly long
	} {
		status, body := env.do(t, "GET", "/agent/v1/engines", nil, token)
		if token == testKey {
			assert.Equal(t, http.StatusOK, status)
			continue
		}
		assert.Equal(t, http.StatusUnauthorized, status)
		if previous != "" {
			assert.Equal(t, previous, body, "401 bodies must be identical regardless of the token")
		}
		previous = body
	}
}

func TestAgentAPI_KeyNeverLeaks(t *testing.T) {
	env := newTestEnv(t, nil)

	requests := []struct {
		method string
		path   string
		body   any
		key    string
	}{
		{"GET", "/agent/v1/engines", nil, testKey},
		{"GET", "/agent/v1/engines", nil, "wrong"},
		{"POST", "/agent/v1/scan", map[string]any{}, testKey},
		{"POST", "/agent/v1/validate", map[string]any{"yaml": "a:\n  engine: nope"}, testKey},
		{"POST", "/agent/v1/apply", map[string]any{"yaml": "a:\n  engine: nope", "approved": true}, testKey},
		{"POST", "/agent/v1/draft", map[string]any{"path": "/no/such.gguf"}, testKey},
		{"GET", "/agent/v1/nosuchroute", nil, testKey},
	}

	for _, req := range requests {
		_, body := env.do(t, req.method, req.path, req.body, req.key)
		assert.NotContains(t, body, testKey, "%s %s leaked the key", req.method, req.path)
	}

	// Nothing was applied, but exercise the success path too via apply_test;
	// here also check the log stream the handlers write to.
	assert.NotContains(t, env.logBuf.String(), testKey, "agent API log output leaked the key")
}

func TestAgentAPI_Engines(t *testing.T) {
	env := newTestEnv(t, nil)

	var resp struct {
		Engines []engineDTO `json:"engines"`
	}
	env.getOK(t, "GET", "/agent/v1/engines", nil, &resp)

	require.Len(t, resp.Engines, 1)
	engine := resp.Engines[0]
	assert.Equal(t, "testbuild", engine.Name)
	assert.Equal(t, "test build", engine.Label)
	assert.Contains(t, engine.Cmd, "fake-llama-server")
	assert.Equal(t, []string{"SECRET_TOKEN", "PLAIN_NAME"}, engine.EnvKeys)

	// Env VALUES are never exposed.
	status, body := env.do(t, "GET", "/agent/v1/engines", nil, testKey)
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body, "hush-hush-value")
}

func TestAgentAPI_Scan(t *testing.T) {
	env := newTestEnv(t, nil)

	var resp scanResponse
	env.getOK(t, "POST", "/agent/v1/scan", map[string]any{}, &resp)

	require.Equal(t, 1, resp.Found)
	require.Equal(t, 1, resp.Drafted)
	require.Equal(t, 0, resp.Skipped)
	require.Len(t, resp.Results, 1)

	result := resp.Results[0]
	assert.Equal(t, filepath.Join(env.modelDir, "tiny-Q4_K_M.gguf"), result.Path)
	assert.Equal(t, "drafted", result.Status)
	require.NotNil(t, result.Metadata)
	assert.Equal(t, "llama", result.Metadata.Architecture)
	assert.Equal(t, int64(4096), result.Metadata.ContextLength)
	assert.Equal(t, "Q4_K_M", result.Metadata.QuantHint)

	require.NotNil(t, result.Draft)
	assert.Equal(t, "tiny-q4-k-m", result.Draft.ModelID)
	assert.Equal(t, "testbuild", result.Draft.Engine)
	assert.Contains(t, result.Draft.Args, "--port ${PORT}")
	// ctxSizeCap 2048 < trained 4096: capped.
	assert.Equal(t, int64(2048), result.Draft.CtxSize)
	assert.True(t, result.Draft.CtxCapped)
	assert.Contains(t, result.Draft.YAML, "engine: testbuild")
}

func TestAgentAPI_ScanWithoutDiscoverySection(t *testing.T) {
	env := newTestEnv(t, func(y string) string {
		idx := strings.Index(y, "discovery:")
		end := strings.Index(y, "agent_api:")
		require.Greater(t, end, idx)
		return y[:idx] + y[end:]
	})

	status, body := env.do(t, "POST", "/agent/v1/scan", map[string]any{}, testKey)
	assert.Equal(t, http.StatusBadRequest, status, body)
	assert.Contains(t, body, "no discovery section")
}

func TestAgentAPI_Draft(t *testing.T) {
	env := newTestEnv(t, nil)
	ggufPath := filepath.Join(env.modelDir, "tiny-Q4_K_M.gguf")

	var resp draftDTO
	env.getOK(t, "POST", "/agent/v1/draft", map[string]any{"path": ggufPath}, &resp)

	assert.Equal(t, "tiny-q4-k-m", resp.ModelID)
	assert.Equal(t, "testbuild", resp.Engine)
	assert.Contains(t, resp.Args, "--model "+ggufPath)
	assert.Contains(t, resp.YAML, "tiny-q4-k-m:")

	// Missing path field.
	status, body := env.do(t, "POST", "/agent/v1/draft", map[string]any{}, testKey)
	assert.Equal(t, http.StatusBadRequest, status, body)

	// Nonexistent file.
	status, body = env.do(t, "POST", "/agent/v1/draft", map[string]any{"path": "/no/such/model.gguf"}, testKey)
	assert.Equal(t, http.StatusNotFound, status, body)

	// Existing but not a GGUF.
	status, body = env.do(t, "POST", "/agent/v1/draft", map[string]any{"path": env.cfgPath}, testKey)
	assert.Equal(t, http.StatusUnprocessableEntity, status, body)
}

func TestAgentAPI_DraftRefinerSeam(t *testing.T) {
	env := newTestEnv(t, nil)
	ggufPath := filepath.Join(env.modelDir, "tiny-Q4_K_M.gguf")

	// The seam: a refinement hook (F3) can rewrite the rule-based draft.
	// Its output must still pass validation.
	original := DraftRefiner
	DraftRefiner = func(_ config.Config, _ discovery.Metadata, draft discovery.Draft) (discovery.Draft, error) {
		draft.EngineSource = "refined"
		return draft, nil
	}
	t.Cleanup(func() { DraftRefiner = original })

	var resp draftDTO
	env.getOK(t, "POST", "/agent/v1/draft", map[string]any{"path": ggufPath}, &resp)
	assert.Equal(t, "refined", resp.EngineSource)

	// A refiner error surfaces as 422 without side effects.
	DraftRefiner = func(_ config.Config, _ discovery.Metadata, draft discovery.Draft) (discovery.Draft, error) {
		return draft, fmt.Errorf("refinement failed: model too spicy")
	}
	status, body := env.do(t, "POST", "/agent/v1/draft", map[string]any{"path": ggufPath}, testKey)
	assert.Equal(t, http.StatusUnprocessableEntity, status, body)
	assert.Contains(t, body, "refinement failed: model too spicy")
}

func TestAgentAPI_Validate(t *testing.T) {
	env := newTestEnv(t, nil)

	validStanza := `newmodel:
  engine: testbuild
  args: |
    --port ${PORT}
    --model /tmp/new.gguf
`
	invalidStanza := `newmodel:
  engine: no-such-engine
  args: |
    --port ${PORT}
`
	fullConfig := `healthCheckTimeout: 30
models:
  m1:
    cmd: /bin/true --port ${PORT}
`

	t.Run("valid stanza", func(t *testing.T) {
		var resp validationResponse
		env.getOK(t, "POST", "/agent/v1/validate", map[string]any{"yaml": validStanza}, &resp)
		assert.True(t, resp.Valid)
		assert.Empty(t, resp.Errors)
	})

	t.Run("invalid stanza", func(t *testing.T) {
		var resp validationResponse
		env.getOK(t, "POST", "/agent/v1/validate", map[string]any{"yaml": invalidStanza}, &resp)
		assert.False(t, resp.Valid)
		assert.NotEmpty(t, resp.Errors)
		assert.Contains(t, strings.Join(resp.Errors, "; "), "no-such-engine")
	})

	t.Run("full config", func(t *testing.T) {
		var resp validationResponse
		env.getOK(t, "POST", "/agent/v1/validate", map[string]any{"yaml": fullConfig}, &resp)
		assert.True(t, resp.Valid)
	})

	t.Run("full config that would not load", func(t *testing.T) {
		var resp validationResponse
		env.getOK(t, "POST", "/agent/v1/validate", map[string]any{"yaml": "models:\n  m1:\n    engine: missing\n"}, &resp)
		assert.False(t, resp.Valid)
	})

	t.Run("garbage yaml is a validation result, not a transport error", func(t *testing.T) {
		var resp validationResponse
		env.getOK(t, "POST", "/agent/v1/validate", map[string]any{"yaml": ":\n  - [unclosed"}, &resp)
		assert.False(t, resp.Valid)
		assert.Contains(t, strings.Join(resp.Errors, "; "), "YAML parse error")
	})

	t.Run("validate has no side effects", func(t *testing.T) {
		before, err := os.ReadFile(env.cfgPath)
		require.NoError(t, err)
		env.getOK(t, "POST", "/agent/v1/validate", map[string]any{"yaml": validStanza}, &validationResponse{})
		after, err := os.ReadFile(env.cfgPath)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after))
	})

	t.Run("missing yaml field", func(t *testing.T) {
		status, body := env.do(t, "POST", "/agent/v1/validate", map[string]any{}, testKey)
		assert.Equal(t, http.StatusBadRequest, status, body)
	})
}

func TestAgentAPI_UnknownRoutesAndMethods(t *testing.T) {
	env := newTestEnv(t, nil)

	// Unknown path under /agent/v1/: JSON 404.
	status, body := env.do(t, "POST", "/agent/v1/nosuch", nil, testKey)
	assert.Equal(t, http.StatusNotFound, status, body)
	assert.Contains(t, body, "unknown agent API route")

	// Wrong method on a known path: falls to the catch-all, JSON 404.
	status, body = env.do(t, "GET", "/agent/v1/scan", nil, testKey)
	assert.Equal(t, http.StatusNotFound, status, body)

	// Everything under /agent/v1 still requires auth first.
	req, err := http.NewRequest("POST", env.server.URL+"/agent/v1/nosuch", nil)
	require.NoError(t, err)
	resp, err := env.server.Client().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAgentAPI_BadJSONBody(t *testing.T) {
	env := newTestEnv(t, nil)

	req, err := http.NewRequest("POST", env.server.URL+"/agent/v1/validate", strings.NewReader("this is not json"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := env.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(payload))
	assert.Contains(t, string(payload), "invalid JSON body")
}
