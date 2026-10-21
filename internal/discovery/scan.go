package discovery

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// Status values for a scanned file.
const (
	// StatusDrafted: metadata extracted and a draft stanza produced.
	StatusDrafted = "drafted"
	// StatusSkipped: reported but not draftable (parse failure, non-model
	// GGUF type, or no engine to draft against).
	StatusSkipped = "skipped"
)

// FileResult is the per-file outcome of a discovery scan.
type FileResult struct {
	// Path is the scanned file's path as configured.
	Path string `json:"path"`

	// Status is "drafted" or "skipped".
	Status string `json:"status"`

	// Metadata is the extracted GGUF metadata, nil when parsing failed.
	Metadata *Metadata `json:"metadata,omitempty"`

	// MetadataError carries the parse failure reason when Metadata is nil.
	MetadataError string `json:"metadataError,omitempty"`

	// Draft is the generated stanza, nil when skipped.
	Draft *Draft `json:"draft,omitempty"`

	// EngineSource explains the engine choice (set when a draft exists).
	EngineSource string `json:"engineSource,omitempty"`

	// SkipReason explains StatusSkipped.
	SkipReason string `json:"skipReason,omitempty"`
}

// ScanResult is a whole-folder-set scan outcome.
type ScanResult struct {
	Results []FileResult `json:"results"`

	Found   int `json:"found"`   // .gguf files seen
	Drafted int `json:"drafted"` // drafts produced
	Skipped int `json:"skipped"` // reported but not draftable
}

// Run scans the config's discovery folders for .gguf files and drafts a
// model stanza for each draftable one. It only reads files — the config is
// never written and nothing is started. A non-parseable or unreadable GGUF
// is reported and skipped, not fatal. Config-level problems (no discovery
// section, no folders) are returned as errors.
func Run(cfg config.Config) (ScanResult, error) {
	result := ScanResult{Results: []FileResult{}}

	if cfg.Discovery == nil {
		return result, fmt.Errorf("config has no discovery section; add discovery.folders to use `llama-swap discover`")
	}
	if len(cfg.Discovery.Folders) == 0 {
		return result, fmt.Errorf("discovery.folders is empty; add at least one folder to scan")
	}

	paths, err := collectGGUFFiles(cfg.Discovery.Folders)
	if err != nil {
		return result, err
	}

	for _, path := range paths {
		result.Found++
		fr := scanFile(cfg, path)
		switch fr.Status {
		case StatusDrafted:
			result.Drafted++
		case StatusSkipped:
			result.Skipped++
		}
		result.Results = append(result.Results, fr)
	}
	return result, nil
}

// scanFile processes one file: metadata, draftability decision, draft.
func scanFile(cfg config.Config, path string) FileResult {
	fr := FileResult{Path: path, Status: StatusSkipped}

	meta, err := ReadMetadata(path)
	if err != nil {
		fr.MetadataError = err.Error()
		fr.SkipReason = fmt.Sprintf("could not parse GGUF: %v", err)
		return fr
	}
	fr.Metadata = &meta

	// Adapters (LoRA), projectors (mmproj) and vocab files are GGUFs but
	// not chat models; general.type says which. An absent general.type
	// (older converters) is treated as a model.
	if meta.Type != "" && meta.Type != "model" {
		fr.SkipReason = fmt.Sprintf("general.type is %q, not a chat model", meta.Type)
		return fr
	}

	draft, err := DraftForFile(cfg, meta)
	if err != nil {
		fr.SkipReason = err.Error()
		return fr
	}
	fr.EngineSource = draft.EngineSource

	// The draft must load exactly like a hand-written stanza. Validation
	// failures are reported per-file instead of aborting the scan.
	if err := draft.ValidateAgainst(cfg); err != nil {
		fr.SkipReason = fmt.Sprintf("draft did not pass config validation: %v", err)
		return fr
	}

	fr.Draft = &draft
	fr.Status = StatusDrafted
	return fr
}

// collectGGUFFiles walks each folder depth-first and returns the sorted,
// deduplicated set of .gguf file paths. Unreadable folders abort the scan
// with a clear error (folders are validated at config load, so this is rare
// — e.g. permissions changed since load).
func collectGGUFFiles(folders []string) ([]string, error) {
	seen := make(map[string]bool)
	var paths []string

	for _, folder := range folders {
		folder = strings.TrimSpace(folder)
		err := filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", folder, err)
		}
	}

	sort.Strings(paths)
	return paths, nil
}

// SummaryLine renders the trailing "N found / M draftable / K skipped" line.
func (s ScanResult) SummaryLine() string {
	return fmt.Sprintf("%d found / %d draftable / %d skipped", s.Found, s.Drafted, s.Skipped)
}
