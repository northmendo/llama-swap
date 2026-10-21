package draftassistant

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
)

// buildPrompts produces the (system, user) message pair for one
// refinement. The system message fixes the output contract (YAML only);
// the user message carries everything the model needs to decide: the
// extracted GGUF metadata as JSON, the rule-based draft it refines, the
// defined engines with their cmd lines and labels, and the user's
// optional conventions block. Engine env values are never included —
// only the names, since engine env is a common place for secrets.
func buildPrompts(cfg config.Config, meta discovery.Metadata, rule discovery.Draft) (system, user string) {
	system = fmt.Sprintf(`You are a configuration assistant for llama-swap, a model swapping proxy.
You refine draft model stanzas produced by its rule-based discovery.

Output contract — follow it exactly:
- Answer with YAML ONLY: a single model stanza, no prose, no markdown
  fences, no comments, no explanation.
- The stanza is a mapping with exactly one key: the model ID %q.
- The model ID is fixed by the discovered file; do not rename it.
- Allowed fields are llama-swap model fields: engine, args, and when
  genuinely useful ttl, env, aliases, name, description. Anything else
  is rejected.
- engine must be one of the engines listed in the input.
- args is a multiline string of launch flags and MUST keep
  --port ${PORT} and --model with the same model file path.
- Improve what the rules cannot decide: context size, quantization-aware
  flags, flash attention, threads, GPU layers, jinja/chat templates,
  MoE tuning, engine choice. Do not invent flags you are unsure about;
  an invalid stanza is discarded and the rule-based draft is used.`, rule.ModelID)

	var b strings.Builder
	fmt.Fprintf(&b, "Refine the draft stanza below for llama-swap.\n\n")

	fmt.Fprintf(&b, "## Model file metadata (extracted from the GGUF)\n")
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		// Metadata is plain ints/strings; marshal cannot fail in
		// practice, but degrade to the file path rather than skip it.
		encoded = []byte(fmt.Sprintf("{\n  \"path\": %q\n}", meta.Path))
	}
	fmt.Fprintf(&b, "%s\n\n", encoded)

	fmt.Fprintf(&b, "## Rule-based draft to refine (YAML)\n")
	fmt.Fprintf(&b, "%s\n\n", indent(rule.YAML))

	fmt.Fprintf(&b, "## Available engines\n")
	for _, e := range engineSummaries(cfg) {
		fmt.Fprintf(&b, "- %s (label: %s)\n      cmd: %s\n", e.name, e.label, e.cmd)
		if len(e.envNames) > 0 {
			fmt.Fprintf(&b, "      env names only (values are not shown): %s\n", strings.Join(e.envNames, ", "))
		}
	}

	if conventions := strings.TrimSpace(cfgSection(cfg).Conventions); conventions != "" {
		fmt.Fprintf(&b, "\n## User conventions (follow these when they apply)\n%s\n", conventions)
	}

	return system, b.String()
}

// engineSummary is one engine as shown in the prompt.
type engineSummary struct {
	name     string
	label    string
	cmd      string
	envNames []string
}

// engineSummaries lists the defined engines in definition order, with
// env NAMES only. Definition order comes from EnginesOrder; a
// hand-built Config without it falls back to sorted names.
func engineSummaries(cfg config.Config) []engineSummary {
	names := cfg.EnginesOrder
	seen := make(map[string]bool, len(names))
	sorted := make([]string, 0, len(cfg.Engines))
	for name := range cfg.Engines {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	out := make([]engineSummary, 0, len(cfg.Engines))
	add := func(name string) {
		if seen[name] {
			return
		}
		engine, found := cfg.Engines[name]
		if !found {
			return
		}
		seen[name] = true
		out = append(out, engineSummary{
			name:     name,
			label:    engine.Label,
			cmd:      engine.Cmd,
			envNames: envNames(engine.Env),
		})
	}
	for _, name := range names {
		add(name)
	}
	for _, name := range sorted {
		add(name)
	}
	return out
}

// cfgSection returns the draft_assistant section, tolerating a nil one
// (prompts degrade to no conventions block).
func cfgSection(cfg config.Config) config.DraftAssistantConfig {
	if cfg.DraftAssistant == nil {
		return config.DraftAssistantConfig{}
	}
	return *cfg.DraftAssistant
}

// envNames returns the NAME half of each NAME=value env entry; values are
// dropped because engine env often carries secrets.
func envNames(entries []string) []string {
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

// indents every non-empty line by two spaces.
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "  " + line
		}
	}
	return strings.Join(lines, "\n")
}
