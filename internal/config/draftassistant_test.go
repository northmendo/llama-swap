package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// draftAssistantConfig loads a config whose `draft_assistant:` section is
// set through sectionYAML, through the real loader so validation runs.
func draftAssistantConfig(t *testing.T, sectionYAML string) (Config, error) {
	t.Helper()
	scratch := t.TempDir()
	binary := filepath.Join(scratch, "fake-engine")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	yamlText := `healthCheckTimeout: 30
engines:
  testbuild:
    label: "test build"
    cmd: ` + binary + ` --flash-attn

models:
  existing:
    engine: testbuild
    args: |
      --port ${PORT}
      --model /does/not/matter.gguf

draft_assistant:
` + sectionYAML

	cfgPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(yamlText), 0o600))
	return LoadConfig(cfgPath)
}

func TestDraftAssistant_AbsentSectionIsNil(t *testing.T) {
	cfg, err := draftAssistantConfig(t, "  enabled: false\n")
	require.NoError(t, err)

	// The helper always writes a section; do a true absence check.
	scratch := t.TempDir()
	cfgPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("healthCheckTimeout: 30\n"), 0o600))
	cfg, err = LoadConfig(cfgPath)
	require.NoError(t, err)
	assert.Nil(t, cfg.DraftAssistant)
}

func TestDraftAssistant_Defaults(t *testing.T) {
	cfg, err := draftAssistantConfig(t, `  enabled: true
  api_base: https://api.openai.com/v1
  key_env: OPENAI_API_KEY
  model: gpt-4o-mini
`)
	require.NoError(t, err)
	require.NotNil(t, cfg.DraftAssistant)

	section := cfg.DraftAssistant
	assert.True(t, section.Enabled)
	assert.Equal(t, "https://api.openai.com/v1", section.APIBase)
	assert.Equal(t, "OPENAI_API_KEY", section.KeyEnv)
	assert.Equal(t, "gpt-4o-mini", section.Model)
	assert.Equal(t, 60*time.Second, section.Timeout, "timeout must default to 60s")
	assert.Equal(t, 2048, section.MaxTokens, "max_tokens must default to 2048")
	assert.Empty(t, section.Conventions)
}

func TestDraftAssistant_DisabledAcceptsAnything(t *testing.T) {
	// A disabled section is inert: nothing is validated, nothing is read.
	_, err := draftAssistantConfig(t, `  enabled: false
  api_base: "not a url at all"
  timeout: -5s
  max_tokens: 0
`)
	require.NoError(t, err)
}

func TestDraftAssistant_EnabledButMisconfigured(t *testing.T) {
	validTail := `  api_base: https://api.openai.com/v1
  key_env: OPENAI_API_KEY
  model: gpt-4o-mini
`

	cases := []struct {
		name   string
		adjust string
		want   string
	}{
		{
			name:   "missing api_base",
			adjust: "  enabled: true\n  key_env: OPENAI_API_KEY\n  model: m\n",
			want:   "draft_assistant.api_base is required",
		},
		{
			name:   "api_base not a url",
			adjust: "  enabled: true\n  api_base: api.openai.com/v1\n  key_env: OPENAI_API_KEY\n  model: m\n",
			want:   `draft_assistant.api_base: "api.openai.com/v1" must be an absolute http(s) URL`,
		},
		{
			name:   "api_base wrong scheme",
			adjust: "  enabled: true\n  api_base: ftp://api.openai.com/v1\n  key_env: OPENAI_API_KEY\n  model: m\n",
			want:   "must be an absolute http(s) URL",
		},
		{
			name:   "missing key_env",
			adjust: "  enabled: true\n  api_base: https://api.openai.com/v1\n  model: m\n",
			want:   "draft_assistant.key_env is required",
		},
		{
			name:   "key_env invalid name",
			adjust: "  enabled: true\n  api_base: https://api.openai.com/v1\n  key_env: BAD-NAME\n  model: m\n",
			want:   `draft_assistant.key_env: "BAD-NAME" is not a valid environment variable name`,
		},
		{
			name:   "key_env looks like an assignment",
			adjust: "  enabled: true\n  api_base: https://api.openai.com/v1\n  key_env: KEY=value\n  model: m\n",
			want:   "is not a valid environment variable name",
		},
		{
			name:   "missing model",
			adjust: "  enabled: true\n  api_base: https://api.openai.com/v1\n  key_env: OPENAI_API_KEY\n",
			want:   "draft_assistant.model is required",
		},
		{
			name:   "zero timeout",
			adjust: "  enabled: true\n" + validTail + "  timeout: 0s\n",
			want:   "draft_assistant.timeout must be positive",
		},
		{
			name:   "zero max_tokens",
			adjust: "  enabled: true\n" + validTail + "  max_tokens: 0\n",
			want:   "draft_assistant.max_tokens must be >= 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := draftAssistantConfig(t, tc.adjust)
			require.Error(t, err, "an enabled but misconfigured section must fail at load")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestDraftAssistant_TrailingSlashTrimmed(t *testing.T) {
	// api_base keeps the user's shape minus any trailing slash, so the
	// provider URL joins cleanly.
	cfg, err := draftAssistantConfig(t, `  enabled: true
  api_base: http://localhost:8000/v1/
  key_env: OPENAI_API_KEY
  model: local-model
`)
	require.NoError(t, err)
	require.NotNil(t, cfg.DraftAssistant)
	assert.Equal(t, "http://localhost:8000/v1", cfg.DraftAssistant.APIBase)
}
