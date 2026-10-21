package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// fakeEngineBinary writes a stub engine binary and returns its path.
func fakeEngineBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-engine")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return path
}

// loadDiscoveryConfig loads an inline YAML config through the real loader.
func loadDiscoveryConfig(t *testing.T, yamlStr string) config.Config {
	t.Helper()
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yamlStr))
	require.NoError(t, err)
	return cfg
}

func discoveryTestConfig(t *testing.T, extra string) (config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	binary := fakeEngineBinary(t, dir)
	modelDir := filepath.Join(dir, "models")
	require.NoError(t, os.MkdirAll(modelDir, 0o755))

	yaml := `
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
  enabled: true
  folders:
    - ` + modelDir + `
` + extra

	return loadDiscoveryConfig(t, yaml), modelDir
}

func TestDiscovery_SelectEngine_Rules(t *testing.T) {
	cfg, modelDir := discoveryTestConfig(t, `
  engine_rules:
    - match: "MTP"
      engine: bigbuild
    - match: "^saluki"
      engine: fastbuild
    - match: "qwen3"
      field: architecture
      engine: bigbuild
`)

	tests := []struct {
		name       string
		path       string
		arch       string
		wantEngine string
		wantSource string
	}{
		{
			name:       "filename rule matches MTP model",
			path:       filepath.Join(modelDir, "Underdog-Saluki-27B-MTP.gguf"),
			arch:       "qwen35",
			wantEngine: "bigbuild",
			wantSource: `rule discovery.engine_rules[0] (filename "MTP" -> bigbuild)`,
		},
		{
			name:       "architecture rule matches",
			path:       filepath.Join(modelDir, "plain.gguf"),
			arch:       "qwen3",
			wantEngine: "bigbuild",
			wantSource: `rule discovery.engine_rules[2] (architecture "qwen3" -> bigbuild)`,
		},
		{
			name:       "no rule match falls back to first defined engine",
			path:       filepath.Join(modelDir, "plain.gguf"),
			arch:       "llama",
			wantEngine: "fastbuild",
			wantSource: "default: first defined engine",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, source, err := SelectEngine(cfg, Metadata{Path: tt.path, Architecture: tt.arch})
			require.NoError(t, err)
			assert.Equal(t, tt.wantEngine, engine)
			assert.Equal(t, tt.wantSource, source)
		})
	}
}

func TestDiscovery_SelectEngine_RulesFirstMatchWins(t *testing.T) {
	cfg, modelDir := discoveryTestConfig(t, `
  engine_rules:
    - match: "saluki"
      engine: fastbuild
    - match: "saluki"
      engine: bigbuild
`)
	engine, source, err := SelectEngine(cfg, Metadata{Path: filepath.Join(modelDir, "saluki-27b.gguf")})
	require.NoError(t, err)
	assert.Equal(t, "fastbuild", engine)
	assert.Contains(t, source, "engine_rules[0]")
}

func TestDiscovery_SelectEngine_NoEngines(t *testing.T) {
	cfg := config.Config{} // no engines defined
	_, _, err := SelectEngine(cfg, Metadata{Path: "x.gguf"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoEngines)
	assert.Contains(t, err.Error(), "no engines are defined")
}

func TestDiscovery_DraftForFile_CtxCap(t *testing.T) {
	tests := []struct {
		name        string
		extra       string
		trainedCtx  int64
		wantCtx     int64
		wantCapped  bool
		wantNoCtxLn bool
	}{
		{
			name:       "default cap 32768 applied to long context",
			extra:      "",
			trainedCtx: 262144,
			wantCtx:    32768,
			wantCapped: true,
		},
		{
			name:       "context below cap is not capped",
			extra:      "",
			trainedCtx: 8192,
			wantCtx:    8192,
			wantCapped: false,
		},
		{
			name:       "configurable cap",
			extra:      "  ctxSizeCap: 16384",
			trainedCtx: 262144,
			wantCtx:    16384,
			wantCapped: true,
		},
		{
			name:        "unknown context omits ctx-size line",
			extra:       "",
			trainedCtx:  0,
			wantCtx:     0,
			wantNoCtxLn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, modelDir := discoveryTestConfig(t, tt.extra)
			path := filepath.Join(modelDir, "Saluki-27B-Q4_K_M.gguf")
			meta := Metadata{Path: path, Architecture: "llama", Type: "model", ContextLength: tt.trainedCtx}

			draft, err := DraftForFile(cfg, meta)
			require.NoError(t, err)

			assert.Equal(t, tt.wantCtx, draft.CtxSize)
			assert.Equal(t, tt.wantCapped, draft.CtxCapped)

			assert.Contains(t, draft.Args, "--port ${PORT}")
			assert.Contains(t, draft.Args, "--model "+path)
			if tt.wantNoCtxLn {
				assert.NotContains(t, draft.Args, "--ctx-size")
			} else {
				assert.Contains(t, draft.Args, "--ctx-size")
			}

			// every draft must pass the real config validation
			require.NoError(t, draft.ValidateAgainst(cfg))
		})
	}
}

func TestDiscovery_DraftForFile_ModelIDFromFilename(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Saluki-27B-Q4_K_M.gguf", "saluki-27b-q4-k-m"},
		{"underdog_saluki_iq2-mix.gguf", "underdog-saluki-iq2-mix"},
		{"Model With Spaces.gguf", "model-with-spaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ModelIDFromFilename(tt.name))
		})
	}
}

func TestDiscovery_DraftForFile_NoEnginesReportsWhy(t *testing.T) {
	cfg := config.Config{}
	_, err := DraftForFile(cfg, Metadata{Path: "x.gguf"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no engines are defined")
}

func TestDiscovery_Draft_StanzaShape(t *testing.T) {
	cfg, modelDir := discoveryTestConfig(t, "")
	path := filepath.Join(modelDir, "Saluki-27B-Q4_K_M.gguf")
	meta := Metadata{Path: path, Architecture: "llama", Type: "model", ContextLength: 8192}

	draft, err := DraftForFile(cfg, meta)
	require.NoError(t, err)

	expected := `  saluki-27b-q4-k-m:
    engine: fastbuild
    args: |
      --port ${PORT}
      --model ` + path + `
      --ctx-size 8192`
	assert.Equal(t, expected, draft.YAML)
}

func TestDiscovery_Draft_ValidateAgainst_UnknownEngine(t *testing.T) {
	cfg, _ := discoveryTestConfig(t, "")
	draft := Draft{ModelID: "x", Engine: "missing-engine", Args: "--port ${PORT}"}
	err := draft.ValidateAgainst(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown engine")
}

func TestDiscovery_Draft_ValidateAgainst_MissingPortMacro(t *testing.T) {
	// The composed launch command must carry --port ${PORT} (or an
	// explicit proxy): the real loader rejects a model whose default
	// proxy references ${PORT} while its command does not.
	cfg, _ := discoveryTestConfig(t, "")
	draft := Draft{ModelID: "x", Engine: "fastbuild", Args: "--model /nope.gguf"}
	err := draft.ValidateAgainst(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy uses ${PORT}")
}
