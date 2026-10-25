package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// Auth-gating of the proxy API (config `apiKeys`) and the UI exemptions
// that keep the embedded web app loadable when keys are configured.
//
// The real uiFS is empty in plain `go test` builds (embed_notag.go), so
// these tests swap in a stand-in UI build and restore it afterwards.

func withAuthGateUIFS(t *testing.T) {
	t.Helper()
	orig := uiFS
	uiFS = http.FS(fstest.MapFS{
		"index.html":       {Data: []byte("<html>app</html>")},
		"assets/app.js":    {Data: []byte("app-bundle")},
		"site.webmanifest": {Data: []byte("{}")},
	})
	t.Cleanup(func() { uiFS = orig })
}

func newAuthGateServer(t *testing.T, keys []string) *Server {
	t.Helper()
	cfg := config.Config{
		Models:          map[string]config.ModelConfig{"llama3": {}},
		RequiredAPIKeys: keys,
	}
	return newTestServerWithConfig(cfg, newStubRouter([]string{"llama3"}, `{"ok":true}`), newStubRouter(nil, ""))
}

func doRequest(s *Server, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestServer_AuthGate_APIKeysSet(t *testing.T) {
	withAuthGateUIFS(t)
	s := newAuthGateServer(t, []string{"test-key"})

	t.Run("GET /ui/ serves the app shell without a key", func(t *testing.T) {
		w := doRequest(s, http.MethodGet, "/ui/", "", "")
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
		if w.Body.String() != "<html>app</html>" {
			t.Errorf("body = %q, want the app shell", w.Body.String())
		}
	})

	t.Run("GET /ui/ static asset serves without a key", func(t *testing.T) {
		w := doRequest(s, http.MethodGet, "/ui/assets/app.js", "", "")
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
		if w.Body.String() != "app-bundle" {
			t.Errorf("body = %q, want app-bundle", w.Body.String())
		}
	})

	t.Run("GET /ui/ manifest exemption still works", func(t *testing.T) {
		w := doRequest(s, http.MethodGet, "/ui/site.webmanifest", "", "")
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("GET /v1/models is 401 without a key and 200 with it", func(t *testing.T) {
		if w := doRequest(s, http.MethodGet, "/v1/models", "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("no key: status = %d, want 401", w.Code)
		}
		if w := doRequest(s, http.MethodGet, "/v1/models", "", "test-key"); w.Code != http.StatusOK {
			t.Errorf("valid key: status = %d, want 200", w.Code)
		}
	})

	t.Run("POST /v1/chat/completions is 401 without a key and routes with it", func(t *testing.T) {
		body := `{"model":"llama3"}`
		if w := doRequest(s, http.MethodPost, "/v1/chat/completions", body, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("no key: status = %d, want 401", w.Code)
		}
		w := doRequest(s, http.MethodPost, "/v1/chat/completions", body, "test-key")
		if w.Code != http.StatusOK {
			t.Errorf("valid key: status = %d, want 200", w.Code)
		}
		if w.Body.String() != `{"ok":true}` {
			t.Errorf("valid key: body = %q, want the routed stub response", w.Body.String())
		}
	})

	t.Run("GET /health stays public", func(t *testing.T) {
		if w := doRequest(s, http.MethodGet, "/health", "", ""); w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("operational endpoints stay gated", func(t *testing.T) {
		for _, path := range []string{"/logs", "/logs/stream", "/metrics", "/unload", "/api/profiles"} {
			if w := doRequest(s, http.MethodGet, path, "", ""); w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s: status = %d, want 401", path, w.Code)
			}
		}
	})
}

func TestServer_AuthGate_APIKeysEmpty_EverythingKeyless(t *testing.T) {
	withAuthGateUIFS(t)
	s := newAuthGateServer(t, nil)

	t.Run("GET /ui/ and static assets reachable keyless", func(t *testing.T) {
		if w := doRequest(s, http.MethodGet, "/ui/", "", ""); w.Code != http.StatusOK {
			t.Errorf("/ui/: status = %d, want 200", w.Code)
		}
		if w := doRequest(s, http.MethodGet, "/ui/assets/app.js", "", ""); w.Code != http.StatusOK {
			t.Errorf("/ui/assets/app.js: status = %d, want 200", w.Code)
		}
	})

	t.Run("GET /v1/models reachable keyless", func(t *testing.T) {
		if w := doRequest(s, http.MethodGet, "/v1/models", "", ""); w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("POST /v1/chat/completions routes keyless", func(t *testing.T) {
		w := doRequest(s, http.MethodPost, "/v1/chat/completions", `{"model":"llama3"}`, "")
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
		if w.Body.String() != `{"ok":true}` {
			t.Errorf("body = %q, want the routed stub response", w.Body.String())
		}
	})

	t.Run("operational endpoints reachable keyless", func(t *testing.T) {
		for _, path := range []string{"/health", "/logs", "/api/profiles"} {
			if w := doRequest(s, http.MethodGet, path, "", ""); w.Code != http.StatusOK {
				t.Errorf("GET %s: status = %d, want 200", path, w.Code)
			}
		}
	})
}
