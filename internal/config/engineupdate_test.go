package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadEngineUpdateConfig loads a config whose single engine has the
// given update block, returning the parse/validation error verbatim.
func loadEngineUpdateConfig(t *testing.T, updateBlock string) error {
	t.Helper()
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)
	yaml := `
engines:
  testengine:
    label: "test engine"
    cmd: ` + binary + `
` + updateBlock + `
`
	_, err := LoadConfigFromReader(strings.NewReader(yaml))
	return err
}

func TestConfig_EngineUpdate_GitHubValid(t *testing.T) {
	err := loadEngineUpdateConfig(t, `
    update:
      source: github
      repo: "beellama/beellama"
      asset_regex: "llama-server-.*-linux-amd64"
`)
	require.NoError(t, err)
}

func TestConfig_EngineUpdate_GitHubDefaultsToAutoSHA(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)
	yaml := `
engines:
  testengine:
    label: "test engine"
    cmd: ` + binary + `
    update:
      source: github
      repo: "beellama/beellama"
      asset_regex: "llama-server-.*"
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)

	update := cfg.Engines["testengine"].Update
	require.NotNil(t, update)
	assert.Equal(t, "", strings.TrimSpace(update.SHA256), "empty sha256 means auto for github sources")
	assert.Equal(t, "https://api.github.com", update.GitHubAPIBase())
}

func TestConfig_EngineUpdate_GitHubEnterpriseAPIBase(t *testing.T) {
	err := loadEngineUpdateConfig(t, `
    update:
      source: github
      repo: "corp/beellama"
      asset_regex: "llama-server-.*"
      api_base: "https://github.example.com/api/v3"
`)
	require.NoError(t, err)

	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)
	yaml := `
engines:
  e:
    label: "e"
    cmd: ` + binary + `
    update:
      source: github
      repo: "corp/beellama"
      asset_regex: "x"
      api_base: "https://github.example.com/api/v3"
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	assert.Equal(t, "https://github.example.com/api/v3", cfg.Engines["e"].Update.GitHubAPIBase())
}

func TestConfig_EngineUpdate_URLRequiresPinnedSHA(t *testing.T) {
	err := loadEngineUpdateConfig(t, `
    update:
      source: url
      url: "https://example.com/dl/llama-server"
      sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`)
	require.NoError(t, err)

	// Missing checksum: rejected, direct URLs expose no digest to trust.
	err = loadEngineUpdateConfig(t, `
    update:
      source: url
      url: "https://example.com/dl/llama-server"
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sha256 is required for url sources")

	// "auto" is a GitHub-only concept.
	err = loadEngineUpdateConfig(t, `
    update:
      source: url
      url: "https://example.com/dl/llama-server"
      sha256: "auto"
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"auto" is not allowed for url sources`)
}

func TestConfig_EngineUpdate_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		want string
		yaml string
	}{
		{
			name: "unknown source kind",
			want: `unknown source kind "gitlab"`,
			yaml: `
    update:
      source: gitlab
      repo: "a/b"
`,
		},
		{
			name: "missing source",
			want: "update.source is required",
			yaml: `
    update:
      repo: "a/b"
      asset_regex: "x"
`,
		},
		{
			name: "bad repo slug",
			want: `is not a GitHub owner/name repository`,
			yaml: `
    update:
      source: github
      repo: "https://github.com/a/b"
      asset_regex: "x"
`,
		},
		{
			name: "github without asset regex",
			want: "asset_regex is required for github sources",
			yaml: `
    update:
      source: github
      repo: "a/b"
`,
		},
		{
			name: "asset regex does not compile",
			want: "asset_regex",
			yaml: `
    update:
      source: github
      repo: "a/b"
      asset_regex: "*llama["
`,
		},
		{
			name: "tag regex does not compile",
			want: "tag_regex",
			yaml: `
    update:
      source: github
      repo: "a/b"
      asset_regex: "x"
      tag_regex: "v(.*"
`,
		},
		{
			name: "pinned sha256 not hex",
			want: "not valid hex",
			yaml: `
    update:
      source: github
      repo: "a/b"
      asset_regex: "x"
      sha256: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
`,
		},
		{
			name: "pinned sha256 wrong length",
			want: "expected 64 hex characters",
			yaml: `
    update:
      source: github
      repo: "a/b"
      asset_regex: "x"
      sha256: "abcd"
`,
		},
		{
			name: "url source without url",
			want: `is not a valid http(s) URL`,
			yaml: `
    update:
      source: url
      sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`,
		},
		{
			name: "url source with non-http scheme",
			want: `is not a valid http(s) URL`,
			yaml: `
    update:
      source: url
      url: "ftp://example.com/llama-server"
      sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`,
		},
		{
			name: "github api_base not a url",
			want: `api_base: "github.example" is not a base http(s) URL`,
			yaml: `
    update:
      source: github
      repo: "a/b"
      asset_regex: "x"
      api_base: "github.example"
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadEngineUpdateConfig(t, tt.yaml)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestConfig_EngineUpdate_VersionSeed(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)
	yaml := `
engines:
  testengine:
    label: "test engine"
    cmd: ` + binary + `
    update:
      source: github
      repo: "a/b"
      asset_regex: "x"
      version: "b12"
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	assert.Equal(t, "b12", cfg.Engines["testengine"].Update.Version)
}
