package engineupdate

import (
	"context"
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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// fakeRuntime is a scriptable Runtime for refusal tests.
type fakeRuntime struct {
	running []string
	stopped []string
	listErr error
}

func (f *fakeRuntime) RunningModelsUsingEngine(engine string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]string(nil), f.running...), nil
}

func (f *fakeRuntime) StopModelsUsingEngine(engine string) ([]string, error) {
	f.stopped = append(f.stopped, f.running...)
	f.running = nil
	return f.stopped, nil
}

// writeFileExecutable writes a fake engine binary.
func writeFileExecutable(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newTestManager loads a config with one engine whose update block is
// updateYAML, with the engine binary at dir/llama-server and the state
// file in dir. It returns the manager and the binary path.
func newTestManager(t *testing.T, dir, updateYAML string, rt Runtime) (*Manager, string) {
	t.Helper()
	bin := filepath.Join(dir, "llama-server")
	writeFileExecutable(t, bin, "#!/bin/sh\necho old\n")

	yaml := fmt.Sprintf(`
engines:
  testengine:
    label: "test engine"
    cmd: %s --flash-attn
    update:
%s
`, bin, updateYAML)
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)

	return NewManager(cfg, filepath.Join(dir, "engine-updates.json"), rt), bin
}

// mockAsset is one asset of a mocked GitHub release.
type mockAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest,omitempty"`
}

type mockRelease struct {
	TagName    string      `json:"tag_name"`
	Prerelease bool        `json:"prerelease"`
	Assets     []mockAsset `json:"assets"`
}

// newGitHubMock serves a GitHub-style releases API and a download
// endpoint. releases is served newest-first as the real API does.
func newGitHubMock(t *testing.T, releases []mockRelease, artifacts map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/beellama/releases", func(w http.ResponseWriter, r *http.Request) {
		// The GitHub API returns absolute download URLs; rewrite the
		// relative artifact paths the tests declare.
		absolute := make([]mockRelease, len(releases))
		for i, rel := range releases {
			rel.Assets = append([]mockAsset(nil), rel.Assets...)
			for j := range rel.Assets {
				rel.Assets[j].URL = "http://" + r.Host + rel.Assets[j].URL
			}
			absolute[i] = rel
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(absolute))
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		content, ok := artifacts[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%s"`, name))
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(content))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func githubUpdateBlock(apiBase, assetRegex, tagRegex, sha string) string {
	block := fmt.Sprintf("      source: github\n      repo: acme/beellama\n      asset_regex: %q\n      api_base: %q\n", assetRegex, apiBase)
	if tagRegex != "" {
		block += fmt.Sprintf("      tag_regex: %q\n", tagRegex)
	}
	if sha != "" {
		block += fmt.Sprintf("      sha256: %q\n", sha)
	}
	return block
}

// --- GitHub source: latest-tag detection with tag/asset regexes ---

func TestEngineUpdate_GitHubLatestTagDetection(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v1.3.0-rc1", Prerelease: true, Assets: []mockAsset{
			{Name: "llama-server-v1.3.0-rc1-linux-amd64", URL: "/download/rc1", Digest: "sha256:" + sha256Hex("rc build")},
		}},
		{TagName: "v1.2.0", Assets: []mockAsset{
			{Name: "llama-server-v1.2.0-linux-amd64", URL: "/download/v120", Digest: "sha256:" + sha256Hex("v120")},
			{Name: "llama-server-v1.2.0-linux-arm64", URL: "/download/v120-arm"},
		}},
		{TagName: "v1.1.0", Assets: []mockAsset{
			{Name: "llama-server-v1.1.0-linux-amd64", URL: "/download/v110", Digest: "sha256:" + sha256Hex("v110")},
		}},
	}, map[string]string{
		"v120": "new binary v1.2.0",
		"v110": "old binary v1.1.0",
	})

	// No tag_regex: the pre-release is skipped, v1.2.0 is latest and the
	// amd64 asset is matched (not the arm64 one).
	m, bin := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-.*-linux-amd64`, "", ""), nil)
	res, err := m.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "v1.2.0", res[0].Tag)
	assert.True(t, res[0].Available, "nothing installed yet, so the tag is available")

	// tag_regex pins an older line: v1.1.0 is matched instead.
	m2, bin2 := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-.*-linux-amd64`, `v1\.1\..*`, ""), nil)
	_ = bin2
	res2, err := m2.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res2, 1)
	assert.Equal(t, "v1.1.0", res2[0].Tag)

	// tag_regex can opt into the pre-release.
	m3, bin3 := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-.*`, `rc`, ""), nil)
	_ = bin3
	res3, err := m3.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res3, 1)
	assert.Equal(t, "v1.3.0-rc1", res3[0].Tag)

	// asset_regex that matches nothing is a check error naming the engine.
	m4, _ := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `does-not-match-anything`, "", ""), nil)
	res4, err := m4.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res4, 1)
	require.Error(t, res4[0].Err)
	assert.Contains(t, res4[0].Err.Error(), "no release of acme/beellama matches")
	_ = bin
}

func TestEngineUpdate_GitHubAutoDigestAndPinnedFallback(t *testing.T) {
	ctx := context.Background()

	// A release whose asset exposes no digest: "auto" must refuse and
	// a pin must be honored.
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200"},
		}},
	}, map[string]string{"v200": "pinned bytes"})

	m, _ := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", ""), nil)
	_, err := m.Update(ctx, "testengine", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exposes no digest")

	pinned := sha256Hex("pinned bytes")
	m2, bin2 := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", pinned), nil)
	res, err := m2.Update(ctx, "testengine", true)
	require.NoError(t, err)
	assert.Equal(t, "v2.0.0", res.Version)
	got, err := os.ReadFile(bin2)
	require.NoError(t, err)
	assert.Equal(t, "pinned bytes", string(got))
}

// --- direct-URL source ---

func newURLMock(t *testing.T, etag, content string) (*httptest.Server, *string) {
	t.Helper()
	current := etag
	mux := http.NewServeMux()
	mux.HandleFunc("/dl/llama-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", current)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:00:00 GMT")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		fmt.Fprint(w, content)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &current
}

func urlUpdateBlock(base, sha, version string) string {
	block := fmt.Sprintf("      source: url\n      url: %s/dl/llama-server\n      sha256: %q\n", base, sha)
	if version != "" {
		block += fmt.Sprintf("      version: %q\n", version)
	}
	return block
}

func TestEngineUpdate_URLChangeDetectionAndIdempotentChecks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	content := "direct build 1"
	api, etag := newURLMock(t, `"direct-1"`, content)
	m, bin := newTestManager(t, dir, urlUpdateBlock(api.URL, sha256Hex(content), ""), nil)

	// First check: never installed through the updater.
	res, err := m.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.True(t, res[0].Available)
	assert.Contains(t, res[0].Note, "not installed through the updater yet")

	// Update installs it (force: no runtime to verify against).
	_, err = m.Update(ctx, "testengine", true)
	require.NoError(t, err)
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))

	// Re-check against the same ETag: up to date, idempotent.
	res, err = m.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.False(t, res[0].Available, "same ETag as the one recorded at install time")
	assert.Empty(t, res[0].Note)

	// The server ships a new build with a new ETag.
	*etag = `"direct-2"`
	res, err = m.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.True(t, res[0].Available, "changed ETag means a changed artifact")
}

func TestEngineUpdate_URLVersionSeed(t *testing.T) {
	ctx := context.Background()
	content := "direct build with a declared version"
	api, _ := newURLMock(t, `"v1"`, content)

	// The version field seeds the installed version before anything
	// was installed through the updater.
	m, _ := newTestManager(t, t.TempDir(), urlUpdateBlock(api.URL, sha256Hex(content), "b12"), nil)
	entries, err := m.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "b12", entries[0].InstalledVersion)

	res, err := m.Update(ctx, "testengine", true)
	require.NoError(t, err)
	assert.Equal(t, "b12", res.Version, "direct URLs have no tag; the declared version is recorded")
}

// --- checks against GitHub state ---

func TestEngineUpdate_CheckUpdatesStateAndSeeds(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200", Digest: "sha256:" + sha256Hex("v2")},
		}},
	}, map[string]string{"v200": "v2"})

	dir := t.TempDir()
	m, _ := newTestManager(t, dir, githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", "")+"      version: \"v1.0.0\"\n", nil)

	// Seeded version v1.0.0 vs latest v2.0.0: available.
	res, err := m.Check(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.True(t, res[0].Available)
	assert.Equal(t, "v2.0.0", res[0].Tag)

	// The check is recorded in the state file on disk.
	state, err := LoadState(filepath.Join(dir, "engine-updates.json"))
	require.NoError(t, err)
	st := state.Engines["testengine"]
	require.NotNil(t, st)
	assert.Equal(t, "v2.0.0", st.Available.Tag)
	assert.False(t, st.LastCheckAt.IsZero())
	assert.Equal(t, "github", st.Source)

	// `list` surfaces the availability without touching the network.
	entries, err := m.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "v1.0.0", entries[0].InstalledVersion)
	assert.Equal(t, "v2.0.0", entries[0].UpdateAvailable)
}

func TestEngineUpdate_CheckSkipsEnginesWithoutSource(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "llama-server")
	writeFileExecutable(t, bin, "#!/bin/sh\n")
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`
engines:
  manual:
    label: "updated by hand"
    cmd: %s
`, bin)))
	require.NoError(t, err)

	m := NewManager(cfg, filepath.Join(t.TempDir(), "engine-updates.json"), nil)
	res, err := m.Check(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, res, "engines without an update block are not checked")

	entries, err := m.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.False(t, entries[0].HasUpdateSource)
}

// --- update: checksum verification and the atomic swap ---

func TestEngineUpdate_ChecksumMismatchAbortsUntouched(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v9.9.9", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/evil", Digest: "sha256:" + sha256Hex("the bytes we expect")},
		}},
	}, map[string]string{"evil": "tampered bytes"})

	dir := t.TempDir()
	m, bin := newTestManager(t, dir, githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", ""), nil)

	_, err := m.Update(ctx, "testengine", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
	assert.Contains(t, err.Error(), "nothing was changed")

	// The binary is untouched, no rollback slot, no temp leftovers.
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	_, err = os.Stat(bin + prevSuffix)
	assert.True(t, os.IsNotExist(err))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".llama-swap-engine-"),
			"temp download file %s leaked", e.Name())
	}
}

func TestEngineUpdate_AtomicSwapCreatesPrevSlot(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200", Digest: "sha256:" + sha256Hex("new build")},
		}},
	}, map[string]string{"v200": "new build"})

	dir := t.TempDir()
	m, bin := newTestManager(t, dir, githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", ""), nil)

	res, err := m.Update(ctx, "testengine", true)
	require.NoError(t, err)
	assert.Equal(t, "v2.0.0", res.Version)
	assert.Equal(t, "llama-server-linux-amd64", res.AssetName)

	// New bytes in place, executable.
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got))
	info, err := os.Stat(bin)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	// The previous build is kept next to it.
	prev, err := os.ReadFile(bin + prevSuffix)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(prev))

	// State records the install.
	state, err := LoadState(filepath.Join(dir, "engine-updates.json"))
	require.NoError(t, err)
	st := state.Engines["testengine"]
	assert.Equal(t, "v2.0.0", st.InstalledVersion)
	assert.False(t, st.LastUpdateAt.IsZero())
	assert.Nil(t, st.Available)

	// A second update replaces the prev slot with the newer binary
	// instead of accumulating timestamped copies.
	api2 := newGitHubMock(t, []mockRelease{
		{TagName: "v2.1.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v210", Digest: "sha256:" + sha256Hex("v2.1.0 build")},
		}},
	}, map[string]string{"v210": "v2.1.0 build"})
	m.Engines["testengine"].Update.APIBase = api2.URL
	_, err = m.Update(ctx, "testengine", true)
	require.NoError(t, err)
	prev2, err := os.ReadFile(bin + prevSuffix)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(prev2), "the single prev slot now holds the previous binary")
	got2, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "v2.1.0 build", string(got2))
}

// --- running-model refusal ---

func TestEngineUpdate_RefusesWhenModelRunning(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200", Digest: "sha256:" + sha256Hex("new build")},
		}},
	}, map[string]string{"v200": "new build"})

	m, bin := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", ""),
		&fakeRuntime{running: []string{"saluki", "mudi"}})

	_, err := m.Update(ctx, "testengine", false)
	require.Error(t, err)
	require.True(t, IsRefusal(err), "the refusal must be recognizable, got %v", err)
	assert.Contains(t, err.Error(), "mudi, saluki")

	// Nothing was swapped and the models are still "running".
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	_, statErr := os.Stat(bin + prevSuffix)
	assert.True(t, os.IsNotExist(statErr))
}

func TestEngineUpdate_ForceStopsModelsThenSwaps(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200", Digest: "sha256:" + sha256Hex("new build")},
		}},
	}, map[string]string{"v200": "new build"})

	rt := &fakeRuntime{running: []string{"saluki"}}
	m, bin := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", ""), rt)

	res, err := m.Update(ctx, "testengine", true)
	require.NoError(t, err)
	assert.Equal(t, []string{"saluki"}, res.StoppedModels)
	assert.Equal(t, []string{"saluki"}, rt.stopped, "models were stopped through the runtime")
	assert.Empty(t, rt.running, "the stopped models are left stopped")

	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got))
}

func TestEngineUpdate_UnverifiableRuntimeRequiresForce(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200", Digest: "sha256:" + sha256Hex("new build")},
		}},
	}, map[string]string{"v200": "new build"})
	block := githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", "")

	// No runtime at all (agent API not enabled): refuse without --force.
	m, bin := newTestManager(t, t.TempDir(), block, nil)
	_, err := m.Update(ctx, "testengine", false)
	require.Error(t, err)
	require.True(t, IsRefusal(err))
	assert.Contains(t, err.Error(), "cannot verify")

	// An unreachable runtime (server down) behaves the same.
	m2, _ := newTestManager(t, t.TempDir(), block, &fakeRuntime{listErr: context.DeadlineExceeded})
	_, err = m2.Update(ctx, "testengine", false)
	require.Error(t, err)
	require.True(t, IsRefusal(err))
	assert.Contains(t, err.Error(), "cannot verify")

	// --force proceeds.
	_, err = m.Update(ctx, "testengine", true)
	require.NoError(t, err)
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got))
}

// --- rollback ---

func TestEngineUpdate_RollbackRestoresAndIsReversible(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200", Digest: "sha256:" + sha256Hex("new build")},
		}},
	}, map[string]string{"v200": "new build"})

	dir := t.TempDir()
	m, bin := newTestManager(t, dir, githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", ""), nil)

	// Nothing to roll back before an update.
	_, err := m.Rollback("testengine", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no rollback slot")

	_, err = m.Update(ctx, "testengine", true)
	require.NoError(t, err)

	// Rollback restores the old bytes and keeps the newer build in the
	// slot, so rolling forward again is possible.
	res, err := m.Rollback("testengine", true)
	require.NoError(t, err)
	assert.Equal(t, bin+prevSuffix, res.RestoredFrom)
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho old\n", string(got))
	prevBytes, err := os.ReadFile(bin + prevSuffix)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(prevBytes))

	// The restored version is unknown until the next check.
	state, err := LoadState(filepath.Join(dir, "engine-updates.json"))
	require.NoError(t, err)
	assert.Equal(t, "", state.Engines["testengine"].InstalledVersion)

	// Rollback is itself reversible: roll "forward" again.
	res2, err := m.Rollback("testengine", true)
	require.NoError(t, err)
	got2, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "new build", string(got2))
	assert.Equal(t, bin+prevSuffix, res2.RestoredFrom)
}

func TestEngineUpdate_RollbackRefusesWhenModelRunning(t *testing.T) {
	ctx := context.Background()
	api := newGitHubMock(t, []mockRelease{
		{TagName: "v2.0.0", Assets: []mockAsset{
			{Name: "llama-server-linux-amd64", URL: "/download/v200", Digest: "sha256:" + sha256Hex("new build")},
		}},
	}, map[string]string{"v200": "new build"})

	rt := &fakeRuntime{running: []string{"saluki"}}
	m, bin := newTestManager(t, t.TempDir(), githubUpdateBlock(api.URL, `llama-server-linux-amd64`, "", ""), rt)

	_, err := m.Update(ctx, "testengine", true)
	require.NoError(t, err)
	newBytes, err := os.ReadFile(bin)
	require.NoError(t, err)

	// The model using the engine is up again when the bad build ships.
	rt.running = []string{"saluki"}
	_, err = m.Rollback("testengine", false)
	require.Error(t, err)
	require.True(t, IsRefusal(err))
	got, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, string(newBytes), string(got), "refused rollback changed nothing")
}

// --- misc ---

func TestEngineUpdate_UnknownEngineAndMissingSource(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, t.TempDir(), githubUpdateBlock("http://127.0.0.1:1", `x`, "", ""), nil)

	_, err := m.Update(ctx, "nosuchengine", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown engine "nosuchengine"`)
}

func TestEngineUpdate_TargetBinaryResolution(t *testing.T) {
	dir := t.TempDir()
	realBin := filepath.Join(dir, "real-llama-server")
	writeFileExecutable(t, realBin, "#!/bin/sh\necho real\n")
	link := filepath.Join(dir, "llama-server-link")
	require.NoError(t, os.Symlink(realBin, link))

	// A bare command name resolves through PATH like a shell would.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeFileExecutable(t, filepath.Join(dir, "llama-server"), "#!/bin/sh\necho path\n")

	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`
engines:
  linked:
    label: "via symlink"
    cmd: %s --flag
  inpath:
    label: "bare name"
    cmd: llama-server
`, link)))
	require.NoError(t, err)

	// A symlinked cmd resolves to the real file, so an update swaps the
	// real binary rather than replacing the symlink.
	target, err := targetBinaryPath(cfg.Engines["linked"])
	require.NoError(t, err)
	assert.Equal(t, realBin, target)

	target2, err := targetBinaryPath(cfg.Engines["inpath"])
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "llama-server"), target2)
}

func TestEngineUpdate_StateRoundTripAndDefaultPath(t *testing.T) {
	dir := t.TempDir()

	// Missing file loads as empty state.
	path := DefaultStatePath(filepath.Join(dir, "config.yaml"))
	assert.Equal(t, filepath.Join(dir, StateFileName), path)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755))
	assert.Equal(t, filepath.Join(dir, "conf.d", StateFileName), DefaultStatePath(filepath.Join(dir, "conf.d")))

	state, err := LoadState(path)
	require.NoError(t, err)
	require.Empty(t, state.Engines)

	now := time.Date(2026, 10, 21, 7, 30, 0, 0, time.UTC)
	st := state.engine("testengine")
	st.InstalledVersion = "v2.0.0"
	st.Source = "github"
	st.LastCheckAt = now
	st.LastUpdateAt = now
	st.Available = &UpdateAvailable{Tag: "v3.0.0", CheckedAt: now}
	require.NoError(t, state.Save(path))

	loaded, err := LoadState(path)
	require.NoError(t, err)
	got := loaded.Engines["testengine"]
	require.NotNil(t, got)
	assert.Equal(t, "v2.0.0", got.InstalledVersion)
	assert.Equal(t, "v3.0.0", got.Available.Tag)
	assert.True(t, got.LastUpdateAt.Equal(now))

	// A corrupt state file is an error, never a silent reset.
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err = LoadState(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "corrupt")
}
