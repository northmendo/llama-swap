package agentapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// validStanza is a stanza that passes validation against the test env's
// testbuild engine.
const validStanza = `newmodel:
  engine: testbuild
  args: |
    --port ${PORT}
    --model /tmp/newmodel.gguf
`

// mountServer mounts the agent API with the given options and returns a
// live test server.
func mountServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, opts)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// postJSON sends an authenticated JSON POST and returns status + body.
func postJSON(t *testing.T, server *httptest.Server, path string, body any) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(string(encoded)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(payload)
}

// backupNameRe matches config.yaml.bak-20060102T150405Z.
var backupNameRe = regexp.MustCompile(`\.bak-\d{8}T\d{6}Z$`)

func TestApply_UnapprovedWritesNothing(t *testing.T) {
	env := newTestEnv(t, nil)
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	var resp applyResponse
	env.getOK(t, "POST", "/agent/v1/apply", map[string]any{"yaml": validStanza, "approved": false}, &resp)

	assert.Equal(t, "pending", resp.Status)
	assert.Equal(t, "newmodel", resp.Model)
	assert.False(t, resp.Approved)
	assert.False(t, resp.Written)
	assert.False(t, resp.ReloadRequired)

	// Nothing written: no content change, no backup, no temp files.
	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assertNoArtifacts(t, env.scratch)

	// approved omitted entirely behaves the same.
	var resp2 applyResponse
	env.getOK(t, "POST", "/agent/v1/apply", map[string]any{"yaml": validStanza}, &resp2)
	assert.Equal(t, "pending", resp2.Status)
	assert.False(t, resp2.Written)
}

func TestApply_ApprovedMergesAndBacksUp(t *testing.T) {
	env := newTestEnv(t, nil)
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	var resp applyResponse
	env.getOK(t, "POST", "/agent/v1/apply", map[string]any{"yaml": validStanza, "approved": true}, &resp)

	assert.Equal(t, "applied", resp.Status)
	assert.Equal(t, "newmodel", resp.Model)
	assert.True(t, resp.Written)
	assert.True(t, resp.ReloadRequired, "without --watch-config the caller must reload")
	assert.Regexp(t, backupNameRe, resp.Backup, "backup name should be timestamped")

	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	// The backup holds the exact original.
	backup, err := os.ReadFile(resp.Backup)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(backup))

	// The merged file keeps everything that was there...
	assert.Contains(t, string(after), "# a comment that must survive an apply")
	assert.Contains(t, string(after), "healthCheckTimeout: 30")
	assert.Contains(t, string(after), "existing:")
	assert.Contains(t, string(after), "testbuild:")

	// ...and gained the new stanza under models:.
	assert.Contains(t, string(after), "newmodel:")
	assert.Contains(t, string(after), "--model /tmp/newmodel.gguf")

	// The merged file loads through the real config pipeline, with both
	// the old and the new model present.
	merged, err := config.LoadConfig(env.cfgPath)
	require.NoError(t, err)
	require.Contains(t, merged.Models, "newmodel")
	require.Contains(t, merged.Models, "existing")
	assert.Equal(t, "testbuild", merged.Models["newmodel"].Engine)

	assertNoArtifacts(t, env.scratch)
}

func TestApply_ReloadRequiredFalseWhenWatching(t *testing.T) {
	env := newTestEnv(t, nil)
	server := mountServer(t, Options{
		Cfg:         env.cfg,
		ConfigPath:  func() string { return env.cfgPath },
		WatchConfig: func() bool { return true },
	})

	status, body := postJSON(t, server, "/agent/v1/apply", map[string]any{"yaml": validStanza, "approved": true})
	require.Equal(t, http.StatusOK, status, body)

	var out applyResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.True(t, out.Written)
	assert.False(t, out.ReloadRequired, "with --watch-config the reload is automatic")
	assert.Contains(t, out.Message, "watch-config")
}

func TestApply_InvalidStanzaWritesNothing(t *testing.T) {
	env := newTestEnv(t, nil)
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	invalid := `newmodel:
  engine: no-such-engine
  args: |
    --port ${PORT}
`
	status, body := env.do(t, "POST", "/agent/v1/apply", map[string]any{"yaml": invalid, "approved": true}, testKey)
	require.Equal(t, http.StatusUnprocessableEntity, status, body)

	var out validationResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.False(t, out.Valid)
	assert.NotEmpty(t, out.Errors)

	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assertNoArtifacts(t, env.scratch)
}

func TestApply_RejectsFullConfigAndMultiStanzas(t *testing.T) {
	env := newTestEnv(t, nil)

	for name, yamlText := range map[string]string{
		"full config": "models:\n  m1:\n    engine: testbuild\n    args: |\n      --port ${PORT}\n",
		"two models":  "a:\n  engine: testbuild\nb:\n  engine: testbuild\n",
		"not a map":   "- just\n- a\n- list\n",
		// YAML null key parses as "null": a non-empty model ID is required.
		"empty id": "~:\n  engine: testbuild\n",
	} {
		t.Run(name, func(t *testing.T) {
			status, body := env.do(t, "POST", "/agent/v1/apply", map[string]any{"yaml": yamlText, "approved": true}, testKey)
			assert.Equal(t, http.StatusBadRequest, status, body)
		})
	}

	// Nothing written by any of them.
	assertNoArtifacts(t, env.scratch)
}

func TestApply_DuplicateModelConflict(t *testing.T) {
	env := newTestEnv(t, nil)
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	duplicate := `existing:
  engine: testbuild
  args: |
    --port ${PORT}
    --model /tmp/sneaky.gguf
`
	status, body := env.do(t, "POST", "/agent/v1/apply", map[string]any{"yaml": duplicate, "approved": true}, testKey)
	require.Equal(t, http.StatusConflict, status, body)
	assert.Contains(t, body, "does not overwrite")

	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assertNoArtifacts(t, env.scratch)
}

func TestApply_MergedConfigInvalidWritesNothing(t *testing.T) {
	// A stanza that is valid on its own but would corrupt the merged
	// config: it reuses an alias already claimed by an existing model.
	env := newTestEnv(t, func(y string) string {
		return strings.Replace(y, "models:\n  existing:\n    engine: testbuild\n",
			"models:\n  existing:\n    engine: testbuild\n    aliases:\n      - shared-name\n", 1)
	})
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	stanza := `newmodel:
  engine: testbuild
  aliases:
    - shared-name
  args: |
    --port ${PORT}
    --model /tmp/new.gguf
`
	status, body := env.do(t, "POST", "/agent/v1/apply", map[string]any{"yaml": stanza, "approved": true}, testKey)
	require.Equal(t, http.StatusUnprocessableEntity, status, body)
	assert.Contains(t, body, "merged config would not load")

	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assertNoArtifacts(t, env.scratch)
}

func TestApply_WithoutConfigPath(t *testing.T) {
	env := newTestEnv(t, nil)
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	// A server running with -config-dir only: ConfigPath is empty and
	// apply must refuse rather than guess where to write.
	server := mountServer(t, Options{Cfg: env.cfg, ConfigPath: nil})
	status, body := postJSON(t, server, "/agent/v1/apply", map[string]any{"yaml": validStanza, "approved": true})
	assert.Equal(t, http.StatusConflict, status, body)
	assert.Contains(t, body, "-config")

	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

func TestApply_FailedWriteLeavesOriginalIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; run as a normal user")
	}

	env := newTestEnv(t, nil)
	before, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)

	// Read-only scratch dir: the backup write (and temp file creation)
	// must fail, and the original must survive untouched.
	require.NoError(t, os.Chmod(env.scratch, 0o500))
	t.Cleanup(func() { _ = os.Chmod(env.scratch, 0o755) })

	status, body := env.do(t, "POST", "/agent/v1/apply", map[string]any{"yaml": validStanza, "approved": true}, testKey)
	assert.Equal(t, http.StatusInternalServerError, status, body)

	after, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))

	// Restore permissions so the artifact scan (and Cleanup) can work.
	require.NoError(t, os.Chmod(env.scratch, 0o755))
	assertNoArtifacts(t, env.scratch)
}

func TestApply_ApplyIsLoggedWithoutKey(t *testing.T) {
	env := newTestEnv(t, nil)

	var resp applyResponse
	env.getOK(t, "POST", "/agent/v1/apply", map[string]any{"yaml": validStanza, "approved": true}, &resp)
	assert.Equal(t, "applied", resp.Status)

	logs := env.logBuf.String()
	assert.Contains(t, logs, "agent api: applied model newmodel")
	assert.NotContains(t, logs, testKey, "the agent key must never be logged")
}

// assertNoArtifacts fails when the config directory contains leftover
// temp files.
func assertNoArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".agent-apply-", "leftover temp file: %s", entry.Name())
	}
}
