package draftassistant

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// stripCodeFences extracts the fenced-code-block content of an LLM
// answer. Models like to wrap YAML in ``` fences with a sentence of prose
// around them; everything outside the fences is dropped. Answers without
// fences are returned trimmed. An unclosed fence keeps its content.
func stripCodeFences(s string) string {
	if !strings.Contains(s, "```") {
		return strings.TrimSpace(s)
	}

	var blocks []string
	var current []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				blocks = append(blocks, strings.Join(current, "\n"))
				current = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			current = append(current, line)
		}
	}
	if inFence {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return strings.TrimSpace(strings.Join(blocks, "\n"))
}

// strictModelConfig is config.ModelConfig with no methods attached, so a
// strict yaml decoder (KnownFields(true)) can reject unknown stanza fields.
// Converting back to config.ModelConfig is a plain type conversion.
type strictModelConfig config.ModelConfig

// parseStanza gates the LLM's answer before it is trusted: the answer
// must parse as YAML (fences tolerated), must be a single model stanza
// (a bare `modelID:` mapping, optionally wrapped in `models:`) for the
// expected model ID, and every field must match the model-stanza schema
// — unknown fields are rejected by strict decoding into config's
// ModelConfig. On success it returns the model ID, the decoded model
// config and the paste-ready stanza YAML (indented for `models:`).
func parseStanza(content, wantModelID string) (string, config.ModelConfig, string, error) {
	text := stripCodeFences(content)
	if text == "" {
		return "", config.ModelConfig{}, "", fmt.Errorf("the answer is empty")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", config.ModelConfig{}, "", fmt.Errorf("YAML parse error: %w", err)
	}

	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return "", config.ModelConfig{}, "", fmt.Errorf("the answer is an empty YAML document")
		}
		root = root.Content[0]
	}

	// Tolerate a `models:` wrapper around the stanza.
	if root.Kind == yaml.MappingNode && len(root.Content) == 2 && root.Content[0].Value == "models" {
		root = root.Content[1]
	}

	if root.Kind != yaml.MappingNode || len(root.Content) == 0 {
		return "", config.ModelConfig{}, "", fmt.Errorf("the answer is not a YAML mapping of model stanzas")
	}

	// Strict decode: model IDs are free-form map keys, but every field
	// inside a stanza must be a real model field. Unknown fields are how
	// hallucinated config sneaks in, so they fail the gate here.
	//
	// The strict pass decodes into strictModelConfig, a type definition of
	// config.ModelConfig that carries no methods. config.ModelConfig's own
	// UnmarshalYAML takes a *yaml.Node and re-decodes through node.Decode(),
	// which starts a fresh decoder and therefore ignores the KnownFields(true)
	// set below (upstream v266 needs that node-based form to reject fractional
	// capacity values). A method-less copy keeps this gate strict.
	stanzaBytes, err := yaml.Marshal(root)
	if err != nil {
		return "", config.ModelConfig{}, "", fmt.Errorf("encoding the stanza for validation: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(stanzaBytes))
	dec.KnownFields(true)
	var strict map[string]strictModelConfig
	if err := dec.Decode(&strict); err != nil {
		return "", config.ModelConfig{}, "", fmt.Errorf("the stanza does not match the model-stanza schema: %w", err)
	}

	// A second pass through the real type applies config's model defaults
	// (default proxy, check endpoint, TTL) that the method-less copy skips,
	// and runs the model's own field checks such as fractional capacities.
	var models map[string]config.ModelConfig
	if err := yaml.Unmarshal(stanzaBytes, &models); err != nil {
		return "", config.ModelConfig{}, "", fmt.Errorf("the stanza does not match the model-stanza schema: %w", err)
	}

	if len(models) != 1 {
		ids := make([]string, 0, len(models))
		for id := range models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return "", config.ModelConfig{}, "", fmt.Errorf("expected exactly one model stanza, got %d (%s)", len(models), strings.Join(ids, ", "))
	}

	var modelID string
	var model config.ModelConfig
	for id, m := range models {
		modelID, model = id, m
	}

	if modelID != wantModelID {
		return "", config.ModelConfig{}, "", fmt.Errorf("expected a stanza for model %q, got %q (the model ID is fixed by the discovered file)", wantModelID, modelID)
	}
	if strings.TrimSpace(model.Engine) == "" {
		return "", config.ModelConfig{}, "", fmt.Errorf("the stanza has no engine field")
	}
	if strings.TrimSpace(model.Args) == "" {
		return "", config.ModelConfig{}, "", fmt.Errorf("the stanza has no args field")
	}

	stanzaYAML, err := renderStanza(root)
	if err != nil {
		return "", config.ModelConfig{}, "", fmt.Errorf("rendering the stanza: %w", err)
	}

	return modelID, model, stanzaYAML, nil
}

// renderStanza re-renders the parsed stanza mapping node as YAML indented
// to paste directly under `models:`, matching the shape discovery's rule
// drafts print (two-space model key, literal args block). The args value
// node is forced to literal style so a quoted-style answer still becomes a
// readable block.
func renderStanza(mapping *yaml.Node) (string, error) {
	// Find the args value node and prefer block style for multiline args.
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "args" && strings.Contains(mapping.Content[i+1].Value, "\n") {
			mapping.Content[i+1].Style = yaml.LiteralStyle
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(mapping); err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "  " + line
		}
	}
	return strings.Join(lines, "\n"), nil
}

// validateStanza is the hard gate's second half: the parsed stanza must
// pass the SAME validation as any config. It splices the stanza into the
// live engines section and runs the merged document through the real
// config loader, so an unknown engine, a binary-less engine command,
// unparseable flags, a missing ${PORT} (via the default proxy), bad
// filters or any other config error rejects the refinement.
func validateStanza(cfg config.Config, modelID string, model config.ModelConfig) error {
	doc := struct {
		Engines map[string]config.EngineConfig `yaml:"engines"`
		Models  map[string]config.ModelConfig  `yaml:"models"`
	}{
		Engines: cfg.Engines,
		Models:  map[string]config.ModelConfig{modelID: model},
	}
	encoded, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encoding refined stanza for validation: %w", err)
	}
	if _, err := config.LoadConfigFromReader(bytes.NewReader(encoded)); err != nil {
		return err
	}
	return nil
}

// ctxSizeRe matches the drafted --ctx-size flag with a space or `=`
// separator, so the reported CtxSize follows whatever form the refined
// args use.
var ctxSizeRe = regexp.MustCompile(`--ctx-size(?:\s+|=)([0-9]+)`)

// ctxSizeFromArgs extracts the --ctx-size value from a model's args, 0
// when the refined stanza carries none.
func ctxSizeFromArgs(args string) int64 {
	m := ctxSizeRe.FindStringSubmatch(args)
	if m == nil {
		return 0
	}
	var v int64
	for _, d := range m[1] {
		v = v*10 + int64(d-'0')
	}
	return v
}
