// The server-side adapter that gives the engine updater a live view of
// the models an engine runs: which are up, and how to stop them through
// the normal unload machinery.
package server

import (
	"sort"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/router"
)

// engineRuntime implements engineupdate.Runtime over the server's
// local router: running models are read from the router's state and
// stopped through its Unload, so an update through the agent API stops
// models exactly like any other unload — same timeouts, same
// bookkeeping — instead of adding a second teardown path.
type engineRuntime struct {
	cfg   config.Config
	local router.LocalRouter
}

// newEngineRuntime builds the adapter for one server instance. The
// config must be the same one the router was built from, so a model's
// engine reference and the router's model IDs agree.
func newEngineRuntime(cfg config.Config, local router.LocalRouter) engineRuntime {
	return engineRuntime{cfg: cfg, local: local}
}

// modelsUsingEngine returns the config model IDs that launch with the
// named engine, sorted for stable messages.
func (r engineRuntime) modelsUsingEngine(engine string) []string {
	var models []string
	for modelID, model := range r.cfg.Models {
		if model.Engine == engine {
			models = append(models, modelID)
		}
	}
	sort.Strings(models)
	return models
}

func (r engineRuntime) RunningModelsUsingEngine(engine string) ([]string, error) {
	running := r.local.RunningModels()
	var up []string
	for _, modelID := range r.modelsUsingEngine(engine) {
		if _, ok := running[modelID]; ok {
			up = append(up, modelID)
		}
	}
	return up, nil
}

func (r engineRuntime) StopModelsUsingEngine(engine string) ([]string, error) {
	running := r.local.RunningModels()
	var targets []string
	for _, modelID := range r.modelsUsingEngine(engine) {
		if _, ok := running[modelID]; ok {
			targets = append(targets, modelID)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	// Timeout 0 uses each model's configured unloadTimeout.
	r.local.Unload(0, targets...)
	return targets, nil
}
