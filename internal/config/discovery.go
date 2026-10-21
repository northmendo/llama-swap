package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// DiscoveryEngineRule maps discovered model files to a named engine. Rules
// are evaluated in order and the first match wins; when no rule matches, the
// draft falls back to the first defined engine.
type DiscoveryEngineRule struct {
	// Match is a regular expression (Go syntax) tested, unanchored, against
	// the value selected by Field. Required.
	Match string `yaml:"match"`

	// Field selects what Match is tested against: "filename" (the model
	// file's base name; the default) or "architecture" (the GGUF
	// general.architecture value, e.g. "qwen3").
	Field string `yaml:"field"`

	// Engine is the name of the engines section entry the draft will
	// reference. Required; must be a defined engine.
	Engine string `yaml:"engine"`
}

// DiscoveryConfig is the top-level `discovery:` section. It configures
// `llama-swap discover`, which scans folders for .gguf files, extracts GGUF
// metadata and prints draft model stanzas. It never writes the config file
// and never starts a server; runtime/swap behavior is unchanged.
type DiscoveryConfig struct {
	// Enabled is a switch for tooling that wants a single flag to turn
	// discovery on. The `llama-swap discover` subcommand works regardless;
	// nothing in the server reads this yet.
	Enabled bool `yaml:"enabled"`

	// Folders lists directories scanned (non-recursively per folder is NOT
	// the rule: each folder is walked depth-first) for .gguf files. Each
	// must exist and be readable; checked when the config is loaded and by
	// `llama-swap -validate`.
	Folders []string `yaml:"folders"`

	// CtxSizeCap caps the drafted --ctx-size at min(trained context, cap).
	// Must be >= 1. Default 32768.
	CtxSizeCap int `yaml:"ctxSizeCap"`

	// EngineRules is an ordered list of regex-to-engine mappings evaluated
	// first-match-wins before falling back to the first defined engine.
	EngineRules []DiscoveryEngineRule `yaml:"engine_rules"`
}

// DefaultDiscoveryCtxSizeCap is the drafted --ctx-size cap when ctxSizeCap
// is not set.
const DefaultDiscoveryCtxSizeCap = 32768

// UnmarshalYAML sets discovery defaults for a present `discovery:` section.
// An absent section leaves the nil pointer in Config untouched.
func (d *DiscoveryConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawDiscoveryConfig DiscoveryConfig
	defaults := rawDiscoveryConfig{
		Enabled:     false,
		Folders:     []string{},
		CtxSizeCap:  DefaultDiscoveryCtxSizeCap,
		EngineRules: []DiscoveryEngineRule{},
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*d = DiscoveryConfig(defaults)
	return nil
}

// validateDiscovery checks a present `discovery:` section so a broken one
// fails at config load (and `llama-swap -validate`) instead of at discover
// time: folders must exist and be readable, ctxSizeCap must be positive,
// rule regexes must compile, rule fields must be known, and rule engines
// must be defined in the engines section.
func validateDiscovery(discovery *DiscoveryConfig, engines map[string]EngineConfig) error {
	if discovery == nil {
		return nil
	}

	if discovery.CtxSizeCap < 1 {
		return fmt.Errorf("discovery.ctxSizeCap must be >= 1, got %d", discovery.CtxSizeCap)
	}

	for i, folder := range discovery.Folders {
		folder = strings.TrimSpace(folder)
		if folder == "" {
			return fmt.Errorf("discovery.folders[%d] must not be empty", i)
		}

		info, err := os.Stat(folder)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("discovery.folders[%d]: %s does not exist", i, folder)
			}
			return fmt.Errorf("discovery.folders[%d]: %w", i, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("discovery.folders[%d]: %s is not a directory", i, folder)
		}

		// Readability probe: listing the directory needs the same
		// permission the discover scan does. Works cross-platform.
		if _, err := os.ReadDir(folder); err != nil {
			return fmt.Errorf("discovery.folders[%d]: %s is not readable: %w", i, folder, err)
		}
	}

	for i, rule := range discovery.EngineRules {
		if strings.TrimSpace(rule.Match) == "" {
			return fmt.Errorf("discovery.engine_rules[%d].match must not be empty", i)
		}
		if _, err := regexp.Compile(rule.Match); err != nil {
			return fmt.Errorf("discovery.engine_rules[%d].match: %w", i, err)
		}
		switch rule.Field {
		case "", "filename", "architecture":
		default:
			return fmt.Errorf("discovery.engine_rules[%d].field: unknown field %q (valid: filename, architecture)", i, rule.Field)
		}
		if strings.TrimSpace(rule.Engine) == "" {
			return fmt.Errorf("discovery.engine_rules[%d].engine must not be empty", i)
		}
		if _, found := engines[rule.Engine]; !found {
			defined := make([]string, 0, len(engines))
			for name := range engines {
				defined = append(defined, name)
			}
			if len(defined) == 0 {
				return fmt.Errorf("discovery.engine_rules[%d].engine references unknown engine %q (no engines are defined)", i, rule.Engine)
			}
			return fmt.Errorf("discovery.engine_rules[%d].engine references unknown engine %q (defined engines: %s)", i, rule.Engine, strings.Join(defined, ", "))
		}
	}

	return nil
}
