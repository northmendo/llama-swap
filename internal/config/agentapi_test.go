package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentAPIConfigYAML builds a config with an agent_api section from the
// given section body.
func agentAPIConfigYAML(section string) string {
	return "models:\n  m1:\n    cmd: /bin/true --port ${PORT}\n" + section
}

func TestAgentAPIConfig_DisabledByDefault(t *testing.T) {
	// No agent_api section at all.
	cfg, err := LoadConfigFromReader(strings.NewReader(agentAPIConfigYAML("")))
	require.NoError(t, err)
	assert.Nil(t, cfg.AgentAPI)
}

func TestAgentAPIConfig_EnabledRequiresKey(t *testing.T) {
	cases := []struct {
		name    string
		section string
		wantErr string
	}{
		{"enabled without key", "agent_api:\n  enabled: true\n", "agent_api.key is required"},
		{"enabled with whitespace-only key", "agent_api:\n  enabled: true\n  key: \"   \"\n", "agent_api.key is required"},
		{"key with spaces", "agent_api:\n  enabled: true\n  key: one two\n", "must not contain whitespace"},
		{"key with newline", "agent_api:\n  enabled: true\n  key: \"line1\\nline2\"\n", "must not contain whitespace"},
		{"disabled with no key is fine", "agent_api:\n  enabled: false\n", ""},
		{"enabled with key", "agent_api:\n  enabled: true\n  key: good-key\n", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader(agentAPIConfigYAML(c.section)))
			if c.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

func TestAgentAPIConfig_KeyIsTrimmed(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(
		agentAPIConfigYAML("agent_api:\n  enabled: true\n  key: \" padded-key \"\n")))
	require.NoError(t, err)
	require.NotNil(t, cfg.AgentAPI)
	assert.True(t, cfg.AgentAPI.Enabled)
	assert.Equal(t, "padded-key", cfg.AgentAPI.Key)
}

func TestAgentAPIConfig_KeySupportsEnvMacros(t *testing.T) {
	t.Setenv("LLAMA_SWAP_TEST_AGENT_KEY", "from-env-key")

	cfg, err := LoadConfigFromReader(strings.NewReader(
		agentAPIConfigYAML("agent_api:\n  enabled: true\n  key: ${env.LLAMA_SWAP_TEST_AGENT_KEY}\n")))
	require.NoError(t, err)
	require.NotNil(t, cfg.AgentAPI)
	assert.Equal(t, "from-env-key", cfg.AgentAPI.Key)
}

func TestAgentAPIConfig_RedactedYAMLNeverShowsKey(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(
		agentAPIConfigYAML("agent_api:\n  enabled: true\n  key: super-secret-bearer-token\n")))
	require.NoError(t, err)

	out, found, err := cfg.RedactedYAML("")
	require.NoError(t, err)
	require.True(t, found)

	assert.NotContains(t, out, "super-secret-bearer-token",
		"RedactedYAML must never render agent_api.key")
	assert.Contains(t, out, "[REDACTED]",
		"the agent_api section should be redacted wholesale")
}
