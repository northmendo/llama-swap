package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"sort"
	"strings"
)

// EngineConfig defines a reusable inference engine build: the binary (plus
// any arguments shared by every model using the engine), a human label, and
// an optional environment. Models select an engine by name with the `engine`
// field and add their own arguments with the `args` field; the effective
// launch command is composed at config load, so the rest of llama-swap only
// ever sees a plain ModelConfig.Cmd.
type EngineConfig struct {
	// Cmd is the base launch command: the engine binary followed by any
	// arguments shared by every model using the engine. Global macros can
	// be used. The model-scoped ${PORT} and ${MODEL_ID} macros belong in the
	// referencing model's args, not here.
	Cmd string `yaml:"cmd"`

	// Label is a short human description of the build, e.g. "beellama b12".
	Label string `yaml:"label"`

	// Env is applied to every model that references the engine. A
	// model-level env entry with the same name wins.
	Env []string `yaml:"env"`
}

func (e *EngineConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawEngineConfig EngineConfig
	defaults := rawEngineConfig{
		Cmd:   "",
		Label: "",
		Env:   []string{},
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*e = EngineConfig(defaults)
	return nil
}

// validateEngines checks every declared engine so a broken engine fails at
// config load (and `llama-swap -validate`) instead of at model start. The
// models' engine references are checked separately, in the model loop.
func validateEngines(engines map[string]EngineConfig) error {
	names := make([]string, 0, len(engines))
	for name := range engines {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("engines: engine names cannot be empty")
		}

		engine := engines[name]
		if strings.TrimSpace(engine.Label) == "" {
			return fmt.Errorf("engines.%s.label must not be empty", name)
		}

		args, err := SanitizeCommand(engine.Cmd)
		if err != nil {
			return fmt.Errorf("engines.%s.cmd must not be empty", name)
		}

		if err := checkEngineBinary(name, args[0]); err != nil {
			return err
		}
	}
	return nil
}

// checkEngineBinary verifies the engine's binary exists and is executable,
// so a missing or non-executable build is reported when the config is
// loaded. Paths containing a separator are checked directly; bare names are
// looked up in PATH like a shell would.
func checkEngineBinary(name, binary string) error {
	_, err := exec.LookPath(binary)
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("engines.%s.cmd: binary %q not found in PATH", name, binary)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("engines.%s.cmd: binary %q does not exist", name, binary)
	}
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("engines.%s.cmd: binary %q is not executable", name, binary)
	}
	return fmt.Errorf("engines.%s.cmd: binary %q is not usable: %w", name, binary, err)
}

// applyModelEngine resolves a model's engine reference into a concrete
// launch command and environment. It is a no-op for models without an
// engine, so plain `cmd` models load exactly as before.
func applyModelEngine(engines map[string]EngineConfig, modelID string, modelConfig *ModelConfig) error {
	if modelConfig.Engine == "" {
		if strings.TrimSpace(modelConfig.Args) != "" {
			return fmt.Errorf("model %s sets args but not engine; args is only used together with engine", modelID)
		}
		return nil
	}

	engine, found := engines[modelConfig.Engine]
	if !found {
		defined := make([]string, 0, len(engines))
		for name := range engines {
			defined = append(defined, name)
		}
		sort.Strings(defined)
		if len(defined) == 0 {
			return fmt.Errorf("model %s references unknown engine %q (no engines are defined)", modelID, modelConfig.Engine)
		}
		return fmt.Errorf("model %s references unknown engine %q (defined engines: %s)", modelID, modelConfig.Engine, strings.Join(defined, ", "))
	}

	if strings.TrimSpace(modelConfig.Cmd) != "" {
		return fmt.Errorf("model %s sets both cmd and engine; a model must use either cmd, or engine with args", modelID)
	}

	modelConfig.Cmd = composeEngineCommand(engine.Cmd, modelConfig.Args)
	modelConfig.Env = mergeEngineEnv(engine.Env, modelConfig.Env)
	return nil
}

// composeEngineCommand joins the engine's base command with the model's own
// arguments segment. A newline keeps multi-line engine commands, comment
// lines and trailing-backslash continuations working through
// SanitizeCommand exactly like a plain cmd. Either segment may be empty.
func composeEngineCommand(engineCmd, args string) string {
	if strings.TrimSpace(args) == "" {
		return engineCmd
	}
	if strings.TrimSpace(engineCmd) == "" {
		return args
	}
	return engineCmd + "\n" + args
}

// mergeEngineEnv merges engine-level env entries into the model's env. A
// model-level entry with the same name wins; within the engine block the
// first entry for a name is kept so the result is deterministic. The model's
// own entries are passed through in their configured order.
func mergeEngineEnv(engineEnv, modelEnv []string) []string {
	if len(engineEnv) == 0 {
		return modelEnv
	}

	overridden := make(map[string]struct{}, len(modelEnv))
	for _, entry := range modelEnv {
		overridden[envEntryName(entry)] = struct{}{}
	}

	merged := make([]string, 0, len(engineEnv)+len(modelEnv))
	seen := make(map[string]struct{}, len(engineEnv))
	for _, entry := range engineEnv {
		name := envEntryName(entry)
		if _, found := overridden[name]; found {
			continue
		}
		if _, found := seen[name]; found {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, entry)
	}
	return append(merged, modelEnv...)
}

// envEntryName returns the NAME half of a NAME=VALUE env entry, or the
// whole entry when it has no '='.
func envEntryName(entry string) string {
	if i := strings.IndexByte(entry, '='); i > 0 {
		return entry[:i]
	}
	return entry
}

// rejectEngineModelScopedMacros rejects ${PORT} and ${MODEL_ID} inside the
// engines section. Engines are shared between models, so these per-model
// macros can never resolve there; the message points at the model's args,
// which is where they belong.
func rejectEngineModelScopedMacros(value any, path string) error {
	switch v := value.(type) {
	case string:
		for _, macroName := range []string{"PORT", "MODEL_ID"} {
			if strings.Contains(v, "${"+macroName+"}") {
				return fmt.Errorf("%s: ${%s} is model-scoped and cannot be used in an engine; put it in the referencing model's args instead", path, macroName)
			}
		}
		return nil
	case map[string]any:
		for key, val := range v {
			if err := rejectEngineModelScopedMacros(val, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for _, val := range v {
			if err := rejectEngineModelScopedMacros(val, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateEngineMacroUses runs the model-scoped macro rejection over the
// raw (post global-substitution) engines section.
func validateEngineMacroUses(raw map[string]any) error {
	engines, ok := raw["engines"].(map[string]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(engines))
	for name := range engines {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := rejectEngineModelScopedMacros(engines[name], "engines."+name); err != nil {
			return err
		}
	}
	return nil
}
