package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failoverUpstream is an httptest server standing in for a peer's upstream,
// counting the requests the failover selector sends to it.
type failoverUpstream struct {
	server *httptest.Server
	hits   atomic.Int32
}

// newFailoverUpstream starts a peer upstream running handler.
func newFailoverUpstream(t *testing.T, handler http.HandlerFunc) *failoverUpstream {
	t.Helper()
	u := &failoverUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(u.server.Close)
	return u
}

// failoverPeer couples a peer ID with its upstream; the slice order is the
// failover chain order.
type failoverPeer struct {
	id       string
	upstream *failoverUpstream
}

// failoverTestServer builds a Server whose config declares the failover
// selector `cascade` over the given peers in order, using a real
// router.NewPeer so peer timeout and connection-error paths are exercised.
func failoverTestServer(t *testing.T, selectorSettings string, peers []failoverPeer, local *stubRouter) *Server {
	t.Helper()

	targets := make([]string, 0, len(peers))
	var peersYAML strings.Builder
	for _, peer := range peers {
		targets = append(targets, peer.id+"/model-"+peer.id)
		fmt.Fprintf(&peersYAML, "  %s:\n    proxy: %s\n    models: [model-%s]\n    timeouts:\n      responseHeader: 2\n", peer.id, peer.upstream.server.URL, peer.id)
	}

	yaml := fmt.Sprintf(`
models:
  local:
    cmd: echo ${PORT}
peers:
%sselectors:
  cascade:
    strategy: failover
    targets: [%s]
%s
`, peersYAML.String(), strings.Join(targets, ", "), selectorSettings)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)

	if local == nil {
		local = newStubRouter([]string{"local"}, "")
	}
	logs := logmon.NewGroup(io.Discard, true, true, true)
	peerRouter, err := router.NewPeer(cfg, logs.ProxyLogs)
	require.NoError(t, err)

	s := newTestServerWithConfig(cfg, local, peerRouter)
	t.Cleanup(func() { s.store.Close() })
	return s
}

func jsonResponse(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func TestServer_SelectorFailover_HappyPathOrder(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"first"}`)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, int32(1), first.hits.Load(), "healthy primary should be used exactly once")
	assert.Equal(t, int32(0), second.hits.Load(), "healthy primary should not spill to the second target")
	assert.Contains(t, w.Body.String(), `"served_by":"first"`)
}

func TestServer_SelectorFailover_401FailsOver(t *testing.T) {
	testStatusFailover(t, http.StatusUnauthorized)
}

func TestServer_SelectorFailover_429FailsOver(t *testing.T) {
	testStatusFailover(t, http.StatusTooManyRequests)
}

func TestServer_SelectorFailover_500FailsOver(t *testing.T) {
	testStatusFailover(t, http.StatusInternalServerError)
}

func testStatusFailover(t *testing.T, status int) {
	t.Helper()
	var failureBody string
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, status, failureBody)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	failureBody = fmt.Sprintf(`{"error":{"message":"upstream says no (%d)"}}`, status)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, int32(1), first.hits.Load(), "failing primary should have been tried")
	assert.Equal(t, int32(1), second.hits.Load(), "second target should have served the request")
	assert.Contains(t, w.Body.String(), `"served_by":"second"`)
}

func TestServer_SelectorFailover_TimeoutFailsOver(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// Hang well past the peer transport's responseHeader timeout (2s
		// in the test config). The handler's own context is tied to the
		// CLIENT request, which stays alive for the whole failover walk,
		// so bound the hang to keep httptest.Server.Close() from waiting.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	w := httptest.NewRecorder()
	start := time.Now()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Less(t, time.Since(start), 10*time.Second, "failover should not wait for the full hang")

	assert.Equal(t, int32(1), first.hits.Load())
	assert.Equal(t, int32(1), second.hits.Load())
	assert.Contains(t, w.Body.String(), `"served_by":"second"`)
}

func TestServer_SelectorFailover_ConnectionRefusedFailsOver(t *testing.T) {
	// A server that is already closed: its port refuses connections, which
	// the peer proxy surfaces as a 502.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})

	yaml := fmt.Sprintf(`
models:
  local:
    cmd: echo ${PORT}
peers:
  dead:
    proxy: %s
    models: [model-dead]
    timeouts:
      responseHeader: 2
      connect: 2
  second:
    proxy: %s
    models: [model-second]
selectors:
  cascade:
    strategy: failover
    targets: [dead/model-dead, second/model-second]
`, deadURL, second.server.URL)
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	logs := logmon.NewGroup(io.Discard, true, true, true)
	peerRouter, err := router.NewPeer(cfg, logs.ProxyLogs)
	require.NoError(t, err)
	s := newTestServerWithConfig(cfg, newStubRouter([]string{"local"}, ""), peerRouter)
	t.Cleanup(func() { s.store.Close() })

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int32(1), second.hits.Load())
	assert.Contains(t, w.Body.String(), `"served_by":"second"`)
}

func TestServer_SelectorFailover_LocalLoadFailureFailsOver(t *testing.T) {
	// The local model "fails to load": the router answers with the same 500
	// error shape a real dispatch failure produces.
	local := newStubRouter([]string{"local"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusInternalServerError, `{"error":{"message":"failed to start model"}}`)
	}
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})

	yaml := fmt.Sprintf(`
models:
  local:
    cmd: echo ${PORT}
peers:
  second:
    proxy: %s
    models: [model-second]
selectors:
  cascade:
    strategy: failover
    targets: [local, second/model-second]
`, second.server.URL)
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	logs := logmon.NewGroup(io.Discard, true, true, true)
	peerRouter, err := router.NewPeer(cfg, logs.ProxyLogs)
	require.NoError(t, err)
	s := newTestServerWithConfig(cfg, local, peerRouter)
	t.Cleanup(func() { s.store.Close() })

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"served_by":"second"`)
}

func TestServer_SelectorFailover_AllFailSurfacesLastError(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusTooManyRequests, `{"error":{"message":"quota exhausted at second"}}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))

	// The LAST target's real error reaches the client, not a generic one.
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "quota exhausted at second")
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, int32(1), first.hits.Load())
	assert.Equal(t, int32(1), second.hits.Load())
}

func TestServer_SelectorFailover_2xxDoesNotFailover(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"first"}`)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int32(1), first.hits.Load())
	assert.Equal(t, int32(0), second.hits.Load())
}

func TestServer_SelectorFailover_MidStreamFailureDoesNotFailover(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"partial\":true}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Die mid-stream: once bytes were forwarded there is no failover.
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, _ := hijacker.Hijack()
			conn.Close()
			return
		}
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"partial":true`)
	assert.Equal(t, int32(0), second.hits.Load(), "no failover after the first response byte")
}

func TestServer_SelectorFailover_ClientDisconnectDoesNotFailover(t *testing.T) {
	arrived := make(chan struct{})
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		// Hold the request until the client hangs up. The transport may
		// leave this parked connection open after the roundtrip is
		// cancelled (it only force-closes idle connections), so bound the
		// wait to keep httptest.Server.Close() from blocking on it.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := chatRequest("cascade").WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(done)
	}()

	<-arrived
	cancel() // the client hangs up mid-attempt
	<-done

	assert.Equal(t, int32(1), first.hits.Load())
	assert.Equal(t, int32(0), second.hits.Load(), "client disconnect must not trigger failover")
}

func TestServer_SelectorFailover_CooldownSkipsThenRearms(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	// 500ms cooldown keeps the test quick but still observable.
	s := failoverTestServer(t, "    settings:\n      cooldown: 500ms\n", []failoverPeer{{"first", first}, {"second", second}}, nil)

	// First request: first fails, second serves; first enters cooldown.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, int32(1), first.hits.Load())

	// Second request inside the cooldown window: first is skipped.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int32(1), first.hits.Load(), "unhealthy target must be skipped during cooldown")
	assert.Equal(t, int32(2), second.hits.Load())

	// After the cooldown expires the primary is tried again.
	time.Sleep(600 * time.Millisecond)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int32(2), first.hits.Load(), "cooldown expiry must re-arm the primary")
	assert.Equal(t, int32(3), second.hits.Load())
}

func TestServer_SelectorFailover_AllTargetsUnhealthyStillTries(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"first"}`)
	})
	s := failoverTestServer(t, "    settings:\n      cooldown: 1h\n", []failoverPeer{{"first", first}}, nil)

	// Put the single target into cooldown through a real failure path.
	state := s.failovers.state("cascade")
	require.NotNil(t, state)
	state.markFailed("first/model-first", "HTTP 500", time.Now())

	// With every target in cooldown the eligible list falls back to the
	// full order rather than refusing to serve.
	require.Equal(t, []string{"first/model-first"}, state.eligibleTargets(time.Now()), "cooldown must not take the whole chain offline")

	// With every target in cooldown the chain still tries rather than
	// refusing to serve for the whole cooldown window.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"served_by":"first"`)
	assert.Equal(t, int32(1), first.hits.Load())
}

func TestServer_SelectorFailover_ReplaysRequestBodyToNextTarget(t *testing.T) {
	var firstBody, secondBody string
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		firstBody = string(body)
		jsonResponse(w, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		secondBody = string(body)
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cascade","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The selector rewrites the model to the peer FQN; the peer router then
	// rewrites the FQN to the bare peer model name before proxying.
	require.Contains(t, firstBody, `"model":"model-first"`, "attempt 1 should rewrite the model to the first target")
	require.Contains(t, firstBody, `"role":"user"`)
	require.Contains(t, secondBody, `"model":"model-second"`, "attempt 2 should rewrite the model to the second target")
	require.Contains(t, secondBody, `"role":"user"`, "the original body must be replayed to the next target")
}

func TestServer_SelectorFailover_ModelListingShowsHealth(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "    settings:\n      cooldown: 90s\n", []failoverPeer{{"first", first}, {"second", second}}, nil)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("cascade"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var response struct {
		Data []modelRecord `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	var cascade modelRecord
	found := false
	for _, record := range response.Data {
		if record.ID == "cascade" {
			cascade = record
			found = true
		}
	}
	require.True(t, found, "cascade record missing from model listing")
	require.NotNil(t, cascade.Meta, "cascade record missing metadata")

	metadata, ok := cascade.Meta["llamaswap"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, config.SelectorStrategyFailover, metadata["strategy"])
	assert.Equal(t, float64(90), metadata["cooldownSeconds"])

	health, ok := metadata["failover"].([]any)
	require.True(t, ok)
	require.Len(t, health, 2)

	firstHealth, ok := health[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "first/model-first", firstHealth["target"])
	assert.Equal(t, false, firstHealth["healthy"])
	assert.NotZero(t, firstHealth["retryAt"])
	assert.Contains(t, firstHealth["lastError"], "401")

	secondHealth, ok := health[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "second/model-second", secondHealth["target"])
	assert.Equal(t, true, secondHealth["healthy"])
}

func TestServer_SelectorFailover_WebsocketNoFailover(t *testing.T) {
	first := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)
	})
	second := newFailoverUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, `{"served_by":"second"}`)
	})
	s := failoverTestServer(t, "", []failoverPeer{{"first", first}, {"second", second}}, nil)

	req := httptest.NewRequest(http.MethodGet, "/props?model=cascade", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	// A websocket upgrade cannot be replayed, so the first target's answer
	// goes straight to the client without a failover attempt.
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, int32(1), first.hits.Load())
	assert.Equal(t, int32(0), second.hits.Load())
}
