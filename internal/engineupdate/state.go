// Package engineupdate implements Obtium-style updates for the engines
// defined in llama-swap's engines section: each engine may declare an
// update source (GitHub releases or a direct URL), the manager checks
// sources for newer builds, downloads and checksum-verifies the matched
// asset, and atomically swaps the engine binary while keeping the
// previous build for a one-command rollback.
//
// The manager is pure machinery: it never decides policy. The callers
// are the `llama-swap engines` CLI subcommand (internal, offline against
// the config file) and the agent API update routes (inside the running
// server, where the Runtime interface can see and stop running models).
package engineupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateFileName is the file the updater keeps next to the config file.
const StateFileName = "engine-updates.json"

// DefaultStatePath returns the state file path for a config source. When
// path is a directory (config-dir mode) the file lands inside it; when
// it is a file (-config mode) it lands next to it.
func DefaultStatePath(path string) string {
	if path == "" {
		return StateFileName
	}
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return filepath.Join(path, StateFileName)
	}
	return filepath.Join(filepath.Dir(path), StateFileName)
}

// UpdateAvailable records what a check found, so `engines list` can say
// "update available: <tag>" without re-querying the source.
type UpdateAvailable struct {
	Tag       string    `json:"tag,omitempty"`
	URL       string    `json:"url,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// EngineState is the recorded updater state of one engine.
type EngineState struct {
	// InstalledVersion is the version of the binary the updater last
	// installed (matched release tag, or the update block's version
	// seed for direct URLs). Empty when unknown — after a rollback or
	// when nothing was ever installed through the updater.
	InstalledVersion string `json:"installed_version,omitempty"`

	// Source is a copy of the update block's source kind ("github" or
	// "url") at the time of the last check.
	Source string `json:"source,omitempty"`

	LastCheckAt  time.Time        `json:"last_check_at,omitempty"`
	LastUpdateAt time.Time        `json:"last_update_at,omitempty"`
	Available    *UpdateAvailable `json:"update_available,omitempty"`

	// ETag and LastModified record the validators the download server
	// exposed for the artifact that was installed. Direct-URL checks
	// compare them to detect changed artifacts without a download.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// State is the whole state file: updater state per engine name.
type State struct {
	Engines map[string]*EngineState `json:"engines"`
}

// LoadState reads the state file. A missing file is an empty state, not
// an error; a malformed file is an error (the updater refuses to guess
// what was installed).
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{Engines: map[string]*EngineState{}}, nil
		}
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("engine update state %s is corrupt: %w", path, err)
	}
	if state.Engines == nil {
		state.Engines = map[string]*EngineState{}
	}
	return &state, nil
}

// engine returns the state entry for name, creating it on first use.
func (s *State) engine(name string) *EngineState {
	st, ok := s.Engines[name]
	if !ok {
		st = &EngineState{}
		s.Engines[name] = st
	}
	return st
}

// Save writes the state file atomically (temp file + rename) so a crash
// mid-write can never leave it truncated.
func (s *State) Save(path string) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".engine-updates-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
