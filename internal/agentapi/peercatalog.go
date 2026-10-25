// The live provider catalog: POST /agent/v1/config/peers.catalog fetches
// a provider's real /v1/models listing so the config UI can offer a
// searchable pick list of actual model IDs instead of hand-typed names.
//
// Two request shapes share one handler:
//
//   - {"name": "openrouter"} queries a CONFIGURED peer: the proxy URL,
//     the API key and the timeouts all come from the server-side config,
//     so a key the file holds as a literal never travels anywhere.
//   - {"proxy": "https://…", "apiKey": "${env.MY_KEY}"} queries a peer
//     that is still being drafted in the add/edit form. The key field
//     accepts an ${env.VAR} macro only — the same rule every write verb
//     enforces — and the macro is resolved server-side from the
//     environment, so a literal secret can never ride through this API.
//
// The fetch itself is the existing upstream discovery code,
// internal/capcompat: its Client already speaks /v1/models with the
// peer's timeouts and header injection (api key), and its
// ModelsResponse already parses the OpenAI listing shape. This handler
// adds no second fetcher — it points that one at a peer's proxy.
//
// Degradation is per provider by construction: one unreachable or
// badly-keyed peer answers this one request with a clean 502 envelope
// and never touches any other peer or panel.
package agentapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/capcompat"
	"github.com/mostlygeek/llama-swap/internal/config"
)

// catalogTimeout caps one live catalog query end to end, on top of the
// per-peer transport timeouts. A dead provider must fail the picker in
// seconds, not hang the dialog open for a minute.
const catalogTimeout = 15 * time.Second

// peerCatalogRequest is the body of POST /agent/v1/config/peers.catalog.
// Exactly one addressing mode is used per request: Name for a configured
// peer, Proxy (with an optional ApiKey macro) for an unsaved one.
type peerCatalogRequest struct {
	// Name addresses a peer from the live config; its proxy, key and
	// timeouts are read server-side and never echoed back.
	Name string `json:"name"`

	// Proxy is the base URL of an unsaved peer, used when Name is empty.
	Proxy *string `json:"proxy"`

	// ApiKey is an optional ${env.VAR} reference for the unsaved-peer
	// mode. Literals are rejected, exactly like the peer write verbs.
	ApiKey *string `json:"apiKey"`
}

// peerCatalogModelDTO is one entry of the pick list.
type peerCatalogModelDTO struct {
	ID      string `json:"id"`
	OwnedBy string `json:"ownedBy,omitempty"`
	// ContextLength is the usable context the provider reported, 0 when
	// it reports none. A hint in the picker, never a promise.
	ContextLength int `json:"contextLength,omitempty"`
}

// peerCatalogResponse is the wire shape of POST /agent/v1/config/peers.catalog.
type peerCatalogResponse struct {
	// Source says what was queried: "peers.<name>" or "proxy".
	Source string                `json:"source"`
	Models []peerCatalogModelDTO `json:"models"`
	// CheckedAt, LatencyMs and ModelCount mirror what the cache now
	// holds, so a caller that just probed can update its view without a
	// second (cached) read.
	CheckedAt  string `json:"checkedAt,omitempty"`
	LatencyMs  int64  `json:"latencyMs,omitempty"`
	ModelCount int    `json:"modelCount"`
}

// handlePeerCatalog is POST /agent/v1/config/peers.catalog.
func (a *api) handlePeerCatalog(w http.ResponseWriter, r *http.Request) {
	var req peerCatalogRequest
	if !decodeBody(w, r, &req) {
		return
	}

	var (
		base     *url.URL
		apiKey   string
		timeouts config.TimeoutsConfig
		source   string
	)

	name := strings.TrimSpace(req.Name)
	switch {
	case name != "":
		peer, found := a.cfg.Peers[name]
		if !found {
			writeError(w, r, http.StatusNotFound, fmt.Sprintf("peers.%s is not defined in the config", name))
			return
		}
		base, apiKey, timeouts = peer.ProxyURL, peer.ApiKey, peer.Timeouts
		source = "peers." + name

	default:
		if req.Proxy == nil || strings.TrimSpace(*req.Proxy) == "" {
			writeError(w, r, http.StatusBadRequest, `"proxy" is required when "name" is not set`)
			return
		}
		parsed, err := url.Parse(strings.TrimSpace(*req.Proxy))
		if err != nil {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid proxy URL (%s): %v", *req.Proxy, err))
			return
		}
		base, timeouts, source = parsed, adhocPeerTimeouts(), "proxy"
		if req.ApiKey != nil && strings.TrimSpace(*req.ApiKey) != "" {
			if err := requireEnvMacro("apiKey", strings.TrimSpace(*req.ApiKey)); err != nil {
				writeError(w, r, http.StatusBadRequest, err.Error())
				return
			}
			apiKey = resolveEnvMacro(strings.TrimSpace(*req.ApiKey))
		}
	}

	// Only plain HTTP(S) proxies expose a fetchable catalog; Tailcat
	// peers speak their own end-to-end protocol.
	if base == nil || (base.Scheme != "http" && base.Scheme != "https") {
		scheme := "<none>"
		if base != nil {
			scheme = base.Scheme
		}
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("the live catalog needs an http or https proxy, not %q; enter model IDs by hand instead", scheme))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), catalogTimeout)
	defer cancel()

	started := time.Now()

	client := capcompat.NewClient(base, timeouts)
	if apiKey != "" {
		// The same dual header the peer's own reverse proxy injects
		// (internal/router/peer.go), so providers that accept either
		// style authenticate the catalog query too.
		client.SetHeader("Authorization", "Bearer "+apiKey)
		client.SetHeader("x-api-key", apiKey)
	}

	var listing capcompat.ModelsResponse
	if err := client.GetJSON(ctx, "/v1/models", &listing); err != nil {
		// A provider being down, key-rejecting or slow is an expected
		// outcome for a live query: a clean 502 with the reason, not a
		// broken panel. The message carries no key material — only
		// the path, the status and the transport error.
		writeError(w, r, http.StatusBadGateway, "provider catalog unavailable: "+err.Error())
		return
	}

	models := make([]peerCatalogModelDTO, 0, len(listing.Data))
	seen := make(map[string]bool, len(listing.Data))
	for _, entry := range listing.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, peerCatalogModelDTO{
			ID:            id,
			OwnedBy:       strings.TrimSpace(entry.OwnedBy),
			ContextLength: entry.ContextTokens(),
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	// Record the success so the cached read (GET, providerscache.go) and
	// therefore the config pickers stay warm. Only this provider's entry
	// is touched, so one dead provider never disturbs another's catalog.
	latency := time.Since(started)
	a.storeCatalog(name, models, latency)

	writeJSON(w, r, http.StatusOK, peerCatalogResponse{
		Source:     source,
		Models:     models,
		CheckedAt:  time.Now().UTC().Format(time.RFC3339),
		LatencyMs:  latency.Milliseconds(),
		ModelCount: len(models),
	})
}

// adhocPeerTimeouts returns the transport timeouts an unsaved peer gets:
// the same defaults config.PeerConfig applies at YAML load time, so a
// catalog query behaves like the peer will once it is written.
func adhocPeerTimeouts() config.TimeoutsConfig {
	return config.TimeoutsConfig{
		Connect:        30,
		KeepAlive:      30,
		ResponseHeader: 60,
		TLSHandshake:   10,
		ExpectContinue: 1,
		IdleConn:       90,
	}
}

// resolveEnvMacro expands a whole-string ${env.VAR} reference using the
// process environment — the same substitution the config loader applies
// at startup. Empty when the variable is unset; the provider's own 401
// then explains the rest.
func resolveEnvMacro(ref string) string {
	m := envMacroRef.FindStringSubmatch(ref)
	if m == nil {
		return ""
	}
	return os.Getenv(m[1])
}
