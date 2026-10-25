package agentapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestConfigSchema_SelectorStrategiesComeFromTheValidator(t *testing.T) {
	env := newTestEnv(t, nil)

	var resp configSchemaResponse
	env.getOK(t, http.MethodGet, "/agent/v1/config/schema", nil, &resp)

	assert.Equal(t, config.SelectorStrategies, resp.SelectorStrategies)
	assert.Equal(t, config.SelectorStrategyFailover, resp.DefaultSelectorStrategy)
	assert.NotEmpty(t, resp.ProxyPresets)
}

// TestConfigSchema_EveryAdvertisedStrategyIsAccepted guards the promise the
// picker makes: whatever the schema endpoint lists, the config validator
// takes.
func TestConfigSchema_EveryAdvertisedStrategyIsAccepted(t *testing.T) {
	for _, strategy := range config.SelectorStrategies {
		assert.True(t, config.ValidSelectorStrategy(strategy), "%q is listed but rejected", strategy)
	}
	assert.False(t, config.ValidSelectorStrategy("round-robin"))
}

func TestEnvNames_ConfigMacrosAreListedAndFlagged(t *testing.T) {
	t.Setenv("LLAMA_SWAP_TEST_ENV_KEY", "env-secret-value")
	env := newTestEnv(t, func(yaml string) string {
		return yaml + "\npeers:\n  macro:\n    proxy: http://127.0.0.1:9\n    apiKey: ${env.LLAMA_SWAP_TEST_ENV_KEY}\n    models:\n      - m\n"
	})

	var resp envNamesResponse
	status, payload := env.do(t, http.MethodGet, "/agent/v1/config/env", nil, testKey)
	require.Equal(t, http.StatusOK, status, "body: %s", payload)
	require.NoError(t, json.Unmarshal([]byte(payload), &resp))

	found := map[string]bool{}
	for _, name := range resp.Names {
		found[name.Name] = name.Set
	}
	set, ok := found["LLAMA_SWAP_TEST_ENV_KEY"]
	require.True(t, ok, "the config's macro must be offered: %+v", resp.Names)
	assert.True(t, set)

	// Names only: no value ever rides along.
	assert.NotContains(t, payload, "env-secret-value")
}

func TestEnvNames_UnresolvableMacroIsListedButUnset(t *testing.T) {
	t.Setenv("LLAMA_SWAP_TEST_MISSING_KEY", "present-at-load")
	env := newTestEnv(t, func(yaml string) string {
		return yaml + "\npeers:\n  macro:\n    proxy: http://127.0.0.1:9\n    apiKey: ${env.LLAMA_SWAP_TEST_MISSING_KEY}\n    models:\n      - m\n"
	})
	// The env var was set when the config loaded; it is gone now.
	t.Setenv("LLAMA_SWAP_TEST_MISSING_KEY", "")

	var resp envNamesResponse
	env.getOK(t, http.MethodGet, "/agent/v1/config/env", nil, &resp)

	found := false
	for _, name := range resp.Names {
		if name.Name == "LLAMA_SWAP_TEST_MISSING_KEY" {
			found = true
			assert.False(t, name.Set)
		}
	}
	assert.True(t, found)
}

func TestEnvNamesFrom_OnlyKeyShapedEnvironmentNames(t *testing.T) {
	names := envNamesFrom(nil, []string{
		"HOME=/home/x",
		"PATH=/usr/bin",
		"OPENROUTER_API_KEY=sk-1",
		"HF_TOKEN=hf-1",
		"DB_PASSWORD=hunter2",
		"IMAGE_TAG=latest",
	})
	got := make([]string, 0, len(names))
	for _, name := range names {
		got = append(got, name.Name)
	}
	assert.Equal(t, []string{"DB_PASSWORD", "HF_TOKEN", "OPENROUTER_API_KEY"}, got)
}

func TestEnvNamesFrom_ConfigMacrosComeFirstAndDedupe(t *testing.T) {
	raw := []byte("peers:\n  p:\n    apiKey: ${env.OPENROUTER_API_KEY}\nmodels:\n  m:\n    args: --key ${env.CUSTOM_THING}\n")
	names := envNamesFrom(raw, []string{"OPENROUTER_API_KEY=sk-1"})

	got := map[string]bool{}
	for _, name := range names {
		got[name.Name] = name.Set
	}
	// CUSTOM_THING is not key-shaped but the config references it, so it
	// is offered (unset); the duplicated env name appears once.
	assert.Len(t, names, 2)
	assert.True(t, got["OPENROUTER_API_KEY"])
	assert.False(t, got["CUSTOM_THING"])
}
