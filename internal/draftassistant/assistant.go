// Package draftassistant implements the optional, disabled-by-default
// LLM-assisted refinement of discovery drafts (the `draft_assistant:`
// config section, internal/config/draftassistant.go).
//
// A refinement sends the extracted GGUF metadata, the rule-based draft,
// the defined engines and the user's conventions to any
// OpenAI-compatible chat-completions endpoint — an external provider or
// llama-swap itself — and asks for a better model stanza. The answer is
// never trusted: it must parse as YAML, match the model-stanza schema and
// pass the SAME validation as any hand-written config (unknown engine,
// bad flags, missing ${PORT} all reject). Any failure — parse, schema,
// validation, provider error or timeout — falls back to the rule-based
// draft with the reason reported. Refinement only ever produces a draft:
// nothing is applied, nothing is written, the config file is never
// touched.
//
// The provider API key is read from the environment variable named by
// draft_assistant.key_env at call time; it is never stored in the config
// file, never logged and never echoed in any output.
package draftassistant

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/discovery"
)

// ErrMisconfigured marks configuration-class problems: the section is
// not enabled, or the environment variable named by key_env is not set.
// These are surfaced to the caller as errors (not silent fallbacks)
// because fixing them is on the operator, not on the model.
var ErrMisconfigured = errors.New("draft assistant is misconfigured")

// RefineOutcome is the result of one refinement attempt. Draft is always
// usable: it is the refined stanza when Refined is true, and the
// rule-based draft unchanged when it is false, in which case Reason
// explains what went wrong.
type RefineOutcome struct {
	Draft discovery.Draft

	// Refined is true when the LLM's answer passed every gate and is
	// what Draft carries.
	Refined bool

	// Reason explains a fallback (Refined false); empty when refined.
	Reason string
}

// Refine runs one LLM-assisted refinement of a rule-based draft.
//
// Config-class problems (section not enabled, key environment variable
// not set) return an error wrapping ErrMisconfigured and no outcome.
// Everything else — provider HTTP errors, timeouts, unparseable or
// schema-invalid answers, stanzas that fail config validation — returns
// an outcome with the rule-based draft and a human-readable reason:
// refinement is an enhancement, so it degrades instead of failing.
func Refine(ctx context.Context, cfg config.Config, meta discovery.Metadata, rule discovery.Draft) (RefineOutcome, error) {
	section := cfg.DraftAssistant
	if section == nil || !section.Enabled {
		return RefineOutcome{}, fmt.Errorf("%w: the draft_assistant section is absent or not enabled", ErrMisconfigured)
	}

	apiKey, ok := os.LookupEnv(section.KeyEnv)
	if !ok || apiKey == "" {
		return RefineOutcome{}, fmt.Errorf("%w: environment variable %s (draft_assistant.key_env) is not set", ErrMisconfigured, section.KeyEnv)
	}

	fallback := func(reason string) (RefineOutcome, error) {
		return RefineOutcome{Draft: rule, Refined: false, Reason: redact(reason, apiKey)}, nil
	}

	system, user := buildPrompts(cfg, meta, rule)

	callCtx, cancel := context.WithTimeout(ctx, section.Timeout)
	defer cancel()

	content, err := chatCompletion(callCtx, *section, apiKey, system, user)
	if err != nil {
		return fallback("provider call failed: " + err.Error())
	}

	modelID, model, stanzaYAML, err := parseStanza(content, rule.ModelID)
	if err != nil {
		return fallback("the LLM answer was rejected: " + err.Error())
	}

	if err := validateStanza(cfg, modelID, model); err != nil {
		return fallback("the refined stanza failed config validation: " + err.Error())
	}

	ctxSize := ctxSizeFromArgs(model.Args)
	draft := discovery.Draft{
		ModelID:      modelID,
		Engine:       model.Engine,
		EngineSource: fmt.Sprintf("draft_assistant refinement (model %s)", section.Model),
		Args:         model.Args,
		CtxSize:      ctxSize,
		CtxCapped:    meta.ContextLength > 0 && ctxSize > 0 && ctxSize < meta.ContextLength,
		YAML:         stanzaYAML,
	}
	return RefineOutcome{Draft: draft, Refined: true}, nil
}
