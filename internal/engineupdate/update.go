package engineupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// downloadTimeout bounds one artifact download.
const downloadTimeout = 10 * time.Minute

// prevSuffix names the one rollback slot next to the target binary:
// <binary>.prev. A single slot (not .prev-<timestamp>) is kept so the
// previous build is always exactly one rollback away and the engine
// directory never accumulates old binaries; a second update replaces
// the slot, rolling back is reversible because rollback itself keeps
// the replaced newer build in the slot.
const prevSuffix = ".prev"

// RefusalError is returned when an update or rollback was refused
// because models using the engine are running (approve with --force,
// or stop them first). Nothing has been touched when it is returned.
type RefusalError struct {
	Engine  string
	Reason  string
	Running []string
}

func (e *RefusalError) Error() string {
	if len(e.Running) > 0 {
		sort.Strings(e.Running)
		return fmt.Sprintf("engine %s: models %s are running and would be swapped under; stop them or rerun with --force",
			e.Engine, strings.Join(e.Running, ", "))
	}
	return fmt.Sprintf("engine %s: %s", e.Engine, e.Reason)
}

// Runtime is the updater's window into the running llama-swap: which
// models using an engine are up, and how to stop them through the
// existing lifecycle machinery. The agent API supplies the real one
// inside the server; the CLI verifies through a running server when
// agent_api is enabled, and runs with a nil Runtime otherwise (which
// forces the --force gate for local swaps).
type Runtime interface {
	// RunningModelsUsingEngine returns the IDs of the running models
	// that launch with the named engine.
	RunningModelsUsingEngine(engine string) ([]string, error)

	// StopModelsUsingEngine stops those models through the existing
	// unload machinery and returns their IDs. Models stopped for an
	// update stay stopped: they are not silently restarted into an
	// unverified binary.
	StopModelsUsingEngine(engine string) ([]string, error)
}

// Manager owns the update lifecycle for the engines of one config:
// checking sources, recording state, and swapping binaries.
type Manager struct {
	Engines   map[string]config.EngineConfig
	Order     []string
	StatePath string
	Client    *http.Client
	Runtime   Runtime

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// NewManager builds a Manager over a config's engines.
func NewManager(cfg config.Config, statePath string, rt Runtime) *Manager {
	return &Manager{
		Engines:   cfg.Engines,
		Order:     cfg.EnginesOrder,
		StatePath: statePath,
		Client:    &http.Client{},
		Runtime:   rt,
		Now:       time.Now,
	}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return http.DefaultClient
}

// engineNames returns the requested engine names, or every defined
// engine name in config order.
func (m *Manager) engineNames(requested []string) []string {
	if len(requested) > 0 {
		return requested
	}
	names := make([]string, 0, len(m.Engines))
	for _, name := range m.Order {
		if _, ok := m.Engines[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// hasUpdateSource reports whether the engine declares one.
func (m *Manager) hasUpdateSource(name string) bool {
	engine, ok := m.Engines[name]
	return ok && engine.Update != nil
}

// ListEntry is one row of `llama-swap engines list`.
type ListEntry struct {
	Name             string
	Label            string
	CmdPath          string
	InstalledVersion string
	UpdateAvailable  string
	LastCheckAt      time.Time
	LastUpdateAt     time.Time
	HasUpdateSource  bool
}

// List describes every engine: what is installed and whether a check
// found a newer build. It reads only the state file — no network.
func (m *Manager) List() ([]ListEntry, error) {
	state, err := LoadState(m.StatePath)
	if err != nil {
		return nil, err
	}

	entries := make([]ListEntry, 0, len(m.Engines))
	for _, name := range m.engineNames(nil) {
		engine := m.Engines[name]
		binPath, binErr := targetBinaryPath(engine)

		entry := ListEntry{
			Name:            name,
			Label:           engine.Label,
			CmdPath:         binPath,
			HasUpdateSource: engine.Update != nil,
		}
		if binErr != nil {
			entry.CmdPath = fmt.Sprintf("(unresolvable: %v)", binErr)
		}

		if st := state.engine(name); st != nil {
			entry.InstalledVersion = m.installedVersion(name, engine, st)
			entry.LastCheckAt = st.LastCheckAt
			entry.LastUpdateAt = st.LastUpdateAt
			if st.Available != nil {
				entry.UpdateAvailable = st.Available.Tag
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// installedVersion is the state's recorded version, seeded by the
// update block's optional version field when the updater has no record.
func (m *Manager) installedVersion(name string, engine config.EngineConfig, st *EngineState) string {
	if st.InstalledVersion != "" {
		return st.InstalledVersion
	}
	if engine.Update != nil && engine.Update.Version != "" {
		return engine.Update.Version
	}
	return ""
}

// CheckResult is one engine's row of a check report.
type CheckResult struct {
	Engine    string
	Tag       string // matched release tag ("" for direct URLs)
	Available bool
	UpToDate  bool
	Note      string
	Err       error
}

// Check queries the update sources of the named engines (all engines
// with a source when none are named), records what it found in the
// state file, and returns the report. It never downloads anything.
// Re-checking is idempotent: the same source state yields the same
// report and only refreshes last_check_at.
func (m *Manager) Check(ctx context.Context, names []string) ([]CheckResult, error) {
	state, err := LoadState(m.StatePath)
	if err != nil {
		return nil, err
	}

	var targets []string
	for _, name := range m.engineNames(names) {
		if m.hasUpdateSource(name) {
			targets = append(targets, name)
		}
	}

	results := make([]CheckResult, 0, len(targets))
	now := m.now()
	for _, name := range targets {
		source, err := NewSource(name, m.Engines[name].Update, m.client())
		res := CheckResult{Engine: name}

		if err != nil {
			res.Err = err
		} else {
			st := state.engine(name)
			st.Source = source.Kind()
			st.LastCheckAt = now

			rel, relErr := source.Latest(ctx)
			switch {
			case relErr != nil:
				res.Err = relErr
				st.Available = nil
			default:
				res.Tag = rel.Tag
				available, note := m.releaseIsNewer(name, st, rel)
				res.Available = available
				res.UpToDate = !available && note == ""
				res.Note = note
				if available {
					st.Available = &UpdateAvailable{Tag: rel.Tag, URL: rel.URL, CheckedAt: now}
				} else {
					st.Available = nil
				}
			}
		}
		results = append(results, res)
	}

	// A failing source is one row of the report, not a failed check.
	if err := state.Save(m.StatePath); err != nil {
		return results, err
	}
	return results, nil
}

// releaseIsNewer decides whether the matched release is newer than what
// the state says is installed, and explains itself in a note.
//
// GitHub sources compare the matched tag with the recorded (or
// version-seeded) installed version. Direct URLs have no version feed:
// the artifact's ETag / Last-Modified validators are compared with the
// ones recorded at the last successful download; with neither a
// recorded validator nor a version seed the check assumes the engine
// has never been installed through the updater and reports available.
func (m *Manager) releaseIsNewer(name string, st *EngineState, rel Release) (bool, string) {
	engine := m.Engines[name]
	if rel.Tag != "" {
		installed := st.InstalledVersion
		if installed == "" && engine.Update != nil {
			installed = engine.Update.Version
		}
		if installed == "" {
			return true, "no version recorded for the installed binary"
		}
		return rel.Tag != installed, ""
	}

	// Direct URL: validator comparison.
	hadValidator := st.ETag != "" || st.LastModified != ""
	if !hadValidator {
		if rel.ETag == "" && rel.LastModified == "" {
			return true, "server exposes no ETag/Last-Modified; run update to re-verify the pinned checksum"
		}
		return true, "not installed through the updater yet"
	}
	if rel.ETag != "" && st.ETag != "" {
		return rel.ETag != st.ETag, ""
	}
	if rel.LastModified != "" && st.LastModified != "" {
		return rel.LastModified != st.LastModified, ""
	}
	return true, "server stopped exposing the validator it served last time"
}

// UpdateResult reports what an update did.
type UpdateResult struct {
	Engine         string
	Version        string
	AssetName      string
	StoppedModels  []string
	RolledBackFrom string
}

// Update downloads the newest matched artifact for the engine, verifies
// its checksum, and atomically swaps the engine binary. When models
// using the engine are running it refuses unless force is set, in which
// case it stops them first and leaves them stopped. The previous binary
// is kept at <binary>.prev for `engines rollback`.
func (m *Manager) Update(ctx context.Context, name string, force bool) (UpdateResult, error) {
	engine, ok := m.Engines[name]
	if !ok {
		return UpdateResult{}, fmt.Errorf("unknown engine %q", name)
	}
	source, err := NewSource(name, engine.Update, m.client())
	if err != nil {
		return UpdateResult{}, err
	}

	target, err := targetBinaryPath(engine)
	if err != nil {
		return UpdateResult{}, err
	}

	state, err := LoadState(m.StatePath)
	if err != nil {
		return UpdateResult{}, err
	}
	st := state.engine(name)
	st.Source = source.Kind()
	st.LastCheckAt = m.now()

	rel, err := source.Latest(ctx)
	if err != nil {
		state.Save(m.StatePath)
		return UpdateResult{}, err
	}
	st.Available = nil

	stopped, err := m.gateRunningModels(name, force)
	if err != nil {
		state.Save(m.StatePath)
		return UpdateResult{}, err
	}

	if rel.SHA256Hex == "" {
		state.Save(m.StatePath)
		return UpdateResult{}, fmt.Errorf("engine %s: no checksum available for %s; pin sha256 in the update block", name, artifactLabel(rel))
	}

	downloadCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	etag, lastModified, err := m.downloadAndSwap(downloadCtx, target, rel)
	if err != nil {
		state.Save(m.StatePath)
		return UpdateResult{}, err
	}

	// Record what is now installed. GitHub sources know the tag; a
	// direct URL installs the version its block pins with the
	// checksum, if any.
	version := rel.Tag
	if version == "" && engine.Update != nil {
		version = engine.Update.Version
	}
	st.InstalledVersion = version
	st.LastUpdateAt = m.now()
	st.ETag = etag
	st.LastModified = lastModified
	if err := state.Save(m.StatePath); err != nil {
		return UpdateResult{}, err
	}

	return UpdateResult{
		Engine:        name,
		Version:       version,
		AssetName:     rel.AssetName,
		StoppedModels: stopped,
	}, nil
}

// gateRunningModels enforces the no-swap-under-running-models rule:
// refuse when a Runtime sees running models unless force is set, stop
// them through the Runtime when it is. Without a Runtime nothing can
// be verified, so a swap requires force; an unreachable Runtime
// (server down mid-update) behaves the same way.
func (m *Manager) gateRunningModels(name string, force bool) ([]string, error) {
	if m.Runtime == nil {
		if force {
			return nil, nil
		}
		return nil, &RefusalError{Engine: name,
			Reason: "cannot verify that no models are running (no agent API available); rerun with --force to swap anyway",
		}
	}

	running, err := m.Runtime.RunningModelsUsingEngine(name)
	if err != nil {
		if force {
			return nil, nil
		}
		return nil, &RefusalError{Engine: name,
			Reason: fmt.Sprintf("cannot verify that no models are running (%v); rerun with --force to swap anyway", err),
		}
	}
	if len(running) == 0 {
		return nil, nil
	}
	if !force {
		return nil, &RefusalError{Engine: name, Running: running}
	}

	stopped, err := m.Runtime.StopModelsUsingEngine(name)
	if err != nil {
		return nil, fmt.Errorf("engine %s: stopping running models: %w", name, err)
	}
	return stopped, nil
}

// downloadAndSwap fetches the release artifact to a temp file next to
// the target (same filesystem, so the final rename is atomic), verifies
// its sha256, makes it executable and swaps it in, keeping the previous
// binary at <target>.prev. On any failure the target is untouched and
// the temp file is removed. The returned validators (ETag,
// Last-Modified) are recorded in the state for later checks.
func (m *Manager) downloadAndSwap(ctx context.Context, target string, rel Release) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("downloading %s: %w", rel.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("downloading %s: %s", rel.URL, resp.Status)
	}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	lastModified := strings.TrimSpace(resp.Header.Get("Last-Modified"))

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".llama-swap-engine-*")
	if err != nil {
		return "", "", fmt.Errorf("creating temp file next to %s: %w", target, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful swap

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("downloading %s: %w", rel.URL, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}

	// Checksum mismatch aborts with nothing touched: only the temp
	// file (removed by the defer) ever held the bad bytes.
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != rel.SHA256Hex {
		return "", "", fmt.Errorf("checksum mismatch for %s: expected sha256 %s, downloaded %s; nothing was changed",
			artifactLabel(rel), rel.SHA256Hex, got)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", "", err
	}

	prev := target + prevSuffix
	if _, err := os.Stat(prev); err == nil {
		if err := os.Remove(prev); err != nil {
			return "", "", fmt.Errorf("clearing the previous rollback slot %s: %w", prev, err)
		}
	}
	if err := os.Rename(target, prev); err != nil {
		return "", "", fmt.Errorf("moving current binary aside: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		// Put the current binary back; a swap must be all-or-nothing.
		if restoreErr := os.Rename(prev, target); restoreErr != nil {
			return "", "", fmt.Errorf("installing new binary: %v (restoring %s failed: %v; recover with `engines rollback`)", err, target, restoreErr)
		}
		return "", "", fmt.Errorf("installing new binary: %w", err)
	}
	return etag, lastModified, nil
}

// RollbackResult reports what a rollback did.
type RollbackResult struct {
	Engine        string
	RestoredFrom  string
	StoppedModels []string
}

// Rollback restores the binary kept at <target>.prev, applying the same
// running-models gate as Update (it swaps the binary under the same
// feet). The swapped-out newer build becomes the new .prev, so a
// rollback is itself reversible.
func (m *Manager) Rollback(name string, force bool) (RollbackResult, error) {
	engine, ok := m.Engines[name]
	if !ok {
		return RollbackResult{}, fmt.Errorf("unknown engine %q", name)
	}
	target, err := targetBinaryPath(engine)
	if err != nil {
		return RollbackResult{}, err
	}
	prev := target + prevSuffix

	if _, err := os.Stat(prev); err != nil {
		return RollbackResult{}, fmt.Errorf("engine %s: no rollback slot (%s) — nothing to roll back to", name, prev)
	}

	stopped, err := m.gateRunningModels(name, force)
	if err != nil {
		return RollbackResult{}, err
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".llama-swap-rollback-*")
	if err != nil {
		return RollbackResult{}, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	os.Remove(tmpName) // only the name is needed as a rename landing pad

	if err := os.Rename(target, tmpName); err != nil {
		return RollbackResult{}, fmt.Errorf("moving current binary aside: %w", err)
	}
	if err := os.Rename(prev, target); err != nil {
		// Put the current binary back.
		if restoreErr := os.Rename(tmpName, target); restoreErr != nil {
			return RollbackResult{}, fmt.Errorf("restoring previous binary: %v (restoring %s failed: %v)", err, target, restoreErr)
		}
		return RollbackResult{}, fmt.Errorf("restoring previous binary: %w", err)
	}
	if err := os.Rename(tmpName, prev); err != nil {
		return RollbackResult{}, fmt.Errorf("keeping the swapped-out binary as the new rollback slot: %w", err)
	}

	// The restored build's version is unknown to the updater; the
	// next `engines check` re-syncs it against the source.
	state, err := LoadState(m.StatePath)
	if err != nil {
		return RollbackResult{}, err
	}
	st := state.engine(name)
	st.InstalledVersion = ""
	st.LastUpdateAt = m.now()
	st.Available = nil
	if err := state.Save(m.StatePath); err != nil {
		return RollbackResult{}, err
	}

	return RollbackResult{
		Engine:        name,
		RestoredFrom:  prev,
		StoppedModels: stopped,
	}, nil
}

// targetBinaryPath resolves the engine's binary — the first token of
// its cmd — to the concrete file the updater swaps: absolute paths are
// used as-is (following symlinks, so an update replaces the real
// binary, not a symlink pointing at it), bare names are looked up in
// PATH. The config layer already verified the binary exists at load.
func targetBinaryPath(engine config.EngineConfig) (string, error) {
	args, err := config.SanitizeCommand(engine.Cmd)
	if err != nil || len(args) == 0 {
		return "", fmt.Errorf("engine has no usable cmd")
	}
	bin := args[0]

	resolved := bin
	if !filepath.IsAbs(resolved) {
		lookedUp, lookErr := exec.LookPath(bin)
		if lookErr != nil {
			return "", fmt.Errorf("engine binary %q: %w", bin, lookErr)
		}
		resolved = lookedUp
	}

	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("engine binary %q: %w", bin, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("engine binary %q: %w", bin, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("engine binary %q is a directory", bin)
	}
	return real, nil
}

// artifactLabel names a release's artifact for messages.
func artifactLabel(rel Release) string {
	if rel.AssetName != "" {
		return rel.AssetName
	}
	return rel.URL
}

// IsRefusal reports whether err is the running-models refusal, so
// callers can surface it without guessing from the text.
func IsRefusal(err error) bool {
	var refusal *RefusalError
	return errors.As(err, &refusal)
}
