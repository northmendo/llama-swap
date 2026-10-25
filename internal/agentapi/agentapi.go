// Package agentapi implements the /agent/v1/ REST surface: a
// bearer-authenticated JSON API that exposes llama-swap's discovery and
// config machinery to automation (LLM agents, home-assistant integrations,
// other people's setups) so model onboarding does not require
// hand-editing YAML.
//
// The API is disabled unless the config carries an enabled `agent_api:`
// section (internal/config/agentapi.go); when disabled, Register mounts
// nothing and every /agent/v1 path 404s. Every mounted route requires
// `Authorization: Bearer <agent_api.key>`; the key is compared in
// constant time and never appears in any response, error or log line.
//
// Routes (JSON in/out, schema documented in docs/kb/guides/api-integration/agent-api.md):
//
//	POST /agent/v1/scan      run discovery over the configured folders
//	GET  /agent/v1/engines   list defined engines (env keys only, never values)
//	GET  /agent/v1/engines/updates     engine update state (check results)
//	POST /agent/v1/engines/check       refresh sources (all, or the named subset)
//	GET  /agent/v1/engines/{name}/running   models up on one engine
//	POST /agent/v1/engines/{name}/stop      stop those models
//	POST /agent/v1/engines/{name}/update    check; swap only when approved
//	POST /agent/v1/engines/{name}/rollback restore the previous binary,
//	                                          only when approved
//	POST /agent/v1/draft      draft a model stanza for one model file
//	POST /agent/v1/refine     draft + refine a stanza (LLM-assisted when
//	                          the draft_assistant section is enabled)
//	POST /agent/v1/validate   validate a model stanza or full config, no side effects
//	POST /agent/v1/apply      merge an approved model stanza into the config file
//	GET  /agent/v1/config              live config as safe JSON (keys as macros
//	                                      or set/unset, never resolved values)
//	POST /agent/v1/config/peers.add     add a peer (approved-gated dry run
//	POST /agent/v1/config/peers.update   and write, full validation, backup,
//	POST /agent/v1/config/peers.remove   atomic replace — see configverbs.go)
//	POST /agent/v1/config/peers.catalog   live /v1/models pick list for one
//	                                      peer (by name or draft proxy+key
//	                                      macro) — see peercatalog.go
//	GET  /agent/v1/config/peers.catalog   every peer's cached catalog, no
//	                                      upstream calls — see providerscache.go
//	GET  /agent/v1/config/schema          the finite value sets the config
//	                                      pickers are fed from
//	GET  /agent/v1/config/env             env var NAMES (never values) a
//	                                      peer key macro can reference
//	POST /agent/v1/config/selectors.add    same contract for cascades
//	POST /agent/v1/config/selectors.update
//	POST /agent/v1/config/selectors.remove
//	POST /agent/v1/config/agent_api.update enable/disable; key as ${env.VAR} only
//	GET  /agent/v1/config/backups  list the timestamped config backups
//	POST /agent/v1/config/restore  restore one backup (approval-gated)
package agentapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
	"github.com/mostlygeek/llama-swap/internal/draftassistant"
	"github.com/mostlygeek/llama-swap/internal/engineupdate"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// maxRequestBytes bounds every request body. The API exchanges small
// documents (a stanza or a file path); a megabyte is already generous.
const maxRequestBytes = 1 << 20

// Options carries what the server knows at mount time. Cfg must contain
// the AgentAPI section. ConfigPath and WatchConfig are functions (called
// per request, nil-safe) because the server learns its -config file and
// --watch-config flag after its routes are built: main calls
// SetConfigSource once the Server exists, and the values are read at
// request time. A ConfigPath returning empty (llama-swap runs with
// -config-dir only) makes apply refuse with 409. Logger receives apply
// lifecycle events and is nil-safe.
type Options struct {
	Cfg config.Config

	ConfigPath  func() string
	WatchConfig func() bool
	Logger      *logmon.Monitor

	// EngineStatePath returns the engine updater's JSON state file,
	// read per request like ConfigPath. Empty means the engine update
	// routes that need state answer 503 until main wires one in.
	EngineStatePath func() string

	// ProvidersCachePath returns the provider catalog cache file
	// (providers-cache.json beside the config). Read per request like
	// ConfigPath; empty falls back to deriving it from ConfigPath.
	ProvidersCachePath func() string

	// Runtime is the running-models view the update routes use to
	// refuse swaps under running models and to stop them on force.
	// Nil disables it: checks and state still work, but an approved
	// update cannot verify models and always requires force.
	Runtime engineupdate.Runtime
}

// api is the mounted handler set. It is immutable after construction,
// except for the apply mutex and the lazily loaded catalog cache.
type api struct {
	opts Options
	cfg  config.Config

	// applyMu serializes apply calls so two concurrent requests cannot
	// interleave read-modify-write cycles on the config file.
	applyMu sync.Mutex

	// catalogMu guards the provider catalog cache, which is loaded from
	// disk on first use (providerscache.go).
	catalogMu     sync.Mutex
	catalog       *catalogCache
	catalogLoaded bool
}

// Register mounts the agent API routes on mux when the config enables
// the `agent_api:` section. When the section is absent or disabled it
// mounts nothing: /agent/v1/... then falls through to the mux's own 404.
func Register(mux *http.ServeMux, opts Options) {
	section := opts.Cfg.AgentAPI
	if section == nil || !section.Enabled {
		return
	}

	a := &api{
		opts: opts,
		cfg:  opts.Cfg,
	}

	inner := http.NewServeMux()
	inner.HandleFunc("POST /agent/v1/scan", a.handleScan)
	inner.HandleFunc("GET /agent/v1/engines", a.handleEngines)
	inner.HandleFunc("GET /agent/v1/engines/updates", a.handleEngineUpdates)
	inner.HandleFunc("POST /agent/v1/engines/check", a.handleEngineCheck)
	inner.HandleFunc("GET /agent/v1/engines/{name}/running", a.handleEngineRunning)
	inner.HandleFunc("POST /agent/v1/engines/{name}/stop", a.handleEngineStop)
	inner.HandleFunc("POST /agent/v1/engines/{name}/update", a.handleEngineUpdate)
	inner.HandleFunc("POST /agent/v1/engines/{name}/rollback", a.handleEngineRollback)
	inner.HandleFunc("POST /agent/v1/draft", a.handleDraft)
	inner.HandleFunc("POST /agent/v1/refine", a.handleRefine)
	inner.HandleFunc("POST /agent/v1/validate", a.handleValidate)
	inner.HandleFunc("POST /agent/v1/apply", a.handleApply)
	inner.HandleFunc("GET /agent/v1/config", a.handleConfigState)
	inner.HandleFunc("POST /agent/v1/config/peers.add", a.handlePeerAdd)
	inner.HandleFunc("POST /agent/v1/config/peers.update", a.handlePeerUpdate)
	inner.HandleFunc("POST /agent/v1/config/peers.remove", a.handlePeerRemove)
	inner.HandleFunc("POST /agent/v1/config/peers.catalog", a.handlePeerCatalog)
	inner.HandleFunc("GET /agent/v1/config/peers.catalog", a.handlePeerCatalogCache)
	inner.HandleFunc("GET /agent/v1/config/schema", a.handleConfigSchema)
	inner.HandleFunc("GET /agent/v1/config/env", a.handleEnvNames)
	inner.HandleFunc("POST /agent/v1/config/selectors.add", a.handleSelectorAdd)
	inner.HandleFunc("POST /agent/v1/config/selectors.update", a.handleSelectorUpdate)
	inner.HandleFunc("POST /agent/v1/config/selectors.remove", a.handleSelectorRemove)
	inner.HandleFunc("POST /agent/v1/config/agent_api.update", a.handleAgentAPIUpdate)
	inner.HandleFunc("GET /agent/v1/config/backups", a.handleConfigBackups)
	inner.HandleFunc("POST /agent/v1/config/restore", a.handleConfigRestore)
	// Unknown /agent/v1 paths get a JSON 404 from this API rather than
	// the outer mux's bare-text one, so agents can parse every response.
	inner.HandleFunc("/agent/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "unknown agent API route")
	})

	mux.Handle("/agent/v1/", a.bearerAuth(section.Key, inner))
}

// bearerAuth wraps next so every request must carry the exact bearer
// token. The comparison runs over SHA-256 digests with
// subtle.ConstantTimeCompare, so neither the key's content nor its
// length is observable through timing. The configured key never appears
// in any response or log; 401 bodies are identical for missing,
// malformed and wrong tokens.
func (a *api) bearerAuth(key string, next http.Handler) http.Handler {
	keyHash := sha256.Sum256([]byte(key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, keyHash) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="llama-swap-agent-api"`)
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, r, http.StatusUnauthorized, "unauthorized: missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized reports whether the request presents the expected bearer
// token. Both sides are hashed first so ConstantTimeCompare sees two
// fixed-length digests.
func authorized(r *http.Request, keyHash [sha256.Size]byte) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
		return false
	}
	provided := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(provided[:], keyHash[:]) == 1
}

// configPath returns the config file apply merges into, "" when unset.
func (a *api) configPath() string {
	if a.opts.ConfigPath == nil {
		return ""
	}
	return a.opts.ConfigPath()
}

// watchConfig reports whether --watch-config is on.
func (a *api) watchConfig() bool {
	if a.opts.WatchConfig == nil {
		return false
	}
	return a.opts.WatchConfig()
}

// errorEnvelope is the error shape for every /agent/v1 response.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	writeJSON(w, r, status, errorEnvelope{Error: errorBody{Message: message}})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("agent api: encoding response: %v", err)
	}
}

// decodeBody reads a JSON request body into dst, enforcing the size cap.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// draftDTO is the wire shape of a discovery.Draft. discovery.Draft has no
// json tags; this gives the API stable lowerCamel field names.
type draftDTO struct {
	ModelID      string `json:"modelId"`
	Engine       string `json:"engine"`
	EngineSource string `json:"engineSource,omitempty"`
	Args         string `json:"args"`
	CtxSize      int64  `json:"ctxSize"`
	CtxCapped    bool   `json:"ctxCapped"`
	YAML         string `json:"yaml"`
}

func draftToDTO(d discovery.Draft) draftDTO {
	return draftDTO{
		ModelID:      d.ModelID,
		Engine:       d.Engine,
		EngineSource: d.EngineSource,
		Args:         d.Args,
		CtxSize:      d.CtxSize,
		CtxCapped:    d.CtxCapped,
		YAML:         d.YAML,
	}
}

// fileResultDTO is the per-file wire shape of a scan result.
type fileResultDTO struct {
	Path          string              `json:"path"`
	Status        string              `json:"status"`
	Metadata      *discovery.Metadata `json:"metadata,omitempty"`
	MetadataError string              `json:"metadataError,omitempty"`
	Draft         *draftDTO           `json:"draft,omitempty"`
	EngineSource  string              `json:"engineSource,omitempty"`
	SkipReason    string              `json:"skipReason,omitempty"`
}

// scanResponse is the wire shape of POST /agent/v1/scan.
type scanResponse struct {
	Found   int             `json:"found"`
	Drafted int             `json:"drafted"`
	Skipped int             `json:"skipped"`
	Results []fileResultDTO `json:"results"`
}

// DraftRefiner is the seam for post-rule draft refinement (planned F3:
// LLM-assisted or heuristic refinement of the rule-based draft). It is
// nil by default, which means drafts are exactly what
// discovery.DraftForFile produces. When non-nil, handleDraft runs it on
// every draft before validation and returns its result to the caller;
// an error fails the request with 422 without side effects.
var DraftRefiner func(cfg config.Config, meta discovery.Metadata, draft discovery.Draft) (discovery.Draft, error)

// handleScan runs discovery over the configured discovery.folders and
// returns one entry per .gguf file: metadata, skip reason or draft.
func (a *api) handleScan(w http.ResponseWriter, r *http.Request) {
	result, err := discovery.Run(a.cfg)
	if err != nil {
		// Config-level problems (no discovery section, unreadable
		// folders): the request is fine but the server's config is not
		// set up for scanning.
		writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	resp := scanResponse{
		Found:   result.Found,
		Drafted: result.Drafted,
		Skipped: result.Skipped,
		Results: make([]fileResultDTO, 0, len(result.Results)),
	}
	for _, fr := range result.Results {
		dto := fileResultDTO{
			Path:          fr.Path,
			Status:        fr.Status,
			Metadata:      fr.Metadata,
			MetadataError: fr.MetadataError,
			EngineSource:  fr.EngineSource,
			SkipReason:    fr.SkipReason,
		}
		if fr.Draft != nil {
			d := draftToDTO(*fr.Draft)
			dto.Draft = &d
		}
		resp.Results = append(resp.Results, dto)
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// engineDTO is the wire shape of one engine in GET /agent/v1/engines.
// Env names are listed without values: engine env carries secrets as
// often as not, and callers only need to know which names are set.
type engineDTO struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Cmd     string   `json:"cmd"`
	EnvKeys []string `json:"envKeys"`
}

// handleEngines lists the defined engines in definition order.
func (a *api) handleEngines(w http.ResponseWriter, r *http.Request) {
	names := a.cfg.EnginesOrder
	if len(names) == 0 {
		for name := range a.cfg.Engines {
			names = append(names, name)
		}
	}

	engines := make([]engineDTO, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		engine, found := a.cfg.Engines[name]
		if !found {
			continue
		}
		engines = append(engines, engineDTO{
			Name:    name,
			Label:   engine.Label,
			Cmd:     engine.Cmd,
			EnvKeys: envNames(engine.Env),
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"engines": engines})
}

// envNames returns the NAME half of each NAME=value env entry. Values are
// dropped on purpose: they are credentials more often than not.
func envNames(entries []string) []string {
	if len(entries) == 0 {
		return []string{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if i := strings.IndexByte(entry, '='); i > 0 {
			names = append(names, entry[:i])
		} else {
			names = append(names, entry)
		}
	}
	return names
}

// draftRequest is the body of POST /agent/v1/draft.
type draftRequest struct {
	// Path is the model file (typically .gguf) to draft a stanza for.
	Path string `json:"path"`
}

// handleDraft drafts a model stanza for one model file: metadata, engine
// selection and args, exactly what `llama-swap discover` produces for a
// single file, plus validation. Read-only.
func (a *api) handleDraft(w http.ResponseWriter, r *http.Request) {
	var req draftRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeError(w, r, http.StatusBadRequest, `"path" is required`)
		return
	}

	meta, err := discovery.ReadMetadata(req.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, r, http.StatusNotFound, "model file not found")
			return
		}
		writeError(w, r, http.StatusUnprocessableEntity, "could not parse model file: "+err.Error())
		return
	}

	draft, err := discovery.DraftForFile(a.cfg, meta)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// The F3 refinement seam: an optional transformation over the
	// rule-based draft, applied before validation so its output must
	// still pass the full config pipeline.
	if DraftRefiner != nil {
		draft, err = DraftRefiner(a.cfg, meta, draft)
		if err != nil {
			writeError(w, r, http.StatusUnprocessableEntity, "draft refinement failed: "+err.Error())
			return
		}
	}

	if err := draft.ValidateAgainst(a.cfg); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}

	writeJSON(w, r, http.StatusOK, draftToDTO(draft))
}

// refineResponse is the wire shape of POST /agent/v1/refine: the draft
// (refined when refined is true, the untouched rule-based draft
// otherwise) plus the refinement outcome. A fallback is a 200: the
// draft is usable either way and reason explains the difference.
type refineResponse struct {
	draftDTO

	// Refined is true when the LLM's answer passed every gate and is
	// what the draft fields carry.
	Refined bool `json:"refined"`

	// Reason explains a fallback; empty when refined.
	Reason string `json:"reason,omitempty"`
}

// handleRefine drafts a stanza for one model file and refines it. When
// the config enables the `draft_assistant:` section the refinement is
// LLM-assisted (internal/draftassistant): the provider's answer must
// parse as YAML, match the model-stanza schema and pass the full config
// validation, or the rule-based draft comes back with the reason in
// reason. Without that section the F2 DraftRefiner seam is honored when
// wired. Read-only: nothing is ever applied here.
func (a *api) handleRefine(w http.ResponseWriter, r *http.Request) {
	var req draftRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeError(w, r, http.StatusBadRequest, `"path" is required`)
		return
	}

	meta, err := discovery.ReadMetadata(req.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, r, http.StatusNotFound, "model file not found")
			return
		}
		writeError(w, r, http.StatusUnprocessableEntity, "could not parse model file: "+err.Error())
		return
	}

	rule, err := discovery.DraftForFile(a.cfg, meta)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}

	var draft discovery.Draft
	var refined bool
	var reason string
	switch {
	case a.cfg.DraftAssistant != nil && a.cfg.DraftAssistant.Enabled:
		outcome, err := draftassistant.Refine(r.Context(), a.cfg, meta, rule)
		if err != nil {
			// Configuration-class problems (key env var not set,
			// ...) are 422s, not fallbacks: the operator must fix
			// them, and the error says what.
			writeError(w, r, http.StatusUnprocessableEntity, err.Error())
			return
		}
		draft, refined, reason = outcome.Draft, outcome.Refined, outcome.Reason

	case DraftRefiner != nil:
		// The F2 seam: refinement wired by the embedder (tests,
		// heuristics) when no draft_assistant section is configured.
		draft, err = DraftRefiner(a.cfg, meta, rule)
		if err != nil {
			writeError(w, r, http.StatusUnprocessableEntity, "draft refinement failed: "+err.Error())
			return
		}
		refined = true

	default:
		writeError(w, r, http.StatusBadRequest, "refinement is not configured: enable the draft_assistant section (see docs/kb/guides/configuration/draft-assistant.md) or wire a DraftRefiner")
		return
	}

	// The hard gate: whatever produced the draft, it must still pass
	// the same validation as any config before it is handed out.
	if err := draft.ValidateAgainst(a.cfg); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}

	writeJSON(w, r, http.StatusOK, refineResponse{
		draftDTO: draftToDTO(draft),
		Refined:  refined,
		Reason:   reason,
	})
}

// validationResponse is the wire shape of POST /agent/v1/validate.
type validationResponse struct {
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors"`
}

// validateRequest is the body of POST /agent/v1/validate.
type validateRequest struct {
	// YAML is either a bare model stanza (the shape drafts produce,
	// pasted directly as the models: block) or a full configuration
	// document.
	YAML string `json:"yaml"`
}

// handleValidate validates the given YAML without side effects. A
// validation failure is a 200 with valid=false; transport problems
// (missing field, bad JSON) are 4xx errors.
func (a *api) handleValidate(w http.ResponseWriter, r *http.Request) {
	var req validateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.YAML) == "" {
		writeError(w, r, http.StatusBadRequest, `"yaml" is required`)
		return
	}

	valid, errs := a.validateYAML(req.YAML)
	writeJSON(w, r, http.StatusOK, validationResponse{Valid: valid, Errors: errs})
}

// validateYAML runs doc through the real config loader. A document whose
// top level carries a `models` key is treated as a full config and must
// stand on its own; anything else is treated as a bare model stanza and
// is spliced into this server's engines section first (the same
// treatment drafts get), so a stanza referencing a configured engine
// validates exactly like one written by hand.
func (a *api) validateYAML(doc string) (bool, []string) {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &top); err != nil {
		return false, []string{"YAML parse error: " + err.Error()}
	}
	if len(top) == 0 {
		return false, []string{"yaml must be a model stanza or a full config document"}
	}

	if _, isFullConfig := top["models"]; isFullConfig {
		if _, err := config.LoadConfigFromReader(strings.NewReader(doc)); err != nil {
			return false, []string{err.Error()}
		}
		return true, []string{}
	}

	// Bare stanza: wrap with the live engines section.
	var models map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &models); err != nil {
		return false, []string{"YAML parse error: " + err.Error()}
	}
	wrapped := map[string]any{
		"engines": a.cfg.Engines,
		"models":  models,
	}
	encoded, err := yaml.Marshal(wrapped)
	if err != nil {
		return false, []string{"encoding stanza for validation: " + err.Error()}
	}
	if _, err := config.LoadConfigFromReader(strings.NewReader(string(encoded))); err != nil {
		return false, []string{err.Error()}
	}
	return true, []string{}
}
