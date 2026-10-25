package agentapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// twoPeerTestEnv is a testEnv whose config carries one healthy mock peer
// and one peer pointing at a port with nothing listening.
func twoPeerTestEnv(t *testing.T, healthyURL, deadURL string) *testEnv {
	t.Helper()
	return newTestEnv(t, func(yaml string) string {
		return yaml + "\npeers:\n" +
			"  healthy:\n    proxy: " + healthyURL + "\n    models:\n      - declared-model\n" +
			"  dead:\n    proxy: " + deadURL + "\n    models:\n      - dead-model\n"
	})
}

func getCatalogCache(t *testing.T, env *testEnv) (int, peerCatalogCacheResponse, string) {
	t.Helper()
	status, payload := env.do(t, http.MethodGet, "/agent/v1/config/peers.catalog", nil, testKey)
	var resp peerCatalogCacheResponse
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal([]byte(payload), &resp))
	}
	return status, resp, payload
}

func cachePeer(t *testing.T, resp peerCatalogCacheResponse, name string) peerCatalogCachePeerDTO {
	t.Helper()
	for _, peer := range resp.Peers {
		if peer.Name == name {
			return peer
		}
	}
	t.Fatalf("peer %q missing from the cached catalog response: %+v", name, resp.Peers)
	return peerCatalogCachePeerDTO{}
}

func TestProvidersCache_EmptyBeforeAnyCheck(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := twoPeerTestEnv(t, mock.server.URL, "http://127.0.0.1:1")

	status, resp, payload := getCatalogCache(t, env)
	require.Equal(t, http.StatusOK, status, "body: %s", payload)
	require.Len(t, resp.Peers, 2)

	// Every configured peer is listed, sorted, with no catalog yet: the
	// screen must render the peers before the first check.
	assert.Equal(t, "dead", resp.Peers[0].Name)
	assert.Equal(t, "healthy", resp.Peers[1].Name)
	for _, peer := range resp.Peers {
		assert.Empty(t, peer.Models)
		assert.Empty(t, peer.CheckedAt)
		// Until a check runs, the count is what the config declares.
		assert.Equal(t, 1, peer.ModelCount)
	}

	// The cached read never touches a provider.
	assert.Empty(t, mock.authSeen)
}

func TestProvidersCache_PostThenCachedRead(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := twoPeerTestEnv(t, mock.server.URL, "http://127.0.0.1:1")

	status, _, payload := postCatalog(t, env, map[string]any{"name": "healthy"})
	require.Equal(t, http.StatusOK, status, "body: %s", payload)

	status, resp, payload := getCatalogCache(t, env)
	require.Equal(t, http.StatusOK, status, "body: %s", payload)
	healthy := cachePeer(t, resp, "healthy")
	require.Len(t, healthy.Models, 3)
	assert.Equal(t, "deepseek-chat", healthy.Models[0].ID)
	assert.Equal(t, 3, healthy.ModelCount)
	assert.NotEmpty(t, healthy.CheckedAt)
	assert.True(t, strings.HasSuffix(healthy.CheckedAt, "Z"), "checkedAt is RFC3339 UTC: %s", healthy.CheckedAt)
	// A peer whose config carries no key macro has nothing to flag.
	assert.Nil(t, healthy.KeyMacroAvailable)

	// The other peer is untouched: the POST updated one entry only.
	dead := cachePeer(t, resp, "dead")
	assert.Empty(t, dead.Models)
	assert.Empty(t, dead.CheckedAt)

	// The cache file exists, is 0600, and holds model IDs only.
	path := filepath.Join(env.scratch, providersCacheFileName)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "deepseek-chat")
	assert.NotContains(t, string(data), "configured-test-key")
	assert.NotContains(t, string(data), "Authorization")
}

func TestProvidersCache_DeadPeerDoesNotDisturbHealthyEntry(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := twoPeerTestEnv(t, mock.server.URL, "http://127.0.0.1:1")

	require.Equal(t, http.StatusOK, func() int {
		status, _, _ := postCatalog(t, env, map[string]any{"name": "healthy"})
		return status
	}())
	_, before, _ := getCatalogCache(t, env)

	status, _, payload := postCatalog(t, env, map[string]any{"name": "dead"})
	assert.Equal(t, http.StatusBadGateway, status, "body: %s", payload)
	assert.Contains(t, payload, "provider catalog unavailable")

	_, after, _ := getCatalogCache(t, env)
	assert.Equal(t, cachePeer(t, before, "healthy"), cachePeer(t, after, "healthy"),
		"a failed probe must leave the healthy peer's entry exactly as it was")
	assert.Empty(t, cachePeer(t, after, "dead").Models)
}

func TestProvidersCache_KeyMacroNameOnlyAndResolvableFlag(t *testing.T) {
	const keyName = "LLAMA_SWAP_TEST_PROVIDER_KEY"
	t.Setenv(keyName, "macro-secret-value")
	mock := newMockCatalogServer(t, mockListing)
	env := newTestEnv(t, func(yaml string) string {
		return yaml + "\npeers:\n  macro:\n    proxy: " + mock.server.URL +
			"\n    apiKey: ${env." + keyName + "}\n    models:\n      - m\n"
	})

	_, resp, payload := getCatalogCache(t, env)
	macro := cachePeer(t, resp, "macro")
	assert.Equal(t, "${env."+keyName+"}", macro.KeyMacro)
	require.NotNil(t, macro.KeyMacroAvailable)
	assert.True(t, *macro.KeyMacroAvailable)

	// The resolved value never appears in the response.
	assert.NotContains(t, payload, "macro-secret-value")

	// Unsetting the variable after startup is exactly the silent-death
	// case the screen reports: the macro is named, flagged unavailable.
	require.NoError(t, os.Unsetenv(keyName))
	_, resp, payload = getCatalogCache(t, env)
	macro = cachePeer(t, resp, "macro")
	require.NotNil(t, macro.KeyMacroAvailable)
	assert.False(t, *macro.KeyMacroAvailable)
	assert.NotContains(t, payload, "macro-secret-value")
}

func TestProvidersCache_LiteralKeyIsNotNamed(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := peerTestEnv(t, mock.server.URL)

	_, resp, payload := getCatalogCache(t, env)
	peer := cachePeer(t, resp, "mock")
	assert.Empty(t, peer.KeyMacro, "a literal key has no macro NAME to show")
	assert.Nil(t, peer.KeyMacroAvailable)
	assert.NotContains(t, payload, "configured-test-key")
}

func TestProvidersCache_SurvivesRestart(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := twoPeerTestEnv(t, mock.server.URL, "http://127.0.0.1:1")
	status, _, payload := postCatalog(t, env, map[string]any{"name": "healthy"})
	require.Equal(t, http.StatusOK, status, "body: %s", payload)

	// A fresh cache object (what a restart builds) reads the file back.
	cache, err := loadCatalogCache(filepath.Join(env.scratch, providersCacheFileName))
	require.NoError(t, err)
	entry, ok := cache.snapshot()["healthy"]
	require.True(t, ok)
	require.Len(t, entry.Models, 3)
	assert.False(t, entry.CheckedAt.IsZero())
}

func TestProvidersCache_CorruptFileIsSurvivable(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := twoPeerTestEnv(t, mock.server.URL, "http://127.0.0.1:1")
	path := filepath.Join(env.scratch, providersCacheFileName)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	// A broken cache must not break the route: it reads as empty.
	status, resp, payload := getCatalogCache(t, env)
	require.Equal(t, http.StatusOK, status, "body: %s", payload)
	require.Len(t, resp.Peers, 2)
	assert.Empty(t, cachePeer(t, resp, "healthy").Models)

	// And a successful probe rewrites it in the right shape.
	status, _, payload = postCatalog(t, env, map[string]any{"name": "healthy"})
	require.Equal(t, http.StatusOK, status, "body: %s", payload)
	cache, err := loadCatalogCache(path)
	require.NoError(t, err)
	assert.Len(t, cache.snapshot()["healthy"].Models, 3)
}

func TestProvidersCache_CapsModelsPerProvider(t *testing.T) {
	cache := newCatalogCache()
	models := make([]peerCatalogModelDTO, 0, maxCatalogModelsPerPeer+50)
	for i := 0; i < maxCatalogModelsPerPeer+50; i++ {
		models = append(models, peerCatalogModelDTO{ID: fmt.Sprintf("model-%04d", i)})
	}
	cache.put("giant", cachedCatalog{Models: models})

	entry := cache.snapshot()["giant"]
	assert.Len(t, entry.Models, maxCatalogModelsPerPeer)
	assert.True(t, entry.Truncated)
	assert.Equal(t, maxCatalogModelsPerPeer+50, entry.TotalModels)
}

func TestProvidersCache_CapsPeerCountDroppingOldest(t *testing.T) {
	cache := newCatalogCache()
	for i := 0; i < maxCatalogPeers+5; i++ {
		cache.put(fmt.Sprintf("peer-%03d", i), cachedCatalog{
			CheckedAt: time.Unix(int64(i), 0).UTC(),
			Models:    []peerCatalogModelDTO{{ID: "m"}},
		})
	}
	snapshot := cache.snapshot()
	assert.Len(t, snapshot, maxCatalogPeers)
	// The five oldest are gone; the newest are kept.
	for i := 0; i < 5; i++ {
		_, ok := snapshot[fmt.Sprintf("peer-%03d", i)]
		assert.False(t, ok, "oldest peer %d should have been evicted", i)
	}
	_, ok := snapshot[fmt.Sprintf("peer-%03d", maxCatalogPeers+4)]
	assert.True(t, ok)
}

func TestProvidersCache_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, filepath.Join(dir, providersCacheFileName), DefaultProvidersCachePath(dir))

	file := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(file, []byte("models: {}\n"), 0o600))
	assert.Equal(t, filepath.Join(dir, providersCacheFileName), DefaultProvidersCachePath(file))

	assert.Equal(t, providersCacheFileName, DefaultProvidersCachePath(""))
}
