package main

// End-to-end tests for `llama-swap engines` against mocked update
// servers: the check → update → rollback lifecycle, and the
// running-model refusal with and without --force. No real network.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newEnginesTestEnv writes a config whose `testbuild` engine updates
// from a mocked GitHub releases API serving the given artifact content,
// and returns the config path and engine binary path.
func newEnginesTestEnv(t *testing.T, dir, artifact string) (cfgPath, bin string) {
	t.Helper()
	sum := sha256.Sum256([]byte(artifact))
	digest := hex.EncodeToString(sum[:])

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/beellama/releases":
			fmt.Fprintf(w, `[{"tag_name":"v2.0.0","assets":[
				{"name":"llama-server-linux-amd64","browser_download_url":%q,"digest":"sha256:%s"}]}]`,
				"http://"+r.Host+"/download/llama-server-linux-amd64", digest)
		case "/download/llama-server-linux-amd64":
			fmt.Fprint(w, artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	bin = filepath.Join(dir, "llama-server")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho old\n"), 0o755))

	cfgPath = filepath.Join(dir, "config.yaml")
	yamlText := fmt.Sprintf(`engines:
  testbuild:
    label: "test build"
    cmd: %s --flash-attn
    update:
      source: github
      repo: acme/beellama
      asset_regex: llama-server-.*
      api_base: %s
      version: "v1.0.0"

models:
  saluki:
    engine: testbuild
    args: |
      --port ${PORT}
      --model /does/not/matter.gguf
`, bin, api.URL)
	require.NoError(t, os.WriteFile(cfgPath, []byte(yamlText), 0o600))
	return cfgPath, bin
}

// runCLI runs `llama-swap engines <args>` in-process, returning its
// exit code and combined output.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := runEngines(args, &out)
	return code, out.String()
}

func TestEnginesCLI_CheckUpdateRollbackLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfgPath, bin := newEnginesTestEnv(t, dir, "new build v2.0.0")

	// list: engine with its seeded version, nothing checked yet.
	code, out := runCLI(t, "-config", cfgPath, "list")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, `testbuild  "test build"`)
	assert.Contains(t, out, bin)
	assert.Contains(t, out, "installed: v1.0.0")

	// check: the mocked source has v2.0.0.
	code, out = runCLI(t, "-config", cfgPath, "check")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "testbuild: update available: v2.0.0")

	// list now shows the found update.
	code, out = runCLI(t, "-config", cfgPath, "list")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "update available: v2.0.0")

	// update: no server is running and the config has no agent_api, so
	// the CLI cannot verify no models are up — --force is required.
	code, out = runCLI(t, "-config", cfgPath, "update", "testbuild")
	require.Equal(t, 3, code, out)
	assert.Contains(t, out, "cannot verify")
	assert.Contains(t, out, "--force")

	// update --force: download, verify, atomic swap.
	code, out = runCLI(t, "-config", cfgPath, "update", "testbuild", "--force")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "testbuild updated to v2.0.0")
	assert.Contains(t, out, "previous binary kept for rollback")

	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "new build v2.0.0", string(got))
	prev, err := os.ReadFile(bin + ".prev")
	require.NoError(t, err, ".prev slot exists")
	assert.Equal(t, "#!/bin/sh\necho old\n", string(prev))

	// check again: up to date.
	code, out = runCLI(t, "-config", cfgPath, "check")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "testbuild: up to date (v2.0.0)")

	// rollback: the old build comes back, the newer one waits in .prev.
	code, out = runCLI(t, "-config", cfgPath, "rollback", "testbuild", "--force")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "testbuild restored from")
	got, err = os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	prevBytes, err := os.ReadFile(bin + ".prev")
	require.NoError(t, err)
	assert.Equal(t, "new build v2.0.0", string(prevBytes))

	// check: v2.0.0 is available again after the rollback.
	code, out = runCLI(t, "-config", cfgPath, "check")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "testbuild: update available: v2.0.0")
}

func TestEnginesCLI_ChecksumMismatchAborts(t *testing.T) {
	dir := t.TempDir()
	cfgPath, bin := newEnginesTestEnv(t, dir, "tampered bytes")

	// Lie in the config: pin a checksum that does not match.
	yamlText, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	pinned := sha256.Sum256([]byte("what we ordered"))
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte(strings.Replace(string(yamlText), "      version:", fmt.Sprintf("      sha256: %q\n      version:", hex.EncodeToString(pinned[:])), 1)), 0o600))

	code, out := runCLI(t, "-config", cfgPath, "update", "testbuild", "--force")
	require.Equal(t, 1, code, out)
	assert.Contains(t, out, "checksum mismatch")
	assert.Contains(t, out, "nothing was changed")

	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got), "the engine binary is untouched")
	_, err = os.Stat(bin + ".prev")
	assert.True(t, os.IsNotExist(err))
}

func TestEnginesCLI_RemoteRuntimeRefusalAndForce(t *testing.T) {
	dir := t.TempDir()
	cfgPath, bin := newEnginesTestEnv(t, dir, "new build v2.0.0")

	// A fake llama-swap agent API: the engine has a model up.
	running := []string{"saluki"}
	var stopped []string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/agent/v1/engines/testbuild/running":
			fmt.Fprintf(w, `{"engine":"testbuild","runningModels":%s}`, strings.Join(runningSlice(running), ","))
		case r.URL.Path == "/agent/v1/engines/testbuild/stop":
			stopped = append(stopped, running...)
			running = nil
			fmt.Fprint(w, `{"engine":"testbuild","stopped":["saluki"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	// Enable the agent API in the config so the CLI verifies through it.
	yamlText, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, append(yamlText,
		[]byte(fmt.Sprintf("\nagent_api:\n  enabled: true\n  key: test-key\n"))...), 0o600))

	// Refused: the server reports saluki running.
	code, out := runCLI(t, "-config", cfgPath, "-server", agent.URL, "update", "testbuild")
	require.Equal(t, 3, code, out)
	assert.Contains(t, out, "saluki")
	assert.Contains(t, out, "running")
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	assert.Empty(t, stopped, "nothing was stopped on a refusal")

	// --force: the model is stopped through the server, then swapped.
	code, out = runCLI(t, "-config", cfgPath, "-server", agent.URL, "update", "testbuild", "--force")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "stopped models (left stopped): saluki")
	got, err = os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "new build v2.0.0", string(got))
	assert.Equal(t, []string{"saluki"}, stopped, "the stop went through the agent API")
}

func runningSlice(running []string) []string {
	if len(running) == 0 {
		return []string{"[]"}
	}
	return []string{`["saluki"]`}
}
