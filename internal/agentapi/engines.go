// Engine update routes of the agent API: the check-results view and the
// approved update, plus the two small helpers (`running`, `stop`) the
// `llama-swap engines` CLI uses to verify no models are up before it
// swaps a binary from outside the server process.
package agentapi

import (
	"net/http"
	"sort"
	"time"

	"github.com/mostlygeek/llama-swap/internal/engineupdate"
)

// engineUpdateDTO is one engine's row of GET /agent/v1/engines/updates.
type engineUpdateDTO struct {
	Name            string              `json:"name"`
	Label           string              `json:"label"`
	HasUpdateSource bool                `json:"hasUpdateSource"`
	InstalledVer    string              `json:"installedVersion,omitempty"`
	Source          string              `json:"source,omitempty"`
	LastCheckAt     string              `json:"lastCheckAt,omitempty"`
	LastUpdateAt    string              `json:"lastUpdateAt,omitempty"`
	Available       *updateAvailableDTO `json:"updateAvailable,omitempty"`
}

type updateAvailableDTO struct {
	Tag       string `json:"tag,omitempty"`
	URL       string `json:"url,omitempty"`
	CheckedAt string `json:"checkedAt"`
}

// handleEngineUpdates answers check results from the updater's state
// file: what is installed per engine and what a check found. It never
// touches the network — run `llama-swap engines check` or POST the
// update route with approved=false to refresh it.
func (a *api) handleEngineUpdates(w http.ResponseWriter, r *http.Request) {
	if a.engineStatePath() == "" {
		writeError(w, r, http.StatusServiceUnavailable, "engine update state path is not configured on this server")
		return
	}
	manager := a.engineManager()
	entries, err := manager.List()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "reading engine update state: "+err.Error())
		return
	}
	state, err := engineupdate.LoadState(a.engineStatePath())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "reading engine update state: "+err.Error())
		return
	}

	dtos := make([]engineUpdateDTO, 0, len(entries))
	for _, entry := range entries {
		dto := engineUpdateDTO{
			Name:            entry.Name,
			Label:           entry.Label,
			HasUpdateSource: entry.HasUpdateSource,
			InstalledVer:    entry.InstalledVersion,
			LastCheckAt:     formatTime(entry.LastCheckAt),
			LastUpdateAt:    formatTime(entry.LastUpdateAt),
		}
		engine := a.cfg.Engines[entry.Name]
		if engine.Update != nil {
			dto.Source = engine.Update.Source
		}
		if st := state.Engines[entry.Name]; st != nil && st.Available != nil {
			dto.Available = &updateAvailableDTO{
				Tag:       st.Available.Tag,
				URL:       st.Available.URL,
				CheckedAt: formatTime(st.Available.CheckedAt),
			}
		}
		dtos = append(dtos, dto)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"engines": dtos})
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleEngineRunning lists the running models that use one engine.
// GET /agent/v1/engines/{name}/running.
func (a *api) handleEngineRunning(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := a.cfg.Engines[name]; !ok {
		writeError(w, r, http.StatusNotFound, "unknown engine "+name)
		return
	}

	running, err := a.engineRuntime().RunningModelsUsingEngine(name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "listing running models: "+err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"engine": name, "runningModels": running})
}

// handleEngineStop stops the running models that use one engine through
// the normal unload machinery. POST /agent/v1/engines/{name}/stop.
func (a *api) handleEngineStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := a.cfg.Engines[name]; !ok {
		writeError(w, r, http.StatusNotFound, "unknown engine "+name)
		return
	}

	stopped, err := a.engineRuntime().StopModelsUsingEngine(name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "stopping running models: "+err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"engine": name, "stopped": stopped})
}

// engineUpdateRequest is the body of POST /agent/v1/engines/{name}/update.
type engineUpdateRequest struct {
	// Approved must be true for any actual swap. A request without it
	// (or with false) is check-only: the source is queried and the
	// state refreshed, but nothing is downloaded or replaced.
	Approved bool `json:"approved"`

	// Force stops models using the engine that are currently running
	// instead of refusing the swap. Stopped models stay stopped.
	Force bool `json:"force"`
}

// engineUpdateResultDTO is the response of the update route, for both
// the check-only and the swapping invocation.
type engineUpdateResultDTO struct {
	Engine    string   `json:"engine"`
	Approved  bool     `json:"approved"`
	Tag       string   `json:"tag,omitempty"`
	Note      string   `json:"note,omitempty"`
	Available bool     `json:"updateAvailable"`
	UpToDate  bool     `json:"upToDate"`
	Version   string   `json:"installedVersion,omitempty"`
	Asset     string   `json:"asset,omitempty"`
	Stopped   []string `json:"stoppedModels,omitempty"`
	PrevSlot  string   `json:"prevSlot,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// handleEngineUpdate is POST /agent/v1/engines/{name}/update: refresh
// the update check for one engine and, only when the body approves it,
// download, verify and swap the binary.
func (a *api) handleEngineUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	engine, ok := a.cfg.Engines[name]
	if !ok {
		writeError(w, r, http.StatusNotFound, "unknown engine "+name)
		return
	}
	if engine.Update == nil {
		writeError(w, r, http.StatusBadRequest, "engine "+name+" has no update source; add an update: block to its engines entry")
		return
	}

	var req engineUpdateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if a.engineStatePath() == "" {
		writeError(w, r, http.StatusServiceUnavailable, "engine update state path is not configured on this server")
		return
	}

	manager := a.engineManager()

	if !req.Approved {
		// Check-only: refresh the state from the source, no downloads.
		results, err := manager.Check(r.Context(), []string{name})
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "updating engine update state: "+err.Error())
			return
		}
		res := results[0]
		dto := engineUpdateResultDTO{
			Engine:    name,
			Approved:  false,
			Tag:       res.Tag,
			Available: res.Available,
			UpToDate:  res.UpToDate,
			Note:      res.Note,
		}
		if res.Err != nil {
			dto.Error = res.Err.Error()
		}
		writeJSON(w, r, http.StatusOK, dto)
		return
	}

	res, err := manager.Update(r.Context(), name, req.Force)
	if err != nil {
		if engineupdate.IsRefusal(err) {
			writeError(w, r, http.StatusConflict, err.Error())
			return
		}
		writeError(w, r, http.StatusInternalServerError, "updating engine "+name+": "+err.Error())
		return
	}

	dto := engineUpdateResultDTO{
		Engine:   name,
		Approved: true,
		Version:  res.Version,
		Asset:    res.AssetName,
		Stopped:  res.StoppedModels,
		UpToDate: true,
	}
	if len(res.StoppedModels) > 0 {
		sort.Strings(dto.Stopped)
	}
	writeJSON(w, r, http.StatusOK, dto)
}

// engineManager builds the updater over this API's config and the
// engine state path main wired in.
func (a *api) engineManager() *engineupdate.Manager {
	return engineupdate.NewManager(a.cfg, a.engineStatePath(), a.engineRuntime())
}

// engineStatePath returns the updater state file path, "" when main
// has not wired one in (tests and non-server callers).
func (a *api) engineStatePath() string {
	if a.opts.EngineStatePath == nil {
		return ""
	}
	return a.opts.EngineStatePath()
}

// engineRuntime returns the running-models view, nil when this API has
// none (checks and state still work; updates then require force).
func (a *api) engineRuntime() engineupdate.Runtime {
	if a.opts.Runtime == nil {
		return nil
	}
	return a.opts.Runtime
}
