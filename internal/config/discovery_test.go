package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadDiscoveryConfigYAML loads an inline YAML config through the real
// pipeline so discovery validation runs exactly as it does at startup.
func loadDiscoveryConfigYAML(t *testing.T, yamlStr string) (Config, error) {
	t.Helper()
	return LoadConfigFromReader(strings.NewReader(yamlStr))
}

func discoveryYAML(t *testing.T, discoverySection string) string {
	t.Helper()
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)
	return `
engines:
  fastbuild:
    label: "fast build"
    cmd: ` + binary + ` --flash-attn
  bigbuild:
    label: "big build"
    cmd: ` + binary + ` --flash-attn --jinja

models:
  existing:
    engine: fastbuild
    args: |
      --port ${PORT}
      --model /nonexistent.gguf

discovery:
` + discoverySection
}

func TestConfig_Discovery_DefaultsAndAbsence(t *testing.T) {
	t.Run("absent section stays nil", func(t *testing.T) {
		cfg, err := loadDiscoveryConfigYAML(t, `
models:
  placeholder:
    cmd: /bin/true --port ${PORT}
`)
		require.NoError(t, err)
		assert.Nil(t, cfg.Discovery)
	})

	t.Run("present section gets defaults", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := loadDiscoveryConfigYAML(t, `
discovery:
  enabled: true
  folders:
    - `+dir+`
models:
  placeholder:
    cmd: /bin/true --port ${PORT}
`)
		require.NoError(t, err)
		require.NotNil(t, cfg.Discovery)
		assert.True(t, cfg.Discovery.Enabled)
		assert.Equal(t, []string{dir}, cfg.Discovery.Folders)
		assert.Equal(t, DefaultDiscoveryCtxSizeCap, cfg.Discovery.CtxSizeCap, "ctxSizeCap defaults to 32768")
		assert.Empty(t, cfg.Discovery.EngineRules)
	})

	t.Run("disabled by default", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := loadDiscoveryConfigYAML(t, `
discovery:
  folders:
    - `+dir+`
models:
  placeholder:
    cmd: /bin/true --port ${PORT}
`)
		require.NoError(t, err)
		require.NotNil(t, cfg.Discovery)
		assert.False(t, cfg.Discovery.Enabled)
	})
}

func TestConfig_Discovery_ValidationErrors(t *testing.T) {
	validDir := t.TempDir()

	tests := []struct {
		name    string
		section string
		wantErr string
	}{
		{
			name:    "missing folder",
			section: "  folders:\n    - /definitely/not/a/real/dir\n",
			wantErr: "does not exist",
		},
		{
			name:    "empty folder entry",
			section: "  folders:\n    - \"  \"\n",
			wantErr: "must not be empty",
		},
		{
			name:    "folder is a file",
			section: "  folders:\n    - " + filepath.Join(validDir, "afile") + "\n",
			wantErr: "not a directory",
		},
		{
			name:    "ctxSizeCap below 1",
			section: "  folders:\n    - " + validDir + "\n  ctxSizeCap: 0\n",
			wantErr: "ctxSizeCap must be >= 1",
		},
		{
			name:    "empty rule match",
			section: "  folders:\n    - " + validDir + "\n  engine_rules:\n    - match: \"\"\n      engine: fastbuild\n",
			wantErr: "engine_rules[0].match must not be empty",
		},
		{
			name:    "bad rule regex",
			section: "  folders:\n    - " + validDir + "\n  engine_rules:\n    - match: \"([unclosed\"\n      engine: fastbuild\n",
			wantErr: "engine_rules[0].match",
		},
		{
			name:    "unknown rule field",
			section: "  folders:\n    - " + validDir + "\n  engine_rules:\n    - match: \"x\"\n      field: shape\n      engine: fastbuild\n",
			wantErr: `engine_rules[0].field: unknown field "shape"`,
		},
		{
			name:    "unknown rule engine",
			section: "  folders:\n    - " + validDir + "\n  engine_rules:\n    - match: \"x\"\n      engine: nonexistent\n",
			wantErr: `engine_rules[0].engine references unknown engine "nonexistent"`,
		},
		{
			name:    "empty rule engine",
			section: "  folders:\n    - " + validDir + "\n  engine_rules:\n    - match: \"x\"\n      engine: \"\"\n",
			wantErr: "engine_rules[0].engine must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// "folder is a file" needs the file to exist
			require.NoError(t, os.WriteFile(filepath.Join(validDir, "afile"), []byte("x"), 0o644))

			_, err := loadDiscoveryConfigYAML(t, discoveryYAML(t, tt.section))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestConfig_Discovery_ValidSectionLoads(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadDiscoveryConfigYAML(t, `
engines:
  fastbuild:
    label: "fast build"
    cmd: /bin/true --flash-attn
discovery:
  enabled: true
  folders:
    - `+dir+`
  ctxSizeCap: 8192
  engine_rules:
    - match: "MTP"
      engine: fastbuild
    - match: "qwen3"
      field: architecture
      engine: fastbuild
models:
  placeholder:
    cmd: /bin/true --port ${PORT}
`)
	require.NoError(t, err)
	require.NotNil(t, cfg.Discovery)
	assert.Equal(t, 8192, cfg.Discovery.CtxSizeCap)
	require.Len(t, cfg.Discovery.EngineRules, 2)
	assert.Equal(t, "", cfg.Discovery.EngineRules[0].Field, "field defaults to filename")
	assert.Equal(t, "architecture", cfg.Discovery.EngineRules[1].Field)
}

func TestConfig_Discovery_UnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based test")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o755))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, err := loadDiscoveryConfigYAML(t, `
discovery:
  folders:
    - `+locked+`
models:
  placeholder:
    cmd: /bin/true --port ${PORT}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not readable")
}

func TestConfig_Discovery_EngineRulesWithNoEnginesIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := loadDiscoveryConfigYAML(t, `
discovery:
  folders:
    - `+dir+`
  engine_rules:
    - match: "x"
      engine: whatever
models:
  placeholder:
    cmd: /bin/true --port ${PORT}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no engines are defined")
}

func TestConfig_EnginesOrder_CapturedFromYAML(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)
	cfg, err := loadDiscoveryConfigYAML(t, `
engines:
  zeta:
    label: "z"
    cmd: `+binary+`
  alpha:
    label: "a"
    cmd: `+binary+`
models:
  placeholder:
    engine: zeta
    args: --port ${PORT}
`)
	require.NoError(t, err)
	assert.Equal(t, []string{"zeta", "alpha"}, cfg.EnginesOrder,
		"definition order is preserved, not alphabetical")
}

func TestConfig_EnginesOrder_EmptyWithoutEngines(t *testing.T) {
	cfg, err := loadDiscoveryConfigYAML(t, `
models:
  placeholder:
    cmd: /bin/true --port ${PORT}
`)
	require.NoError(t, err)
	assert.Empty(t, cfg.EnginesOrder)
}
