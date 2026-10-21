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

// writeEngineBinary creates a fake engine binary in dir with the given mode
// and returns its path.
func writeEngineBinary(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		path += ".exe"
		content = "exit 0\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

// engineTestConfig wraps LoadConfigFromReader for inline YAML configs.
func loadEngineConfig(t *testing.T, yamlStr string) (Config, error) {
	t.Helper()
	return LoadConfigFromReader(strings.NewReader(yamlStr))
}

func TestConfig_Engines_ComposeCommand(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)

	tests := []struct {
		name    string
		yaml    string
		wantCmd string
		wantTok []string
	}{
		{
			name: "engine with model args",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: ` + binary + ` --flash-attn
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
    args: --model /models/saluki.gguf --ctx-size 8192
`,
			wantCmd: binary + " --flash-attn\n--model /models/saluki.gguf --ctx-size 8192",
			wantTok: []string{binary, "--flash-attn", "--model", "/models/saluki.gguf", "--ctx-size", "8192"},
		},
		{
			name: "engine without args runs the engine command as-is",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: ` + binary + ` --flash-attn
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
`,
			wantCmd: binary + " --flash-attn",
			wantTok: []string{binary, "--flash-attn"},
		},
		{
			name: "multi-line engine cmd with comments and continuations",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: |
      ` + binary + `
      --flash-attn
      # a comment in the engine command
      --no-webui \
        --threads 4
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
    args: |
      --model /models/saluki.gguf
`,
			wantTok: []string{binary, "--flash-attn", "--no-webui", "--threads", "4", "--model", "/models/saluki.gguf"},
		},
		{
			name: "global macros expand in engine cmd",
			yaml: `
macros:
  "threads": 4
engines:
  beellama:
    label: "beellama b12"
    cmd: |
      ` + binary + `
      --threads ${threads}
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
    args: --model /models/saluki.gguf
`,
			wantTok: []string{binary, "--threads", "4", "--model", "/models/saluki.gguf"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadEngineConfig(t, tt.yaml)
			require.NoError(t, err)
			require.Contains(t, cfg.Models, "saluki")

			mc := cfg.Models["saluki"]
			if tt.wantCmd != "" {
				assert.Equal(t, tt.wantCmd, mc.Cmd)
			}
			tokens, err := mc.SanitizedCommand()
			require.NoError(t, err)
			assert.Equal(t, tt.wantTok, tokens)

			// The model keeps its reference for introspection.
			assert.Equal(t, "beellama", mc.Engine)
			assert.Equal(t, "beellama b12", cfg.Engines["beellama"].Label)
		})
	}
}

func TestConfig_Engines_PortAllocation(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)

	t.Run("PORT in args is allocated and substituted", func(t *testing.T) {
		cfg, err := loadEngineConfig(t, `
engines:
  beellama:
    label: "beellama b12"
    cmd: `+binary+` --flash-attn
models:
  saluki:
    engine: beellama
    args: --port ${PORT} --model /models/saluki.gguf
`)
		require.NoError(t, err)
		mc := cfg.Models["saluki"]
		assert.Contains(t, mc.Cmd, "--port 5800")
		assert.Equal(t, "http://localhost:5800", mc.Proxy)
	})

	t.Run("engine model without PORT must set an explicit proxy", func(t *testing.T) {
		_, err := loadEngineConfig(t, `
engines:
  beellama:
    label: "beellama b12"
    cmd: `+binary+` --flash-attn
models:
  saluki:
    engine: beellama
    args: --model /models/saluki.gguf
`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy uses ${PORT} but cmd does not")
	})
}

func TestConfig_Engines_EnvMerge(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)

	cfg, err := loadEngineConfig(t, `
engines:
  beellama:
    label: "beellama b12"
    cmd: `+binary+` --flash-attn
    env:
      - "AAA=engine"
      - "BBB=engine"
      - "CCC=engine"
      - "AAA=engine-duplicate"
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
    env:
      - "BBB=model"
      - "DDD=model"
`)
	require.NoError(t, err)
	mc := cfg.Models["saluki"]
	assert.Equal(t, []string{"AAA=engine", "CCC=engine", "BBB=model", "DDD=model"}, mc.Env)
}

func TestConfig_Engines_MergeHelpers(t *testing.T) {
	t.Run("mergeEngineEnv", func(t *testing.T) {
		tests := []struct {
			name      string
			engineEnv []string
			modelEnv  []string
			want      []string
		}{
			{
				name:      "no engine env",
				engineEnv: nil,
				modelEnv:  []string{"A=1"},
				want:      []string{"A=1"},
			},
			{
				name:      "no model env",
				engineEnv: []string{"A=1"},
				modelEnv:  nil,
				want:      []string{"A=1"},
			},
			{
				name:      "model wins on collision",
				engineEnv: []string{"A=engine", "B=engine"},
				modelEnv:  []string{"A=model"},
				want:      []string{"B=engine", "A=model"},
			},
			{
				name:      "first engine entry wins within engine",
				engineEnv: []string{"A=one", "A=two"},
				modelEnv:  nil,
				want:      []string{"A=one"},
			},
			{
				name:      "entry without equals is a whole-name key",
				engineEnv: []string{"FLAG"},
				modelEnv:  []string{"FLAG=1"},
				want:      []string{"FLAG=1"},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, mergeEngineEnv(tt.engineEnv, tt.modelEnv))
			})
		}
	})

	t.Run("composeEngineCommand", func(t *testing.T) {
		assert.Equal(t, "engine-cmd", composeEngineCommand("engine-cmd", ""))
		assert.Equal(t, "engine-cmd", composeEngineCommand("engine-cmd", "  "))
		assert.Equal(t, "args", composeEngineCommand("", "args"))
		assert.Equal(t, "engine-cmd\nargs", composeEngineCommand("engine-cmd", "args"))
	})
}

func TestConfig_Engines_ValidationErrors(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)

	tests := []struct {
		name       string
		yaml       string
		wantErrSub string
		posixOnly  bool
	}{
		{
			name: "unknown engine reference",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: ` + binary + `
models:
  saluki:
    engine: nope
    proxy: "http://localhost:1234"
`,
			wantErrSub: `references unknown engine "nope" (defined engines: beellama)`,
		},
		{
			name: "unknown engine reference with no engines defined",
			yaml: `
models:
  saluki:
    engine: nope
    proxy: "http://localhost:1234"
`,
			wantErrSub: `references unknown engine "nope" (no engines are defined)`,
		},
		{
			name: "both cmd and engine",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: ` + binary + `
models:
  saluki:
    engine: beellama
    cmd: some-command --model x
    proxy: "http://localhost:1234"
`,
			wantErrSub: "sets both cmd and engine",
		},
		{
			name: "args without engine",
			yaml: `
models:
  saluki:
    cmd: ` + binary + `
    proxy: "http://localhost:1234"
    args: --extra-flag
`,
			wantErrSub: "sets args but not engine",
		},
		{
			name: "missing engine label",
			yaml: `
engines:
  beellama:
    cmd: ` + binary + `
models:
  saluki:
    cmd: ` + binary + `
    proxy: "http://localhost:1234"
`,
			wantErrSub: "engines.beellama.label must not be empty",
		},
		{
			name: "empty engine cmd",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: |
      # only a comment
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
`,
			wantErrSub: "engines.beellama.cmd must not be empty",
		},
		{
			name: "missing binary",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: /does/not/exist/llama-server
models:
  saluki:
    cmd: ` + binary + `
    proxy: "http://localhost:1234"
`,
			wantErrSub: "does not exist",
		},
		{
			name: "non-executable binary",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: NON_EXECUTABLE_PATH --flash-attn
models:
  saluki:
    cmd: ` + binary + `
    proxy: "http://localhost:1234"
`,
			wantErrSub: "is not executable",
			posixOnly:  true,
		},
		{
			name: "bare binary name not in PATH",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: definitely-not-a-real-binary-xyz --flash-attn
models:
  saluki:
    cmd: ` + binary + `
    proxy: "http://localhost:1234"
`,
			wantErrSub: "not found in PATH",
		},
		{
			name: "PORT macro in engine cmd",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: ` + binary + ` --port ${PORT}
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
`,
			wantErrSub: "engines.beellama.cmd: ${PORT} is model-scoped and cannot be used in an engine",
		},
		{
			name: "MODEL_ID macro in engine cmd",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: ` + binary + ` --model-id ${MODEL_ID}
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
`,
			wantErrSub: "engines.beellama.cmd: ${MODEL_ID} is model-scoped and cannot be used in an engine",
		},
		{
			name: "unknown macro in engine cmd",
			yaml: `
engines:
  beellama:
    label: "beellama b12"
    cmd: ` + binary + ` --threads ${threads}
models:
  saluki:
    cmd: ` + binary + `
    proxy: "http://localhost:1234"
`,
			wantErrSub: "engines.beellama.cmd: unknown macro '${threads}'",
		},
		{
			name: "empty engine name",
			yaml: `
engines:
  "":
    label: "empty"
    cmd: ` + binary + `
models:
  saluki:
    cmd: ` + binary + `
    proxy: "http://localhost:1234"
`,
			wantErrSub: "engine names cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.posixOnly && runtime.GOOS == "windows" {
				t.Skip("exec-bit semantics differ on windows")
			}
			yamlStr := tt.yaml
			if tt.name == "non-executable binary" {
				// Create the file but strip its execute bit.
				nonExec := writeEngineBinary(t, t.TempDir(), "llama-server", 0o644)
				yamlStr = strings.ReplaceAll(tt.yaml, "NON_EXECUTABLE_PATH", nonExec)
			}
			_, err := loadEngineConfig(t, yamlStr)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrSub)
		})
	}
}

func TestConfig_Engines_BackwardCompatible(t *testing.T) {
	// A config in the pre-engines shape must load with byte-identical model
	// commands and no engine machinery touching anything.
	yaml := `
models:
  model1:
    cmd: /usr/bin/fake-cmd --port ${PORT}
    proxy: "http://localhost:8081"
  model2:
    cmd: /usr/bin/fake-cmd --port ${PORT}
    env:
      - "AAA=1"
`
	cfg, err := loadEngineConfig(t, yaml)
	require.NoError(t, err)
	assert.Nil(t, cfg.Engines)
	assert.Equal(t, "/usr/bin/fake-cmd --port 5800", cfg.Models["model1"].Cmd)
	assert.Equal(t, "/usr/bin/fake-cmd --port 5801", cfg.Models["model2"].Cmd)
	assert.Equal(t, []string{"AAA=1"}, cfg.Models["model2"].Env)
	assert.Empty(t, cfg.Models["model1"].Engine)
	assert.Empty(t, cfg.Models["model1"].Args)
}

func TestConfig_Engines_PlainAndEngineModelsCoexist(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)

	cfg, err := loadEngineConfig(t, `
engines:
  beellama:
    label: "beellama b12"
    cmd: `+binary+` --flash-attn
models:
  aaa-plain:
    cmd: /usr/bin/fake-cmd --port ${PORT}
  zzz-engine:
    engine: beellama
    args: --port ${PORT} --model /models/x.gguf
`)
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/fake-cmd --port 5800", cfg.Models["aaa-plain"].Cmd)
	assert.Equal(t, binary+" --flash-attn\n--port 5801 --model /models/x.gguf", cfg.Models["zzz-engine"].Cmd)
	assert.Equal(t, "http://localhost:5800", cfg.Models["aaa-plain"].Proxy)
	assert.Equal(t, "http://localhost:5801", cfg.Models["zzz-engine"].Proxy)
}

func TestConfig_Engines_ConfigDirDuplicateEngineIsRejected(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("engines:\n  beellama:\n    label: \"one\"\n    cmd: /bin/true\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("engines:\n  beellama:\n    label: \"two\"\n    cmd: /bin/true\n"), 0o644))

	_, err := LoadConfigSources("", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "engines")
}

func TestConfig_Engines_RedactedYAMLRedactsEngineEnv(t *testing.T) {
	binary := writeEngineBinary(t, t.TempDir(), "llama-server", 0o755)

	cfg, err := loadEngineConfig(t, `
engines:
  beellama:
    label: "beellama b12"
    cmd: `+binary+` --flash-attn
    env:
      - "HF_TOKEN=hf_AAAAAAAAAAAAAAAAAAAA"
models:
  saluki:
    engine: beellama
    proxy: "http://localhost:1234"
`)
	require.NoError(t, err)

	out, found, err := cfg.RedactedYAML("engines")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, out, RedactedPlaceholder)
	assert.NotContains(t, out, "hf_AAAAAAAAAAAAAAAAAAAA")
}
