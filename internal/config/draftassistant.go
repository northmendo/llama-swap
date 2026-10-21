package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DraftAssistantConfig is the top-level `draft_assistant:` section. It
// configures the optional LLM-assisted refinement of discovery drafts
// (`llama-swap discover --refine` and POST /agent/v1/refine): a rule-based
// draft is sent, together with the extracted GGUF metadata, the defined
// engines and the user's conventions, to any OpenAI-compatible
// chat-completions endpoint, and the model's answer must survive the full
// config validation before it replaces the draft. The endpoint can be an
// external provider or llama-swap itself (an always-on local model), so no
// external dependency is required.
//
// The section is tooling-only: the running server's proxy and swap
// behavior never read it, and refinement never writes the config file.
// Disabled by default. The API key is deliberately NOT a field: key_env
// names the environment variable that holds it, and the key is read at
// call time so it never lives in the config file.
type DraftAssistantConfig struct {
	// Enabled turns draft refinement on. Default false; when false the
	// section is inert and `llama-swap discover --refine` refuses to run.
	Enabled bool `yaml:"enabled"`

	// APIBase is the base URL of an OpenAI-compatible /v1 endpoint,
	// e.g. https://api.openai.com/v1 or http://localhost:8000/v1
	// (llama-swap's own proxy, with an always-on model, works). The
	// provider call posts to APIBase + "/chat/completions". Required
	// when enabled.
	APIBase string `yaml:"api_base"`

	// KeyEnv is the NAME of the environment variable holding the
	// provider API key (e.g. OPENAI_API_KEY). The key itself must not
	// be stored in the config file; it is read from the environment at
	// call time and is never logged or echoed. Required when enabled.
	KeyEnv string `yaml:"key_env"`

	// Model is the chat model the refinement requests are sent to
	// (a provider model name, or a llama-swap model ID when api_base
	// points at llama-swap itself). Required when enabled.
	Model string `yaml:"model"`

	// Timeout bounds one refinement request, provider side included.
	// A request that exceeds it falls back to the rule-based draft.
	// Default 60s.
	Timeout time.Duration `yaml:"timeout"`

	// MaxTokens is the chat-completions max_tokens sent with every
	// refinement request. Default 2048.
	MaxTokens int `yaml:"max_tokens"`

	// Conventions is optional free text describing house rules the
	// refined drafts should follow (preferred flags, ctx-size policy,
	// naming, anything the rules cannot decide). It is sent to the
	// model verbatim as part of the prompt.
	Conventions string `yaml:"conventions"`
}

// Defaults for a present `draft_assistant:` section.
const (
	DefaultDraftAssistantTimeout   = 60 * time.Second
	DefaultDraftAssistantMaxTokens = 2048
)

// UnmarshalYAML sets draft_assistant defaults for a present section. An
// absent section leaves the nil pointer in Config untouched.
func (d *DraftAssistantConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawDraftAssistantConfig DraftAssistantConfig
	defaults := rawDraftAssistantConfig{
		Enabled:   false,
		Timeout:   DefaultDraftAssistantTimeout,
		MaxTokens: DefaultDraftAssistantMaxTokens,
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*d = DraftAssistantConfig(defaults)
	return nil
}

// envVarNameRe matches strings usable as environment variable names in
// common shells and in os.Setenv: a letter or underscore, then letters,
// digits or underscores.
var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateDraftAssistant checks a present, enabled `draft_assistant:`
// section at config load (and `llama-swap -validate`) so a broken one
// fails at startup instead of at first refinement: api_base must be an
// absolute http(s) URL, key_env a valid environment variable NAME (the
// key itself is never stored in the config), model must be set, timeout
// positive and max_tokens at least 1. A disabled section is accepted with
// any values; they are then never read. The environment variable named
// by key_env is deliberately NOT checked here: it is read at call time
// so validating a config does not require the key to be present in the
// validating shell.
func validateDraftAssistant(assistant *DraftAssistantConfig) error {
	if assistant == nil || !assistant.Enabled {
		return nil
	}

	apiBase := strings.TrimSpace(assistant.APIBase)
	if apiBase == "" {
		return fmt.Errorf("draft_assistant.api_base is required when draft_assistant.enabled is true")
	}
	parsed, err := url.Parse(apiBase)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("draft_assistant.api_base: %q must be an absolute http(s) URL of an OpenAI-compatible /v1 endpoint (e.g. https://api.openai.com/v1)", apiBase)
	}
	assistant.APIBase = strings.TrimRight(apiBase, "/")

	keyEnv := strings.TrimSpace(assistant.KeyEnv)
	if keyEnv == "" {
		return fmt.Errorf("draft_assistant.key_env is required when draft_assistant.enabled is true: name the environment variable holding the API key (the key itself must not be stored in the config file)")
	}
	if !envVarNameRe.MatchString(keyEnv) {
		return fmt.Errorf("draft_assistant.key_env: %q is not a valid environment variable name", keyEnv)
	}
	assistant.KeyEnv = keyEnv

	if strings.TrimSpace(assistant.Model) == "" {
		return fmt.Errorf("draft_assistant.model is required when draft_assistant.enabled is true")
	}
	assistant.Model = strings.TrimSpace(assistant.Model)

	if assistant.Timeout <= 0 {
		return fmt.Errorf("draft_assistant.timeout must be positive, got %v", assistant.Timeout)
	}
	if assistant.MaxTokens < 1 {
		return fmt.Errorf("draft_assistant.max_tokens must be >= 1, got %d", assistant.MaxTokens)
	}

	return nil
}
