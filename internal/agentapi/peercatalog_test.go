package agentapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockCatalogServer is a minimal OpenAI-compatible provider: it serves
// /v1/models, records the Authorization header it saw, and can be told
// to answer 401 instead (the bad-key case).
type mockCatalogServer struct {
	server     *httptest.Server
	authSeen   string
	unexpected int // requests outside /v1/models
	listing    string
	answer401  bool
}

func newMockCatalogServer(t *testing.T, listing string) *mockCatalogServer {
	t.Helper()
	m := &mockCatalogServer{listing: listing}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			m.unexpected++
			http.NotFound(w, r)
			return
		}
		m.authSeen = r.Header.Get("Authorization")
		if m.answer401 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.listing))
	}))
	t.Cleanup(m.server.Close)
	return m
}

const mockListing = `{
  "object": "list",
  "data": [
    {"id": "zephyr-7b", "object": "model", "owned_by": "vllm", "max_model_len": 8192},
    {"id": "deepseek-chat", "object": "model", "owned_by": "openrouter"},
    {"id": "deepseek-chat", "object": "model", "owned_by": "openrouter"},
    {"id": "", "object": "model"},
    {"id": "llama-3.1-8b", "object": "model", "owned_by": "meta", "context_length": 131072}
  ]
}`

// peerTestEnv is a testEnv whose config carries a peer pointing at mock.
func peerTestEnv(t *testing.T, mockURL string) *testEnv {
	t.Helper()
	return newTestEnv(t, func(yaml string) string {
		return yaml + "\npeers:\n  mock:\n    proxy: " + mockURL + "\n    apiKey: configured-test-key\n    models:\n      - remote-model\n"
	})
}

func postCatalog(t *testing.T, env *testEnv, body any) (int, peerCatalogResponse, string) {
	t.Helper()
	status, payload := env.do(t, http.MethodPost, "/agent/v1/config/peers.catalog", body, testKey)
	var resp peerCatalogResponse
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal([]byte(payload), &resp))
	}
	return status, resp, payload
}

func TestPeerCatalog_ConfiguredPeerReturnsLiveModels(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := peerTestEnv(t, mock.server.URL)

	status, resp, payload := postCatalog(t, env, map[string]any{"name": "mock"})
	require.Equal(t, http.StatusOK, status, "body: %s", payload)

	// The provider was authenticated with the configured key,
	// server-side: the UI never sees it.
	assert.Equal(t, "Bearer configured-test-key", mock.authSeen)

	assert.Equal(t, "peers.mock", resp.Source)
	require.Len(t, resp.Models, 3)
	// Sorted, deduped, blank IDs dropped, context tokens picked from
	// whichever field the provider used.
	assert.Equal(t, "deepseek-chat", resp.Models[0].ID)
	assert.Equal(t, "llama-3.1-8b", resp.Models[1].ID)
	assert.Equal(t, 131072, resp.Models[1].ContextLength)
	assert.Equal(t, "zephyr-7b", resp.Models[2].ID)
	assert.Equal(t, 8192, resp.Models[2].ContextLength)

	// The key never appears anywhere in the response.
	assert.NotContains(t, payload, "configured-test-key")
}

func TestPeerCatalog_AdhocProxyResolvesEnvMacroKey(t *testing.T) {
	t.Setenv("LLAMA_SWAP_TEST_PEER_KEY", "adhoc-test-key")
	mock := newMockCatalogServer(t, mockListing)
	env := newTestEnv(t, nil)

	status, resp, payload := postCatalog(t, env, map[string]any{
		"proxy":  mock.server.URL,
		"apiKey": "${env.LLAMA_SWAP_TEST_PEER_KEY}",
	})
	require.Equal(t, http.StatusOK, status, "body: %s", payload)

	assert.Equal(t, "proxy", resp.Source)
	require.Len(t, resp.Models, 3)
	assert.Equal(t, "Bearer adhoc-test-key", mock.authSeen)
	assert.NotContains(t, payload, "adhoc-test-key")
}

func TestPeerCatalog_AdhocProxyLiteralKeyRejected(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	env := newTestEnv(t, nil)

	status, _, payload := postCatalog(t, env, map[string]any{
		"proxy":  mock.server.URL,
		"apiKey": "sk-live-literal-secret",
	})
	assert.Equal(t, http.StatusBadRequest, status, "body: %s", payload)
	assert.Contains(t, payload, "must reference an environment variable macro")
	// The provider was never contacted with a literal secret.
	assert.Empty(t, mock.authSeen)
}

func TestPeerCatalog_AdhocProxyWithoutKeyStillQueries(t *testing.T) {
	// Public providers need no key; the query must not demand one.
	mock := newMockCatalogServer(t, mockListing)
	env := newTestEnv(t, nil)

	status, resp, _ := postCatalog(t, env, map[string]any{"proxy": mock.server.URL})
	require.Equal(t, http.StatusOK, status)
	require.Len(t, resp.Models, 3)
	assert.Empty(t, mock.authSeen)
}

func TestPeerCatalog_DeadPeerIsCleanError(t *testing.T) {
	// A port with nothing listening: the classic dead-peer case.
	env := newTestEnv(t, nil)

	status, _, payload := postCatalog(t, env, map[string]any{"proxy": "http://127.0.0.1:1"})
	assert.Equal(t, http.StatusBadGateway, status, "body: %s", payload)
	assert.Contains(t, payload, "provider catalog unavailable")
	assert.Contains(t, payload, "error") // the standard envelope, parseable by the UI
}

func TestPeerCatalog_BadKeyIsCleanError(t *testing.T) {
	mock := newMockCatalogServer(t, mockListing)
	mock.answer401 = true
	env := peerTestEnv(t, mock.server.URL)

	status, _, payload := postCatalog(t, env, map[string]any{"name": "mock"})
	assert.Equal(t, http.StatusBadGateway, status, "body: %s", payload)
	assert.Contains(t, payload, "provider catalog unavailable")
	assert.Contains(t, payload, "401")
	// The upstream's own error text is not leaked as the envelope; only
	// the transport status line is.
	assert.NotContains(t, payload, "bad key")
}

func TestPeerCatalog_EmptyCatalogIsAnEmptyList(t *testing.T) {
	mock := newMockCatalogServer(t, `{"object":"list","data":[]}`)
	env := peerTestEnv(t, mock.server.URL)

	status, resp, _ := postCatalog(t, env, map[string]any{"name": "mock"})
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, resp.Models)
}

func TestPeerCatalog_UnknownPeerIs404(t *testing.T) {
	env := newTestEnv(t, nil)
	status, _, payload := postCatalog(t, env, map[string]any{"name": "nope"})
	assert.Equal(t, http.StatusNotFound, status, "body: %s", payload)
	assert.Contains(t, payload, "peers.nope is not defined")
}

func TestPeerCatalog_NeitherNameNorProxyIs400(t *testing.T) {
	env := newTestEnv(t, nil)
	status, _, payload := postCatalog(t, env, map[string]any{})
	assert.Equal(t, http.StatusBadRequest, status, "body: %s", payload)
	assert.Contains(t, payload, `is required when`)
}

func TestPeerCatalog_NonHTTPSchemeIsRejectedCleanly(t *testing.T) {
	env := newTestEnv(t, nil)
	status, _, payload := postCatalog(t, env, map[string]any{"proxy": "tailcat://friend"})
	assert.Equal(t, http.StatusBadRequest, status, "body: %s", payload)
	assert.Contains(t, payload, "http or https proxy")
}

func TestPeerCatalog_BadProxyURLIs400(t *testing.T) {
	env := newTestEnv(t, nil)
	status, _, payload := postCatalog(t, env, map[string]any{"proxy": "::not a url::"})
	assert.Equal(t, http.StatusBadRequest, status, "body: %s", payload)
	assert.Contains(t, payload, "invalid proxy URL")
}

func TestPeerCatalog_ConfiguredPeerTakesPrecedenceOverExtraFields(t *testing.T) {
	// A stale form may still carry proxy/apiKey fields next to name;
	// for a configured peer the server-side values win, so a key the
	// file holds as a literal is used without ever being echoed.
	mock := newMockCatalogServer(t, mockListing)
	env := peerTestEnv(t, mock.server.URL)

	status, _, payload := postCatalog(t, env, map[string]any{
		"name":   "mock",
		"apiKey": "${env.SOME_OTHER_KEY}",
	})
	require.Equal(t, http.StatusOK, status, "body: %s", payload)
	assert.Equal(t, "Bearer configured-test-key", mock.authSeen)
	assert.NotContains(t, payload, "configured-test-key")
	assert.Equal(t, 0, mock.unexpected, "the query must hit /v1/models only")
}

func TestPeerCatalog_FollowsBasePathOfProxy(t *testing.T) {
	// Providers configured with a base path (https://host/api) must be
	// queried at /api/v1/models, like the peer proxy itself would.
	var hitPath string
	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		hitPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	t.Cleanup(base.Close)

	env := peerTestEnv(t, base.URL+"/api")
	status, resp, _ := postCatalog(t, env, map[string]any{"name": "mock"})
	require.Equal(t, http.StatusOK, status)
	require.Len(t, resp.Models, 1)
	assert.Equal(t, "/api/v1/models", hitPath)
}
