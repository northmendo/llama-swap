// Config verbs of the agent API: the safe read-only view of the live
// configuration (GET /agent/v1/config), the typed write verbs for peers,
// selectors and the agent_api section, and the backup list/restore pair.
//
// Every write follows the apply contract from apply.go:
//
//   - the change is staged on the parsed YAML node tree, so the rest of
//     the file (keys, order, comments) survives byte-level formatting;
//   - the merged document must load through the real config pipeline
//     (config.LoadConfigFromReader, the same strict validation the binary
//     uses) before anything is written — a failure is a 400 carrying the
//     validator's exact message and leaves the file untouched;
//   - only then is a byte-identical timestamped backup taken next to the
//     config (same convention as /agent/v1/apply) and the file replaced
//     atomically;
//   - every verb is approval-gated the same way apply is: without
//     "approved": true the request is a dry run that reports "pending"
//     and writes nothing.
//
// API keys are never exposed and never accepted as literals: reads show
// only the ${env.VAR} macro the file references (or a set/unset boolean),
// and writes only accept macro references for keys, so a secret can
// never travel through this API in either direction.
package agentapi

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// envMacroRef matches exactly one whole ${env.VAR_NAME} reference. Key
// fields in this API accept only this shape — never a literal secret.
var envMacroRef = regexp.MustCompile(`^\$\{env\.([A-Za-z_][A-Za-z0-9_]*)\}$`)

// backupNameSuffix matches the timestamp half of a backup file name
// (config.yaml.bak-20060102T150405Z).
var backupNameSuffix = regexp.MustCompile(`^\d{8}T\d{6}Z$`)

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

// secretRefDTO is how a key field is exposed: the ${env.VAR} macro when
// the config file references one, plus whether any key is configured at
// all. The resolved value never appears.
type secretRefDTO struct {
	Macro string `json:"macro,omitempty"`
	Set   bool   `json:"set"`
}

func secretRefFromNode(n *yaml.Node) secretRefDTO {
	if n == nil || n.Tag == "!!null" {
		return secretRefDTO{}
	}
	if n.Kind == yaml.ScalarNode && envMacroRef.MatchString(n.Value) {
		return secretRefDTO{Macro: n.Value, Set: true}
	}
	return secretRefDTO{Set: true}
}

// peerStateDTO is one peer in GET /agent/v1/config.
type peerStateDTO struct {
	Name       string       `json:"name"`
	Proxy      string       `json:"proxy"`
	ApiKey     secretRefDTO `json:"apiKey"`
	TailcatKey secretRefDTO `json:"tailcatKey"`
	Models     []string     `json:"models"`
}

// selectorStateDTO is one selector in GET /agent/v1/config.
type selectorStateDTO struct {
	Name        string   `json:"name"`
	Strategy    string   `json:"strategy"`
	Targets     []string `json:"targets"`
	Spillover   int      `json:"spillover"`
	Cooldown    string   `json:"cooldown,omitempty"`
	Description string   `json:"description,omitempty"`
	Unlisted    bool     `json:"unlisted,omitempty"`
}

// agentAPIStateDTO is the agent_api section in GET /agent/v1/config.
type agentAPIStateDTO struct {
	Enabled bool         `json:"enabled"`
	Key     secretRefDTO `json:"key"`
}

// configStateDTO is the wire shape of GET /agent/v1/config and the
// post-change state embedded in every write response.
type configStateDTO struct {
	Peers     []peerStateDTO     `json:"peers"`
	Selectors []selectorStateDTO `json:"selectors"`
	AgentAPI  *agentAPIStateDTO  `json:"agentApi,omitempty"`
}

// configWriteResponse is the wire shape of every write verb, for both
// the dry run (status "pending", nothing written) and the applied call.
type configWriteResponse struct {
	Status         string          `json:"status"`
	Approved       bool            `json:"approved"`
	Written        bool            `json:"written"`
	ReloadRequired bool            `json:"reloadRequired"`
	Backup         string          `json:"backup,omitempty"`
	Message        string          `json:"message,omitempty"`
	Config         *configStateDTO `json:"config,omitempty"`
}

// backupDTO is one row of GET /agent/v1/config/backups.
type backupDTO struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"sizeBytes"`
	ModifiedAt string `json:"modifiedAt"`
}

// ---------------------------------------------------------------------------
// Typed errors for the mutation stage
// ---------------------------------------------------------------------------

// notFoundError maps to 404 (unknown peer/selector/backup).
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

// conflictError maps to 409 (duplicate name).
type conflictError struct{ msg string }

func (e *conflictError) Error() string { return e.msg }

// ---------------------------------------------------------------------------
// State building
// ---------------------------------------------------------------------------

// handleConfigState is GET /agent/v1/config: the live configuration as
// structured JSON, with every key field reduced to its ${env.VAR} macro
// or a set/unset boolean. The resolved values never leave the process.
func (a *api) handleConfigState(w http.ResponseWriter, r *http.Request) {
	raw, err := a.readConfigFile()
	if err != nil {
		// Reading the file is best-effort here: the in-memory config is
		// still a safe (macro-less) view when -config-dir is in use.
		raw = nil
	}
	writeJSON(w, r, http.StatusOK, configStateFrom(raw, a.cfg))
}

// readConfigFile returns the raw config file bytes, nil when unset or
// unreadable.
func (a *api) readConfigFile() ([]byte, error) {
	path := a.configPath()
	if path == "" {
		return nil, fmt.Errorf("no config file")
	}
	return os.ReadFile(path)
}

// configStateFrom builds the safe state view: lists come from the loaded
// (resolved) config, key macros from the raw document so the viewer sees
// exactly what the file says. raw may be nil.
func configStateFrom(raw []byte, cfg config.Config) configStateDTO {
	state := configStateDTO{
		Peers:     []peerStateDTO{},
		Selectors: []selectorStateDTO{},
	}

	rawRoot := parseRawRoot(raw)

	peerNames := make([]string, 0, len(cfg.Peers))
	for name := range cfg.Peers {
		peerNames = append(peerNames, name)
	}
	sort.Strings(peerNames)
	for _, name := range peerNames {
		peer := cfg.Peers[name]
		dto := peerStateDTO{
			Name:       name,
			Proxy:      peer.Proxy,
			ApiKey:     secretRefDTO{Set: peer.ApiKey != ""},
			TailcatKey: secretRefDTO{Set: peer.TailcatKey != ""},
			Models:     append([]string(nil), peer.Models...),
		}
		if peerNode := mappingValue(mappingValue(rawRoot, "peers"), name); peerNode != nil {
			dto.ApiKey = secretRefFromNode(mappingValue(peerNode, "apiKey"))
			dto.TailcatKey = secretRefFromNode(mappingValue(peerNode, "tailcatKey"))
		}
		state.Peers = append(state.Peers, dto)
	}

	selectorNames := make([]string, 0, len(cfg.Selectors))
	for name := range cfg.Selectors {
		selectorNames = append(selectorNames, name)
	}
	sort.Strings(selectorNames)
	for _, name := range selectorNames {
		sel := cfg.Selectors[name]
		// The configured targets are authoritative; the raw file shows
		// the same list unless it is stale, which only happens between
		// a write and the reload.
		state.Selectors = append(state.Selectors, selectorStateDTO{
			Name:        name,
			Strategy:    sel.Strategy,
			Targets:     append([]string(nil), sel.Targets...),
			Spillover:   sel.Settings.Spillover,
			Cooldown:    sel.Settings.Cooldown,
			Description: sel.Description,
			Unlisted:    sel.Unlisted,
		})
	}

	if cfg.AgentAPI != nil {
		agent := &agentAPIStateDTO{
			Enabled: cfg.AgentAPI.Enabled,
			Key:     secretRefDTO{Set: cfg.AgentAPI.Key != ""},
		}
		if node := mappingValue(mappingValue(rawRoot, "agent_api"), "key"); node != nil {
			agent.Key = secretRefFromNode(node)
		}
		state.AgentAPI = agent
	}
	return state
}

// parseRawRoot parses raw YAML into its root mapping node, or returns a
// nil node when raw is empty or does not parse.
func parseRawRoot(raw []byte) *yaml.Node {
	if len(raw) == 0 {
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	return root
}

// ---------------------------------------------------------------------------
// YAML node helpers (operate on the parsed document so comments, order
// and untouched sections survive every write)
// ---------------------------------------------------------------------------

// mappingValue returns the value node for key, nil when absent.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mappingSetValue sets key to value, appending the pair when new.
func mappingSetValue(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// mappingRemove deletes key (and its value) from the mapping, reporting
// whether the key was there.
func mappingRemove(m *yaml.Node, key string) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// ensureSection returns the mapping under a top-level key, creating an
// empty mapping when the section is absent.
func ensureSection(root *yaml.Node, key string) *yaml.Node {
	section := mappingValue(root, key)
	if section == nil {
		section = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mappingSetValue(root, key, section)
		return section
	}
	if section.Kind != yaml.MappingNode {
		panic(fmt.Sprintf("config section %q must be a mapping", key))
	}
	return section
}

// dropSectionIfEmpty removes a top-level section that has no entries
// left, so a remove of the last peer does not strand `peers: {}`.
func dropSectionIfEmpty(root *yaml.Node, key string) {
	section := mappingValue(root, key)
	if section != nil && section.Kind == yaml.MappingNode && len(section.Content) == 0 {
		mappingRemove(root, key)
	}
}

func scalarStr(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func scalarInt(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}

func scalarBool(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}
}

func seqStr(values []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, v := range values {
		seq.Content = append(seq.Content, scalarStr(v))
	}
	return seq
}

// parseDocument parses a config file into its root mapping node.
func parseDocument(original []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root = doc.Content[0]
	}
	if root.Kind == 0 {
		root.Kind = yaml.MappingNode
		root.Tag = "!!map"
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config file's top level must be a YAML mapping")
	}
	return root, nil
}

// encodeDocument renders a node tree with the 2-space indent llama-swap's
// own docs and apply use.
func encodeDocument(root *yaml.Node) ([]byte, error) {
	doc := yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encoding config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encoding config: %w", err)
	}
	return buf.Bytes(), nil
}

// requireEnvMacro checks that a key field carries an ${env.VAR}
// reference, rejecting literal secrets at the door.
func requireEnvMacro(field, value string) error {
	if value == "" || envMacroRef.MatchString(value) {
		return nil
	}
	return fmt.Errorf("%s must reference an environment variable macro (e.g. ${env.MY_KEY}); literal secrets are rejected by this API", field)
}

// ---------------------------------------------------------------------------
// The shared write pipeline
// ---------------------------------------------------------------------------

// stagedChange is one validated-then-maybe-written config mutation.
type stagedChange struct {
	// verb names the change in log lines, e.g. "peers.add upstream".
	verb string

	// mutate stages the change on the parsed document. It returns a
	// notFoundError (404), conflictError (409) or a plain error (500)
	// without touching the file.
	mutate func(root *yaml.Node) error

	// approved gates the write exactly like /agent/v1/apply: false is a
	// validate-only dry run.
	approved bool
}

// writeStaged runs the full safety pipeline for one verb: stage on the
// node tree, prove the merged document loads, dry-run report or backup +
// atomic replace. Every failure path leaves the original file intact.
func (a *api) writeStaged(w http.ResponseWriter, r *http.Request, change stagedChange) (*configWriteResponse, bool) {
	path := a.configPath()
	if path == "" {
		writeError(w, r, http.StatusConflict,
			"this verb needs a single config file: restart llama-swap with -config <file> (it cannot merge into a -config-dir setup)")
		return nil, false
	}

	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	original, err := os.ReadFile(path)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "reading config file: "+err.Error())
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "stat config file: "+err.Error())
		return nil, false
	}

	root, err := parseDocument(original)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if err := change.mutate(root); err != nil {
		var nf *notFoundError
		var cf *conflictError
		switch {
		case errAs(err, &nf):
			writeError(w, r, http.StatusNotFound, err.Error())
		case errAs(err, &cf):
			writeError(w, r, http.StatusConflict, err.Error())
		default:
			writeError(w, r, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}

	merged, err := encodeDocument(root)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err.Error())
		return nil, false
	}

	// The merged document must load through the real pipeline before
	// anything is written: this is the same strict validation the binary
	// applies at startup, so a write can never leave the server with a
	// config it would refuse to start from.
	loaded, err := config.LoadConfigFromReader(bytes.NewReader(merged))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error())
		return nil, false
	}

	state := configStateFrom(merged, loaded)

	if !change.approved {
		resp := &configWriteResponse{
			Status:   "pending",
			Approved: false,
			Written:  false,
			Message:  "change is valid and ready to apply; nothing was written. Send the same request with \"approved\": true to write it.",
			Config:   &state,
		}
		writeJSON(w, r, http.StatusOK, resp)
		return resp, true
	}

	backupPath := path + ".bak-" + time.Now().UTC().Format(backupTimeFormat)
	if err := os.WriteFile(backupPath, original, info.Mode().Perm()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "writing config backup: "+err.Error())
		return nil, false
	}
	if err := atomicWriteFile(path, merged, info.Mode().Perm()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "writing config atomically: "+err.Error())
		return nil, false
	}

	if a.opts.Logger != nil {
		a.opts.Logger.Infof("agent api: applied %s to %s (backup at %s)", change.verb, path, backupPath)
	}

	watchConfig := a.watchConfig()
	resp := &configWriteResponse{
		Status:         "applied",
		Approved:       true,
		Written:        true,
		ReloadRequired: !watchConfig,
		Backup:         backupPath,
		Message:        reloadMessage(watchConfig),
		Config:         &state,
	}
	writeJSON(w, r, http.StatusOK, resp)
	return resp, true
}

// errAs is errors.As with the indirection the linter wants for the two
// concrete types above.
func errAs(err error, target any) bool {
	switch t := target.(type) {
	case **notFoundError:
		nf, ok := err.(*notFoundError)
		if ok {
			*t = nf
		}
		return ok
	case **conflictError:
		cf, ok := err.(*conflictError)
		if ok {
			*t = cf
		}
		return ok
	}
	return false
}

// ---------------------------------------------------------------------------
// Peers
// ---------------------------------------------------------------------------

// peerWriteRequest is the shared body of the peer verbs. Fields left nil
// (or empty for add) keep their configured values; apiKey/tailcatKey
// accept an ${env.VAR} reference or empty — never a literal secret.
type peerWriteRequest struct {
	Approved   bool      `json:"approved"`
	Name       string    `json:"name"`
	Proxy      *string   `json:"proxy"`
	ApiKey     *string   `json:"apiKey"`
	TailcatKey *string   `json:"tailcatKey"`
	Models     *[]string `json:"models"`
}

// handlePeerAdd is POST /agent/v1/config/peers.add.
func (a *api) handlePeerAdd(w http.ResponseWriter, r *http.Request) {
	var req peerWriteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, http.StatusBadRequest, `"name" is required`)
		return
	}
	if req.Proxy == nil || strings.TrimSpace(*req.Proxy) == "" {
		writeError(w, r, http.StatusBadRequest, `"proxy" is required`)
		return
	}
	if req.Models == nil || len(*req.Models) == 0 {
		writeError(w, r, http.StatusBadRequest, `"models" must contain at least one entry`)
		return
	}
	for _, field := range []struct {
		label string
		value *string
	}{{"apiKey", req.ApiKey}, {"tailcatKey", req.TailcatKey}} {
		if field.value != nil {
			if err := requireEnvMacro(field.label, *field.value); err != nil {
				writeError(w, r, http.StatusBadRequest, err.Error())
				return
			}
		}
	}

	proxy := strings.TrimSpace(*req.Proxy)
	models := append([]string(nil), (*req.Models)...)

	a.writeStaged(w, r, stagedChange{
		verb:     "peers.add " + name,
		approved: req.Approved,
		mutate: func(root *yaml.Node) error {
			peers := ensureSection(root, "peers")
			if mappingValue(peers, name) != nil {
				return &conflictError{msg: fmt.Sprintf("peers.%s is already defined in the config file", name)}
			}
			stanza := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			mappingSetValue(stanza, "proxy", scalarStr(proxy))
			if req.ApiKey != nil && *req.ApiKey != "" {
				mappingSetValue(stanza, "apiKey", scalarStr(*req.ApiKey))
			}
			if req.TailcatKey != nil && *req.TailcatKey != "" {
				mappingSetValue(stanza, "tailcatKey", scalarStr(*req.TailcatKey))
			}
			mappingSetValue(stanza, "models", seqStr(models))
			mappingSetValue(peers, name, stanza)
			return nil
		},
	})
}

// handlePeerUpdate is POST /agent/v1/config/peers.update: overlay the
// provided fields on the existing stanza, preserving everything the
// request does not mention (filters, timeouts, ...).
func (a *api) handlePeerUpdate(w http.ResponseWriter, r *http.Request) {
	var req peerWriteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, http.StatusBadRequest, `"name" is required`)
		return
	}
	for _, field := range []struct {
		label string
		value *string
	}{{"apiKey", req.ApiKey}, {"tailcatKey", req.TailcatKey}} {
		if field.value != nil {
			if err := requireEnvMacro(field.label, *field.value); err != nil {
				writeError(w, r, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	if req.Models != nil && len(*req.Models) == 0 {
		writeError(w, r, http.StatusBadRequest, `"models" must contain at least one entry`)
		return
	}

	a.writeStaged(w, r, stagedChange{
		verb:     "peers.update " + name,
		approved: req.Approved,
		mutate: func(root *yaml.Node) error {
			peer := mappingValue(mappingValue(root, "peers"), name)
			if peer == nil {
				return &notFoundError{msg: fmt.Sprintf("peers.%s is not defined in the config file", name)}
			}
			if peer.Kind != yaml.MappingNode {
				return fmt.Errorf("peers.%s must be a mapping", name)
			}
			if req.Proxy != nil {
				if strings.TrimSpace(*req.Proxy) == "" {
					return fmt.Errorf("peers.%s.proxy must not be empty", name)
				}
				mappingSetValue(peer, "proxy", scalarStr(strings.TrimSpace(*req.Proxy)))
			}
			if req.ApiKey != nil {
				if *req.ApiKey == "" {
					mappingRemove(peer, "apiKey")
				} else {
					mappingSetValue(peer, "apiKey", scalarStr(*req.ApiKey))
				}
			}
			if req.TailcatKey != nil {
				if *req.TailcatKey == "" {
					mappingRemove(peer, "tailcatKey")
				} else {
					mappingSetValue(peer, "tailcatKey", scalarStr(*req.TailcatKey))
				}
			}
			if req.Models != nil {
				mappingSetValue(peer, "models", seqStr(*req.Models))
			}
			return nil
		},
	})
}

// peerRemoveRequest is the body of POST /agent/v1/config/peers.remove.
type peerRemoveRequest struct {
	Approved bool   `json:"approved"`
	Name     string `json:"name"`
}

// handlePeerRemove is POST /agent/v1/config/peers.remove. A peer still
// referenced by a selector is rejected by the merged-document
// validation, so the selector can never be left dangling.
func (a *api) handlePeerRemove(w http.ResponseWriter, r *http.Request) {
	var req peerRemoveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, http.StatusBadRequest, `"name" is required`)
		return
	}

	a.writeStaged(w, r, stagedChange{
		verb:     "peers.remove " + name,
		approved: req.Approved,
		mutate: func(root *yaml.Node) error {
			peers := mappingValue(root, "peers")
			if peers == nil || mappingValue(peers, name) == nil {
				return &notFoundError{msg: fmt.Sprintf("peers.%s is not defined in the config file", name)}
			}
			mappingRemove(peers, name)
			dropSectionIfEmpty(root, "peers")
			return nil
		},
	})
}

// ---------------------------------------------------------------------------
// Selectors
// ---------------------------------------------------------------------------

// selectorWriteRequest is the shared body of the selector verbs.
type selectorWriteRequest struct {
	Approved    bool      `json:"approved"`
	Name        string    `json:"name"`
	Strategy    *string   `json:"strategy"`
	Targets     *[]string `json:"targets"`
	Spillover   *int      `json:"spillover"`
	Cooldown    *string   `json:"cooldown"`
	Description *string   `json:"description"`
	Unlisted    *bool     `json:"unlisted"`
}

// applySelectorFields overlays the provided fields onto a selector
// stanza node (or builds a fresh one when stanza is nil), returning the
// stanza that must be stored under the selector's name.
func applySelectorFields(name string, stanza *yaml.Node, req selectorWriteRequest) (*yaml.Node, error) {
	if stanza == nil {
		stanza = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if stanza.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("selectors.%s must be a mapping", name)
	}
	if req.Strategy != nil {
		if *req.Strategy == "" {
			return nil, fmt.Errorf("selectors.%s.strategy must not be empty", name)
		}
		mappingSetValue(stanza, "strategy", scalarStr(*req.Strategy))
	}
	if req.Targets != nil {
		if len(*req.Targets) == 0 {
			return nil, fmt.Errorf("selectors.%s.targets must contain at least one entry", name)
		}
		mappingSetValue(stanza, "targets", seqStr(*req.Targets))
	}
	if req.Spillover != nil || req.Cooldown != nil {
		settings := mappingValue(stanza, "settings")
		if settings == nil {
			settings = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			mappingSetValue(stanza, "settings", settings)
		}
		if req.Spillover != nil {
			mappingSetValue(settings, "spillover", scalarInt(*req.Spillover))
		}
		if req.Cooldown != nil {
			if *req.Cooldown == "" {
				mappingRemove(settings, "cooldown")
			} else {
				mappingSetValue(settings, "cooldown", scalarStr(*req.Cooldown))
			}
		}
	}
	if req.Description != nil {
		if *req.Description == "" {
			mappingRemove(stanza, "description")
		} else {
			mappingSetValue(stanza, "description", scalarStr(*req.Description))
		}
	}
	if req.Unlisted != nil {
		if *req.Unlisted {
			mappingSetValue(stanza, "unlisted", scalarBool(true))
		} else {
			mappingRemove(stanza, "unlisted")
		}
	}
	return stanza, nil
}

// handleSelectorAdd is POST /agent/v1/config/selectors.add.
func (a *api) handleSelectorAdd(w http.ResponseWriter, r *http.Request) {
	var req selectorWriteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, http.StatusBadRequest, `"name" is required`)
		return
	}
	if req.Strategy == nil || *req.Strategy == "" {
		writeError(w, r, http.StatusBadRequest, `"strategy" is required (warm, pin, spillover, failover)`)
		return
	}
	if req.Targets == nil || len(*req.Targets) == 0 {
		writeError(w, r, http.StatusBadRequest, `"targets" must contain at least one entry`)
		return
	}

	a.writeStaged(w, r, stagedChange{
		verb:     "selectors.add " + name,
		approved: req.Approved,
		mutate: func(root *yaml.Node) error {
			selectors := ensureSection(root, "selectors")
			if mappingValue(selectors, name) != nil {
				return &conflictError{msg: fmt.Sprintf("selectors.%s is already defined in the config file", name)}
			}
			stanza, err := applySelectorFields(name, nil, req)
			if err != nil {
				return err
			}
			mappingSetValue(selectors, name, stanza)
			return nil
		},
	})
}

// handleSelectorUpdate is POST /agent/v1/config/selectors.update.
func (a *api) handleSelectorUpdate(w http.ResponseWriter, r *http.Request) {
	var req selectorWriteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, http.StatusBadRequest, `"name" is required`)
		return
	}

	a.writeStaged(w, r, stagedChange{
		verb:     "selectors.update " + name,
		approved: req.Approved,
		mutate: func(root *yaml.Node) error {
			selector := mappingValue(mappingValue(root, "selectors"), name)
			if selector == nil {
				return &notFoundError{msg: fmt.Sprintf("selectors.%s is not defined in the config file", name)}
			}
			stanza, err := applySelectorFields(name, selector, req)
			if err != nil {
				return err
			}
			mappingSetValue(mappingValue(root, "selectors"), name, stanza)
			return nil
		},
	})
}

// selectorRemoveRequest is the body of POST /agent/v1/config/selectors.remove.
type selectorRemoveRequest struct {
	Approved bool   `json:"approved"`
	Name     string `json:"name"`
}

// handleSelectorRemove is POST /agent/v1/config/selectors.remove.
func (a *api) handleSelectorRemove(w http.ResponseWriter, r *http.Request) {
	var req selectorRemoveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, http.StatusBadRequest, `"name" is required`)
		return
	}

	a.writeStaged(w, r, stagedChange{
		verb:     "selectors.remove " + name,
		approved: req.Approved,
		mutate: func(root *yaml.Node) error {
			selectors := mappingValue(root, "selectors")
			if selectors == nil || mappingValue(selectors, name) == nil {
				return &notFoundError{msg: fmt.Sprintf("selectors.%s is not defined in the config file", name)}
			}
			mappingRemove(selectors, name)
			dropSectionIfEmpty(root, "selectors")
			return nil
		},
	})
}

// ---------------------------------------------------------------------------
// agent_api
// ---------------------------------------------------------------------------

// agentAPIUpdateRequest is the body of POST /agent/v1/config/agent_api.update.
// Key accepts an ${env.VAR} reference or "" (clear); nil keeps the
// configured key. A literal key is rejected.
type agentAPIUpdateRequest struct {
	Approved bool    `json:"approved"`
	Enabled  *bool   `json:"enabled"`
	Key      *string `json:"key"`
}

// handleAgentAPIUpdate is POST /agent/v1/config/agent_api.update.
func (a *api) handleAgentAPIUpdate(w http.ResponseWriter, r *http.Request) {
	var req agentAPIUpdateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Enabled == nil && req.Key == nil {
		writeError(w, r, http.StatusBadRequest, `nothing to update: set "enabled" and/or "key"`)
		return
	}
	if req.Key != nil {
		if err := requireEnvMacro("agent_api.key", *req.Key); err != nil {
			writeError(w, r, http.StatusBadRequest, err.Error())
			return
		}
	}

	resp, ok := a.writeStaged(w, r, stagedChange{
		verb:     "agent_api.update",
		approved: req.Approved,
		mutate: func(root *yaml.Node) error {
			section := ensureSection(root, "agent_api")
			if req.Enabled != nil {
				mappingSetValue(section, "enabled", scalarBool(*req.Enabled))
			}
			if req.Key != nil {
				if *req.Key == "" {
					mappingRemove(section, "key")
				} else {
					mappingSetValue(section, "key", scalarStr(*req.Key))
				}
			}
			return nil
		},
	})
	// Warn before the disable takes effect: after the next reload the
	// /agent/v1 routes (including this one) are unmounted.
	if ok && resp != nil && req.Enabled != nil && !*req.Enabled {
		resp.Message = "agent_api.enabled=false was written; after the next reload the /agent/v1 routes are unmounted and this API is the only way to re-enable it. " + resp.Message
	}
}

// ---------------------------------------------------------------------------
// Backups and restore
// ---------------------------------------------------------------------------

// handleConfigBackups is GET /agent/v1/config/backups: the timestamped
// backups taken by apply and the config verbs, newest first.
func (a *api) handleConfigBackups(w http.ResponseWriter, r *http.Request) {
	path := a.configPath()
	if path == "" {
		writeError(w, r, http.StatusConflict,
			"backups live next to the config file: restart llama-swap with -config <file> to use them")
		return
	}

	prefix := filepath.Base(path) + ".bak-"
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "reading config directory: "+err.Error())
		return
	}

	backups := make([]backupDTO, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if !backupNameSuffix.MatchString(strings.TrimPrefix(entry.Name(), prefix)) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backups = append(backups, backupDTO{
			Name:       entry.Name(),
			Path:       filepath.Join(filepath.Dir(path), entry.Name()),
			SizeBytes:  info.Size(),
			ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	// The names sort chronologically; newest first.
	sort.Sort(sort.Reverse(sortByBackupName(backups)))

	writeJSON(w, r, http.StatusOK, map[string]any{"backups": backups})
}

type sortByBackupName []backupDTO

func (b sortByBackupName) Len() int           { return len(b) }
func (b sortByBackupName) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b sortByBackupName) Less(i, j int) bool { return b[i].Name < b[j].Name }

// restoreRequest is the body of POST /agent/v1/config/restore.
type restoreRequest struct {
	// Backup names one of the timestamped backups (basename only),
	// as listed by GET /agent/v1/config/backups.
	Backup string `json:"backup"`

	// Approved must be true for anything to be written, exactly like
	// /agent/v1/apply.
	Approved bool `json:"approved"`
}

// restoreResponse is the wire shape of POST /agent/v1/config/restore.
type restoreResponse struct {
	Status         string `json:"status"`
	Approved       bool   `json:"approved"`
	Written        bool   `json:"written"`
	ReloadRequired bool   `json:"reloadRequired"`
	Backup         string `json:"backup,omitempty"`
	RestoredFrom   string `json:"restoredFrom,omitempty"`
	Message        string `json:"message,omitempty"`
}

// handleConfigRestore is POST /agent/v1/config/restore: put a previous
// backup back in place. The backup content must load through the real
// config pipeline first; the current config is backed up before it is
// replaced, so a restore is itself reversible.
func (a *api) handleConfigRestore(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Backup)
	if name == "" {
		writeError(w, r, http.StatusBadRequest, `"backup" is required (a name from GET /agent/v1/config/backups)`)
		return
	}
	// Basenames only: a restore must never be tricked into reading or
	// writing outside the config's directory.
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		writeError(w, r, http.StatusBadRequest, `"backup" must be a backup file name, not a path`)
		return
	}

	path := a.configPath()
	if path == "" {
		writeError(w, r, http.StatusConflict,
			"restore needs a single config file: restart llama-swap with -config <file>")
		return
	}
	dir := filepath.Dir(path)
	if !strings.HasPrefix(name, filepath.Base(path)+".bak-") || !backupNameSuffix.MatchString(strings.TrimPrefix(name, filepath.Base(path)+".bak-")) {
		writeError(w, r, http.StatusNotFound, "unknown backup "+name)
		return
	}

	backupPath := filepath.Join(dir, name)
	content, err := os.ReadFile(backupPath)
	if err != nil {
		writeError(w, r, http.StatusNotFound, "unknown backup "+name)
		return
	}

	// A backup the server would refuse to start from is not restorable:
	// the same strict validation every write passes.
	if _, err := config.LoadConfigFromReader(bytes.NewReader(content)); err != nil {
		writeError(w, r, http.StatusBadRequest, "backup would not load: "+err.Error())
		return
	}

	if !req.Approved {
		writeJSON(w, r, http.StatusOK, restoreResponse{
			Status:  "pending",
			Message: "backup loads cleanly; nothing was written. Send the same request with \"approved\": true to restore it.",
		})
		return
	}

	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	current, err := os.ReadFile(path)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "reading config file: "+err.Error())
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "stat config file: "+err.Error())
		return
	}

	safetyPath := path + ".bak-" + time.Now().UTC().Format(backupTimeFormat)
	if err := os.WriteFile(safetyPath, current, info.Mode().Perm()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "writing config backup: "+err.Error())
		return
	}
	if err := atomicWriteFile(path, content, info.Mode().Perm()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "writing config atomically: "+err.Error())
		return
	}

	if a.opts.Logger != nil {
		a.opts.Logger.Infof("agent api: restored config from %s (backup of the replaced config at %s)", backupPath, safetyPath)
	}

	watchConfig := a.watchConfig()
	writeJSON(w, r, http.StatusOK, restoreResponse{
		Status:         "applied",
		Approved:       true,
		Written:        true,
		ReloadRequired: !watchConfig,
		Backup:         safetyPath,
		RestoredFrom:   backupPath,
		Message:        reloadMessage(watchConfig),
	})
}
