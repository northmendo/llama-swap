package agentapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/engineupdate"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// fakeEngineRuntime is a scriptable engineupdate.Runtime.
type fakeEngineRuntime struct {
	running []string
	stopped []string
}

func (f *fakeEngineRuntime) RunningModelsUsingEngine(engine string) ([]string, error) {
	return append([]string(nil), f.running...), nil
}

func (f *fakeEngineRuntime) StopModelsUsingEngine(engine string) ([]string, error) {
	f.stopped = append(f.stopped, f.running...)
	f.running = nil
	return f.stopped, nil
}

// engineUpdateEnv is one mounted agent API with an updatable engine.
type engineUpdateEnv struct {
	server   *httptest.Server
	cfgPath  string
	stateDir string
	bin      string
	rt       *fakeEngineRuntime
}

// newEngineUpdateEnv mounts the agent API on a config whose single
// engine updates from a mocked GitHub releases API.
func newEngineUpdateEnv(t *testing.T, releaseAssetContent string) *engineUpdateEnv {
	t.Helper()

	// The mocked releases API: one release, one asset, with digest.
	assetContent := releaseAssetContent
	sum := sha256.Sum256([]byte(assetContent))
	digest := hex.EncodeToString(sum[:])

	ghMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/beellama/releases":
			fmt.Fprintf(w, `[{"tag_name":"v2.0.0","assets":[
				{"name":"llama-server-linux-amd64","browser_download_url":%q,"digest":"sha256:%s"}]}]`,
				"http://"+r.Host+"/download/llama-server-linux-amd64", digest)
		case "/download/llama-server-linux-amd64":
			fmt.Fprint(w, assetContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghMock.Close)

	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho old\n"), 0o755))
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlText := fmt.Sprintf(`engines:
  testbuild:
    label: "test build"
    cmd: %s --flash-attn
    update:
      source: github
      repo: acme/beellama
      asset_regex: llama-server-.*
      api_base: %s

models:
  saluki:
    engine: testbuild
    args: |
      --port ${PORT}
      --model /does/not/matter.gguf

agent_api:
  enabled: true
  key: %s
`, bin, ghMock.URL, testKey)
	require.NoError(t, os.WriteFile(cfgPath, []byte(yamlText), 0o600))

	cfg, err := config.LoadConfig(cfgPath)
	require.NoError(t, err)

	env := &engineUpdateEnv{
		cfgPath:  cfgPath,
		stateDir: dir,
		bin:      bin,
		rt:       &fakeEngineRuntime{},
	}
	mux := http.NewServeMux()
	Register(mux, Options{
		Cfg:             cfg,
		ConfigPath:      func() string { return cfgPath },
		EngineStatePath: func() string { return engineupdate.DefaultStatePath(cfgPath) },
		Runtime:         env.rt,
		Logger:          logmon.NewWriter(nil),
	})
	env.server = httptest.NewServer(mux)
	t.Cleanup(env.server.Close)
	return env
}

// doEngineRequest performs an authenticated JSON request.
func (e *engineUpdateEnv) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()

	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := e.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	payload := make(map[string]any)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return resp.StatusCode, payload
}

func TestAgentAPI_EngineUpdatesGet(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")

	// No state yet: the engine shows up with no version.
	status, body := env.do(t, http.MethodGet, "/agent/v1/engines/updates", nil)
	require.Equal(t, http.StatusOK, status)

	engines, ok := body["engines"].([]any)
	require.True(t, ok, "engines key: %v", body)
	require.Len(t, engines, 1)
	entry := engines[0].(map[string]any)
	assert.Equal(t, "testbuild", entry["name"])
	assert.Equal(t, "test build", entry["label"])
	assert.Equal(t, true, entry["hasUpdateSource"])
	assert.Equal(t, "github", entry["source"])

	// After a check, the update is reported from state.
	status, _ = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{"approved": false})
	require.Equal(t, http.StatusOK, status)

	status, body = env.do(t, http.MethodGet, "/agent/v1/engines/updates", nil)
	require.Equal(t, http.StatusOK, status)
	entry = body["engines"].([]any)[0].(map[string]any)
	require.NotNil(t, entry["updateAvailable"], "check result visible: %v", entry)
	assert.Equal(t, "v2.0.0", entry["updateAvailable"].(map[string]any)["tag"])

	// Without a bearer token the route is locked like the rest.
	req, err := http.NewRequest(http.MethodGet, env.server.URL+"/agent/v1/engines/updates", nil)
	require.NoError(t, err)
	resp, err := env.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAgentAPI_EngineUpdate_UnapprovedIsCheckOnly(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")

	status, body := env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{"approved": false})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, false, body["approved"])
	assert.Equal(t, true, body["updateAvailable"])
	assert.Equal(t, "v2.0.0", body["tag"])

	// A body without the field at all is check-only too.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, false, body["approved"])

	// Nothing was downloaded or swapped.
	got, err := os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	_, err = os.Stat(env.bin + ".prev")
	assert.True(t, os.IsNotExist(err))
}

func TestAgentAPI_EngineUpdate_ApprovedSwaps(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")

	status, body := env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{"approved": true})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, true, body["approved"])
	assert.Equal(t, "v2.0.0", body["installedVersion"])

	got, err := os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got))
	_, err = os.Stat(env.bin + ".prev")
	require.NoError(t, err, "previous binary kept for rollback")

	// A second approved update finds nothing new: up to date.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{"approved": true})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, true, body["upToDate"])
}

func TestAgentAPI_EngineUpdate_RefusesRunningModelThenForceStops(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")
	env.rt.running = []string{"saluki"}

	status, body := env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{"approved": true})
	require.Equal(t, http.StatusConflict, status)
	errMessage := body["error"].(map[string]any)["message"].(string)
	assert.Contains(t, errMessage, "saluki")

	// Refused: nothing swapped, model still running.
	got, err := os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	assert.Equal(t, []string{"saluki"}, env.rt.running)

	// Approved + force: the model is stopped through the runtime, then
	// the swap happens.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update",
		map[string]any{"approved": true, "force": true})
	require.Equal(t, http.StatusOK, status)
	stopped, _ := body["stoppedModels"].([]any)
	require.Len(t, stopped, 1)
	assert.Equal(t, "saluki", stopped[0])
	assert.Empty(t, env.rt.running, "stopped models stay stopped")

	got, err = os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got))
}

func TestAgentAPI_EngineRunningAndStop(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")
	env.rt.running = []string{"saluki", "mudi"}

	status, body := env.do(t, http.MethodGet, "/agent/v1/engines/testbuild/running", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "testbuild", body["engine"])
	running, _ := body["runningModels"].([]any)
	require.Len(t, running, 2)

	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/stop", nil)
	require.Equal(t, http.StatusOK, status)
	stopped, _ := body["stopped"].([]any)
	require.Len(t, stopped, 2)
	assert.Empty(t, env.rt.running)

	// Unknown engines 404.
	status, _ = env.do(t, http.MethodGet, "/agent/v1/engines/nosuch/running", nil)
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = env.do(t, http.MethodPost, "/agent/v1/engines/nosuch/stop", nil)
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = env.do(t, http.MethodPost, "/agent/v1/engines/nosuch/update", map[string]any{"approved": true})
	assert.Equal(t, http.StatusNotFound, status)
}

func TestAgentAPI_EngineCheck(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")

	// Check-all (no engines named): every engine with a source is
	// queried and lands in the state file the updates view reads.
	status, body := env.do(t, http.MethodPost, "/agent/v1/engines/check", map[string]any{})
	require.Equal(t, http.StatusOK, status)
	results, ok := body["results"].([]any)
	require.True(t, ok, "results key: %v", body)
	require.Len(t, results, 1)
	row := results[0].(map[string]any)
	assert.Equal(t, "testbuild", row["engine"])
	assert.Equal(t, "v2.0.0", row["tag"])
	assert.Equal(t, true, row["updateAvailable"])

	status, body = env.do(t, http.MethodGet, "/agent/v1/engines/updates", nil)
	require.Equal(t, http.StatusOK, status)
	entry := body["engines"].([]any)[0].(map[string]any)
	require.NotNil(t, entry["updateAvailable"], "check result visible: %v", entry)

	// A named subset is the same report for those engines only.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/check",
		map[string]any{"engines": []string{"testbuild"}})
	require.Equal(t, http.StatusOK, status)
	require.Len(t, body["results"], 1)

	// Unknown engine names 404; nothing was checked.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/check",
		map[string]any{"engines": []string{"nosuch"}})
	require.Equal(t, http.StatusNotFound, status)

	// Check never swaps: the binary and the rollback slot are
	// untouched.
	got, err := os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	_, err = os.Stat(env.bin + ".prev")
	assert.True(t, os.IsNotExist(err))

	// Locked like the rest of the API.
	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/agent/v1/engines/check", strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := env.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAgentAPI_EngineRollback(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")

	// Unapproved is refused outright: no swap, no gate side effects.
	status, body := env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/rollback", map[string]any{})
	require.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body["error"].(map[string]any)["message"].(string), "approved")

	// Without a previous update there is no rollback slot; the
	// error says so and nothing changes.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/rollback", map[string]any{"approved": true})
	require.Equal(t, http.StatusInternalServerError, status)
	assert.Contains(t, body["error"].(map[string]any)["message"].(string), "no rollback slot")

	// Unknown engine 404.
	status, _ = env.do(t, http.MethodPost, "/agent/v1/engines/nosuch/rollback", map[string]any{"approved": true})
	assert.Equal(t, http.StatusNotFound, status)

	// After an update, rollback restores the old binary and is
	// itself reversible: the newer build becomes the new slot.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{"approved": true})
	require.Equal(t, http.StatusOK, status)

	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/rollback", map[string]any{"approved": true})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "testbuild", body["engine"])
	assert.Equal(t, true, body["approved"])
	assert.Contains(t, body["restoredFrom"], ".prev")

	got, err := os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))

	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/rollback", map[string]any{"approved": true})
	require.Equal(t, http.StatusOK, status)
	got, err = os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got), "rollback is reversible")
}

func TestAgentAPI_EngineRollbackRefusesRunningModelThenForceStops(t *testing.T) {
	env := newEngineUpdateEnv(t, "new build")

	status, _ := env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/update", map[string]any{"approved": true})
	require.Equal(t, http.StatusOK, status)

	env.rt.running = []string{"saluki"}
	status, body := env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/rollback", map[string]any{"approved": true})
	require.Equal(t, http.StatusConflict, status)
	assert.Contains(t, body["error"].(map[string]any)["message"].(string), "saluki")

	// Refused: binary unchanged, model still running.
	got, err := os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got))
	assert.Equal(t, []string{"saluki"}, env.rt.running)

	// Approved + force: model stopped through the runtime, then the
	// swap happens and the stopped model is reported.
	status, body = env.do(t, http.MethodPost, "/agent/v1/engines/testbuild/rollback",
		map[string]any{"approved": true, "force": true})
	require.Equal(t, http.StatusOK, status)
	stopped, _ := body["stoppedModels"].([]any)
	require.Len(t, stopped, 1)
	assert.Equal(t, "saluki", stopped[0])
	assert.Empty(t, env.rt.running, "stopped models stay stopped")

	got, err = os.ReadFile(env.bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
}
