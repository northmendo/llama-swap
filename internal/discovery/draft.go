package discovery

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// quantHintRe matches the quantization tag conventionally embedded in GGUF
// filenames: Q4_K_M, IQ3_S, Q6_K, TQ1_0, PQ2_0, IQ2-mix, BF16, F16, F32.
// The Q is required (P?I?Q[0-9]+), which keeps model names that merely
// contain a Q (Qwen3-4B) out.
var quantHintRe = regexp.MustCompile(`(?i)\b(P?I?Q[0-9]+(?:_[A-Z0-9]+)*(?:-[A-Z0-9]+)*|TQ[0-9]_[0-9]|BF16|F16|F32)\b`)

// QuantHintFromFilename extracts the quantization tag from a GGUF filename
// (Q4_K_M, IQ3_S, PQ2_0, IQ2-mix, ...), uppercased. The hint is taken from
// the name, not the file contents: the quant lives in tensor info, not the
// KV metadata, and reading it would cost a full tensor-descriptor parse.
// Empty when the name carries no recognizable tag.
func QuantHintFromFilename(path string) string {
	matches := quantHintRe.FindAllString(filepath.Base(path), -1)
	if len(matches) == 0 {
		return ""
	}
	// Take the last tag: filenames put the quant at the end, after the
	// model name (…-27B-Q4_K_M.gguf).
	return strings.ToUpper(matches[len(matches)-1])
}

// ModelIDFromFilename derives a draft model ID from a GGUF filename: the
// base name without the .gguf suffix, lowercased, with underscores and
// whitespace turned into dashes. The full name (quant tag included) is
// kept so the ID stays traceable to its file.
func ModelIDFromFilename(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.ToLower(base)
	base = strings.NewReplacer("_", "-", " ", "-").Replace(base)
	base = strings.TrimSpace(base)
	for strings.Contains(base, "--") {
		base = strings.ReplaceAll(base, "--", "-")
	}
	return strings.Trim(base, "-")
}

// Draft is a rule-generated model stanza for one discovered GGUF file.
type Draft struct {
	// ModelID is the suggested models key.
	ModelID string

	// Engine is the engine name the draft references.
	Engine string

	// EngineSource explains how the engine was chosen: the matching rule,
	// or the default.
	EngineSource string

	// Args is the model-specific args block (with ${PORT}).
	Args string

	// CtxSize is the drafted --ctx-size: min(trained context, ctxSizeCap).
	// 0 when the file carries no context length, in which case no
	// --ctx-size line is drafted.
	CtxSize   int64
	CtxCapped bool

	// YAML is the full stanza, ready to paste under models:.
	YAML string
}

// ErrNoEngines is returned when a draft cannot pick an engine because the
// config defines no engines section.
var ErrNoEngines = fmt.Errorf("no engines are defined; define at least one engine in the engines section for discovery to draft against")

// SelectEngine applies discovery.engine_rules first-match-wins and falls
// back to the first defined engine. The returned source string explains the
// choice.
func SelectEngine(cfg config.Config, meta Metadata) (string, string, error) {
	if len(cfg.Engines) == 0 {
		return "", "", ErrNoEngines
	}

	for i, rule := range discoveryRules(cfg) {
		field := rule.Field
		if field == "" {
			field = "filename"
		}
		value := filepath.Base(meta.Path)
		if field == "architecture" {
			value = meta.Architecture
		}
		re, err := regexp.Compile(rule.Match)
		if err != nil {
			// cannot happen: validateDiscovery compiled every rule
			return "", "", fmt.Errorf("engine_rules[%d].match: %w", i, err)
		}
		if re.MatchString(value) {
			source := fmt.Sprintf("rule discovery.engine_rules[%d] (%s %q -> %s)", i, field, rule.Match, rule.Engine)
			return rule.Engine, source, nil
		}
	}

	first, ok := firstDefinedEngine(cfg)
	if !ok {
		return "", "", ErrNoEngines
	}
	return first, "default: first defined engine", nil
}

// firstDefinedEngine returns the first engine as defined in the YAML
// document, falling back to the alphabetically first name when definition
// order is unavailable (hand-built Config values, exotic merge orders).
func firstDefinedEngine(cfg config.Config) (string, bool) {
	if len(cfg.EnginesOrder) > 0 {
		for _, name := range cfg.EnginesOrder {
			if _, found := cfg.Engines[name]; found {
				return name, true
			}
		}
	}
	names := make([]string, 0, len(cfg.Engines))
	for name := range cfg.Engines {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", false
	}
	return names[0], true
}

func discoveryRules(cfg config.Config) []config.DiscoveryEngineRule {
	if cfg.Discovery == nil {
		return nil
	}
	return cfg.Discovery.EngineRules
}

// DraftForFile builds the draft model stanza for one file's metadata:
// model ID from the filename, engine by rule or default, and args with
// --port ${PORT}, --model <path> and --ctx-size min(trained ctx, cap).
func DraftForFile(cfg config.Config, meta Metadata) (Draft, error) {
	draft := Draft{}

	engine, source, err := SelectEngine(cfg, meta)
	if err != nil {
		return draft, err
	}
	draft.Engine = engine
	draft.EngineSource = source

	draft.ModelID = ModelIDFromFilename(meta.Path)

	var argLines []string
	argLines = append(argLines, "--port ${PORT}")
	argLines = append(argLines, "--model "+meta.Path)

	ctxCap := int64(config.DefaultDiscoveryCtxSizeCap)
	if cfg.Discovery != nil && cfg.Discovery.CtxSizeCap > 0 {
		ctxCap = int64(cfg.Discovery.CtxSizeCap)
	}
	if meta.ContextLength > 0 {
		draft.CtxSize = meta.ContextLength
		if meta.ContextLength > ctxCap {
			draft.CtxSize = ctxCap
			draft.CtxCapped = true
		}
		argLines = append(argLines, fmt.Sprintf("--ctx-size %d", draft.CtxSize))
	}
	draft.Args = strings.Join(argLines, "\n")

	draft.YAML = renderStanzaYAML(draft)
	return draft, nil
}

// renderStanzaYAML renders the draft as a two-space-indented stanza suitable
// for pasting directly under models:. The args block uses literal style so
// line breaks survive round-trips through the config loader.
func renderStanzaYAML(d Draft) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s:\n", d.ModelID)
	fmt.Fprintf(&b, "    engine: %s\n", d.Engine)
	b.WriteString("    args: |\n")
	for _, line := range strings.Split(d.Args, "\n") {
		fmt.Fprintf(&b, "      %s\n", line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// draftDoc is the minimal full-config shape used to validate a draft: the
// real engines section plus the single drafted model. Loading it through
// the real config pipeline exercises the same checks a hand-written stanza
// must pass (known engine, executable binary, ${PORT} composition, command
// sanitation).
type draftDoc struct {
	Engines map[string]config.EngineConfig `yaml:"engines"`
	Models  map[string]draftDocModel       `yaml:"models"`
}

type draftDocModel struct {
	Engine string `yaml:"engine"`
	Args   string `yaml:"args"`
}

// ValidateAgainst splices the draft into the given config's engines section
// and runs the result through the full config loader. A nil error means the
// draft stanza is exactly as valid as one written by hand.
func (d Draft) ValidateAgainst(cfg config.Config) error {
	doc := draftDoc{
		Engines: cfg.Engines,
		Models: map[string]draftDocModel{
			d.ModelID: {Engine: d.Engine, Args: d.Args},
		},
	}
	encoded, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("draft %s: encoding draft config: %w", d.ModelID, err)
	}
	if _, err := config.LoadConfigFromReader(strings.NewReader(string(encoded))); err != nil {
		return fmt.Errorf("draft %s: %w", d.ModelID, err)
	}
	return nil
}
