package config

import (
	"fmt"
	"strings"
)

// AgentAPIConfig is the top-level `agent_api:` section. It gates the
// agent REST API mounted at /agent/v1/ on the running server: a
// bearer-authenticated JSON surface that lets automation drive model
// onboarding (scan folders, draft stanzas, validate and apply config
// changes) without hand-editing YAML. Disabled by default; when
// disabled the server registers no /agent/v1 routes at all and they
// all return 404. See internal/agentapi.
type AgentAPIConfig struct {
	// Enabled turns the /agent/v1/ routes on. Default false.
	Enabled bool `yaml:"enabled"`

	// Key is the bearer token every /agent/v1 request must present as
	// `Authorization: Bearer <key>`. Required when enabled. It is
	// compared in constant time and never rendered into any response,
	// log line or redacted config output. ${env.VAR} macros work here,
	// so the key can stay out of the file entirely.
	Key string `yaml:"key"`
}

// validateAgentAPI checks a present, enabled `agent_api:` section at
// config load (and `llama-swap -validate`) so a broken one fails at
// startup instead of at first request: the key is required and must be
// a single token without whitespace (it travels in an Authorization
// header). A disabled section is accepted with any key value; the key
// is then never read.
func validateAgentAPI(agentAPI *AgentAPIConfig) error {
	if agentAPI == nil || !agentAPI.Enabled {
		return nil
	}

	key := strings.TrimSpace(agentAPI.Key)
	if key == "" {
		return fmt.Errorf("agent_api.key is required when agent_api.enabled is true")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return fmt.Errorf("agent_api.key must not contain whitespace (it is sent as a single bearer token)")
	}
	agentAPI.Key = key
	return nil
}
