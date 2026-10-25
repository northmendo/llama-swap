// The finite value sets the config UI's pickers are fed from, so a picker
// can never drift from what the server actually validates.
//
//   - GET /agent/v1/config/schema — selector strategies (from
//     internal/config's validator, the one place that decides them) and the
//     proxy presets the peers form offers.
//   - GET /agent/v1/config/env — the NAME-only environment variable list a
//     peer's apiKey can reference. Never a value: this endpoint answers
//     "which macros could I pick?" and "is this macro resolvable right
//     now?", which is the most common way a provider silently dies.
package agentapi

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// envMacroAnywhere matches every ${env.VAR} occurrence inside a string
// (model args, engine env, headers). envMacroRef, by contrast, anchors a
// whole scalar because a key field may hold nothing else.
var envMacroAnywhere = regexp.MustCompile(`\$\{env\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// envNamePattern narrows the process environment to names that plausibly
// hold provider credentials. The full environment is none of the UI's
// business (it would enumerate unrelated infrastructure secrets), so the
// list is deliberately small: config-referenced macros plus key-shaped
// names.
var envNamePattern = regexp.MustCompile(`(?i)(key|token|secret|password|credential)`)

// maxEnvNames bounds the list.
const maxEnvNames = 200

// envNameDTO is one selectable ${env.VAR} name. Set reports whether the
// server environment currently resolves it; the value never appears.
type envNameDTO struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
}

// envNamesResponse is the wire shape of GET /agent/v1/config/env.
type envNamesResponse struct {
	Names []envNameDTO `json:"names"`
}

// handleEnvNames is GET /agent/v1/config/env.
func (a *api) handleEnvNames(w http.ResponseWriter, r *http.Request) {
	raw, err := a.readConfigFile()
	if err != nil {
		raw = nil
	}
	writeJSON(w, r, http.StatusOK, envNamesResponse{Names: envNamesFrom(raw, os.Environ())})
}

// envNamesFrom builds the name list: every ${env.VAR} the raw config
// references (so an unresolvable macro still shows up, flagged) plus every
// key-shaped name in environ. environ is os.Environ() shaped ("NAME=value"),
// which keeps this pure for tests.
func envNamesFrom(raw []byte, environ []string) []envNameDTO {
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		values[name] = value
	}

	seen := make(map[string]bool)
	names := make([]string, 0, len(values))
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}

	for _, match := range envMacroAnywhere.FindAllSubmatch(raw, -1) {
		add(string(match[1]))
	}
	envKeys := make([]string, 0, len(values))
	for name := range values {
		envKeys = append(envKeys, name)
	}
	sort.Strings(envKeys)
	for _, name := range envKeys {
		if len(name) > 64 || !envNamePattern.MatchString(name) {
			continue
		}
		add(name)
	}

	sort.Strings(names)
	if len(names) > maxEnvNames {
		names = names[:maxEnvNames]
	}
	out := make([]envNameDTO, 0, len(names))
	for _, name := range names {
		out = append(out, envNameDTO{Name: name, Set: values[name] != ""})
	}
	return out
}

// proxyPresetDTO is one preset in the peers form's proxy picker.
type proxyPresetDTO struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// proxyPresets are the well-known OpenAI-compatible provider endpoints the
// peers form offers. The peers already in the config are added by the UI
// from GET /agent/v1/config — these are the starting points.
var proxyPresets = []proxyPresetDTO{
	{Value: "https://openrouter.ai/api", Label: "OpenRouter"},
	{Value: "https://api.openai.com", Label: "OpenAI"},
	{Value: "https://api.groq.com/openai", Label: "Groq"},
	{Value: "https://api.together.xyz", Label: "Together AI"},
	{Value: "https://api.deepseek.com", Label: "DeepSeek"},
	{Value: "https://api.mistral.ai", Label: "Mistral"},
	{Value: "https://api.fireworks.ai/inference", Label: "Fireworks AI"},
	{Value: "https://api.x.ai", Label: "xAI"},
	{Value: "http://127.0.0.1:8080", Label: "local llama-server"},
}

// configSchemaResponse is the wire shape of GET /agent/v1/config/schema:
// every finite set the config screen's pickers need.
type configSchemaResponse struct {
	SelectorStrategies      []string         `json:"selectorStrategies"`
	DefaultSelectorStrategy string           `json:"defaultSelectorStrategy"`
	ProxyPresets            []proxyPresetDTO `json:"proxyPresets"`
}

// handleConfigSchema is GET /agent/v1/config/schema.
func (a *api) handleConfigSchema(w http.ResponseWriter, r *http.Request) {
	strategies := append([]string(nil), config.SelectorStrategies...)
	writeJSON(w, r, http.StatusOK, configSchemaResponse{
		SelectorStrategies:      strategies,
		DefaultSelectorStrategy: config.SelectorStrategyFailover,
		ProxyPresets:            append([]proxyPresetDTO(nil), proxyPresets...),
	})
}
