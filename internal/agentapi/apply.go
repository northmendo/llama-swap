package agentapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// applyRequest is the body of POST /agent/v1/apply.
type applyRequest struct {
	// YAML is a single model stanza (one top-level key: the model ID).
	YAML string `json:"yaml"`

	// Approved must be exactly true for anything to be written. Any
	// other value means "validate and report", nothing more.
	Approved bool `json:"approved"`
}

// applyResponse is the wire shape of a successful apply (approved or not).
type applyResponse struct {
	Status         string `json:"status"`
	Model          string `json:"model"`
	Approved       bool   `json:"approved"`
	Written        bool   `json:"written"`
	ReloadRequired bool   `json:"reloadRequired"`
	Backup         string `json:"backup,omitempty"`
	Message        string `json:"message,omitempty"`
}

// backupTimeFormat renders the timestamp in a config file backup's name.
// It sorts chronologically and is filesystem-safe everywhere.
const backupTimeFormat = "20060102T150405Z"

// handleApply validates a model stanza and, only when approved is true,
// merges it into the config file. Safety contract:
//
//   - invalid stanza -> 422, nothing written (not even a backup);
//   - valid but approved != true -> 200 with status "pending", nothing
//     written: the API is stateless, no draft is stored server-side;
//   - approved == true -> the stanza is merged (never overwriting the
//     rest of the config), a timestamped backup is taken first, and the
//     file is replaced atomically (temp file + rename), so a failure at
//     any point leaves the original config intact.
//
// llama-swap has no in-process reload trigger: the server hot-swaps on
// SIGHUP or on --watch-config's poll. Apply therefore reports
// reload_required (true unless --watch-config is on, in which case the
// watcher reloads the merged file automatically within its 2s poll).
func (a *api) handleApply(w http.ResponseWriter, r *http.Request) {
	var req applyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.YAML) == "" {
		writeError(w, r, http.StatusBadRequest, `"yaml" is required`)
		return
	}

	modelID, modelNode, err := parseModelStanza(req.YAML)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Validate exactly like /agent/v1/validate would: the stanza spliced
	// into the live engines section must pass the full config loader.
	stanza := map[string]yaml.Node{modelID: modelNode}
	wrapped := map[string]any{
		"engines": a.cfg.Engines,
		"models":  stanza,
	}
	encoded, err := yaml.Marshal(wrapped)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "encoding stanza for validation: "+err.Error())
		return
	}
	if _, err := config.LoadConfigFromReader(bytes.NewReader(encoded)); err != nil {
		writeJSON(w, r, http.StatusUnprocessableEntity, validationResponse{
			Valid:  false,
			Errors: []string{err.Error()},
		})
		return
	}

	if !req.Approved {
		writeJSON(w, r, http.StatusOK, applyResponse{
			Status:   "pending",
			Model:    modelID,
			Approved: false,
			Written:  false,
			Message:  "stanza is valid and ready to apply; nothing was written. Send the same request with \"approved\": true to merge it into the config file.",
		})
		return
	}

	if a.configPath() == "" {
		writeError(w, r, http.StatusConflict,
			"apply needs a single config file: restart llama-swap with -config <file> (apply cannot merge into a -config-dir setup)")
		return
	}

	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	backupPath, err := a.mergeAndWrite(modelID, modelNode)
	if err != nil {
		// mergeAndWrite only fails before the rename or at the rename
		// itself; either way the original config is intact.
		status := http.StatusInternalServerError
		var exists *modelExistsError
		var invalid *invalidMergedError
		switch {
		case errors.As(err, &exists):
			status = http.StatusConflict
		case errors.As(err, &invalid):
			status = http.StatusUnprocessableEntity
		}
		writeError(w, r, status, err.Error())
		return
	}

	if a.opts.Logger != nil {
		a.opts.Logger.Infof("agent api: applied model %s to %s (backup at %s)", modelID, a.configPath(), backupPath)
	}

	watchConfig := a.watchConfig()
	writeJSON(w, r, http.StatusOK, applyResponse{
		Status:         "applied",
		Model:          modelID,
		Approved:       true,
		Written:        true,
		ReloadRequired: !watchConfig,
		Backup:         backupPath,
		Message:        reloadMessage(watchConfig),
	})
}

func reloadMessage(watchConfig bool) string {
	if watchConfig {
		return "model merged into the config file; --watch-config will reload it automatically."
	}
	return "model merged into the config file; send SIGHUP (or restart) to load it, or run llama-swap with --watch-config for automatic reloads."
}

// parseModelStanza parses a single-model stanza and returns the model ID
// and its config node. It rejects anything that is not exactly one
// top-level mapping entry (full config documents included), because apply
// merges one model at a time.
func parseModelStanza(doc string) (string, yaml.Node, error) {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &top); err != nil {
		return "", yaml.Node{}, fmt.Errorf("YAML parse error: %w", err)
	}
	switch {
	case len(top) == 0:
		return "", yaml.Node{}, fmt.Errorf("stanza must be a YAML mapping with one model entry")
	case len(top) > 1:
		return "", yaml.Node{}, fmt.Errorf("apply takes exactly one model stanza, got %d entries", len(top))
	}
	if _, ok := top["models"]; ok {
		return "", yaml.Node{}, fmt.Errorf("apply takes a single model stanza, not a full config document; use /agent/v1/validate to check a full config")
	}

	for id, node := range top {
		if strings.TrimSpace(id) == "" {
			return "", yaml.Node{}, fmt.Errorf("model ID must not be empty")
		}
		if node.Kind != yaml.MappingNode {
			return "", yaml.Node{}, fmt.Errorf("model %s: stanza entry must be a mapping", id)
		}
		// Copy the node: it comes from the request document and must
		// survive being spliced into the config's node tree.
		value := node
		return id, value, nil
	}
	return "", yaml.Node{}, fmt.Errorf("unreachable")
}

// modelExistsError reports a rejected apply because the model ID is
// already defined in the live config. Overwriting a hand-tuned stanza
// behind an automation's back is exactly what apply must never do.
type modelExistsError struct {
	ModelID string
}

func (e *modelExistsError) Error() string {
	return fmt.Sprintf("model %s is already defined in the config file; apply does not overwrite existing models", e.ModelID)
}

// invalidMergedError reports a merged document that failed the full
// config loader. Nothing has been written when it is returned.
type invalidMergedError struct {
	Err error
}

func (e *invalidMergedError) Error() string {
	return fmt.Sprintf("merged config would not load: %v", e.Err)
}

func (e *invalidMergedError) Unwrap() error { return e.Err }

// mergeAndWrite performs the whole write path under the caller's lock:
// read the config file, merge the stanza into its models section, prove
// the merged document still loads, back the original up, then replace it
// atomically. Every failure path leaves the original file untouched.
func (a *api) mergeAndWrite(modelID string, modelNode yaml.Node) (string, error) {
	path := a.configPath()

	original, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading config file: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat config file: %w", err)
	}

	merged, err := mergeModelIntoConfig(original, modelID, modelNode)
	if err != nil {
		return "", err // modelExistsError or a parse error; nothing touched
	}

	// The merged document must load through the real pipeline before
	// anything is written: this is what guarantees an apply can never
	// leave the server with a config it would refuse to start from.
	if _, err := config.LoadConfigFromReader(bytes.NewReader(merged)); err != nil {
		return "", &invalidMergedError{Err: err}
	}

	backupPath := path + ".bak-" + time.Now().UTC().Format(backupTimeFormat)
	if err := os.WriteFile(backupPath, original, info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("writing config backup: %w", err)
	}

	if err := atomicWriteFile(path, merged, info.Mode().Perm()); err != nil {
		// The backup is a copy of the untouched original; leaving it
		// behind is harmless (and useful for forensics).
		return "", fmt.Errorf("writing config atomically: %w", err)
	}

	return backupPath, nil
}

// mergeModelIntoConfig splices one model stanza into the models section
// of an existing config document, preserving everything else in the file
// byte-for-byte as parsed: keys, order, comments and quoted styles all
// survive because the merge works on the YAML node tree.
func mergeModelIntoConfig(original []byte, modelID string, modelNode yaml.Node) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root = doc.Content[0]
	}

	// An empty file parses to a zero Node; start a mapping so the merge
	// has somewhere to land.
	if root.Kind == 0 {
		root.Kind = yaml.MappingNode
		root.Tag = "!!map"
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config file's top level must be a YAML mapping")
	}

	var models *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "models" {
			models = root.Content[i+1]
			break
		}
	}

	if models == nil {
		// No models section yet: create one at the end of the document.
		keyNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "models"}
		models = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &keyNode, models)
	}
	if models.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config file's models section must be a mapping")
	}

	for i := 0; i+1 < len(models.Content); i += 2 {
		if models.Content[i].Value == modelID {
			return nil, &modelExistsError{ModelID: modelID}
		}
	}

	idNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: modelID}
	models.Content = append(models.Content, &idNode, &modelNode)

	// Render with the 2-space indent llama-swap's own docs and drafts use;
	// yaml.Marshal defaults to 4.
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encoding merged config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encoding merged config: %w", err)
	}
	return buf.Bytes(), nil
}

// atomicWriteFile writes data to path via a temp file in the same
// directory followed by a rename, so readers never observe a partial
// file and a failure before the rename leaves the original in place.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agent-apply-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Best effort: never leave a temp file behind on failure.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file into place: %w", err)
	}
	return nil
}
