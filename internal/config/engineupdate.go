package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// defaultGitHubAPIBase is the public GitHub releases API used when an
// engine's update block does not override api_base (GitHub Enterprise
// Server installs can point it at their own host).
const defaultGitHubAPIBase = "https://api.github.com"

// engineRepoPattern accepts GitHub "owner/name" repository slugs: one
// path element each, limited to the characters GitHub allows in user and
// repository names. Schemes, ports and query strings are rejected.
var engineRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// EngineUpdateConfig declares where new builds of an engine binary come
// from. It is the optional `update:` block inside an engines entry. Two
// source kinds exist:
//
//	github: poll {api_base}/repos/{repo}/releases, pick the newest release
//	        whose tag matches tag_regex (all releases when absent), then
//	        the first asset whose name matches asset_regex.
//	url:    a single direct URL; the artifact is verified against the
//	        pinned sha256 hex (required — "auto" is not allowed because a
//	        plain URL exposes no digest to trust).
//
// The binary is swapped atomically by the engine updater (see
// internal/engineupdate and docs/kb/guides/operations/engine-updater.md);
// the running server only reads this block to answer update checks.
type EngineUpdateConfig struct {
	// Source selects the kind: "github" or "url". Required.
	Source string `yaml:"source"`

	// Repo is the GitHub "owner/name" slug (github source). Required.
	Repo string `yaml:"repo"`

	// AssetRegex selects which release asset to download (github source).
	// Go syntax, matched unanchored against the asset name. Required.
	AssetRegex string `yaml:"asset_regex"`

	// TagRegex filters which releases are considered (github source).
	// Go syntax, matched unanchored against the tag name. Optional: all
	// non-draft releases are considered when absent.
	TagRegex string `yaml:"tag_regex"`

	// SHA256 is the checksum policy. For github sources "auto" (the
	// default) verifies the digest the GitHub release API exposes and
	// requires a pinned value when a release has none; a 64-char hex
	// string pins the checksum instead. For url sources a pinned hex
	// string is REQUIRED and "auto" is rejected.
	SHA256 string `yaml:"sha256"`

	// URL is the direct artifact URL (url source). Required, http/https.
	URL string `yaml:"url"`

	// Version optionally declares the version of the currently installed
	// binary. It seeds the recorded installed version before the first
	// check or update runs, so "update available" can be decided without
	// state. Optional for both source kinds.
	Version string `yaml:"version"`

	// APIBase overrides the GitHub API root (github source), for GitHub
	// Enterprise Server. Default https://api.github.com. Optional.
	APIBase string `yaml:"api_base"`
}

// GitHubAPIBase returns the GitHub API root for the update block,
// defaulting to the public API.
func (u *EngineUpdateConfig) GitHubAPIBase() string {
	if strings.TrimSpace(u.APIBase) != "" {
		return strings.TrimRight(strings.TrimSpace(u.APIBase), "/")
	}
	return defaultGitHubAPIBase
}

// validateEngineUpdate checks one engine's update block so a broken
// source fails at config load and `llama-swap -validate`. A nil block is
// valid: updates are opt-in per engine.
func validateEngineUpdate(name string, u *EngineUpdateConfig) error {
	if u == nil {
		return nil
	}

	switch strings.TrimSpace(u.Source) {
	case "github":
		if err := validateEngineUpdateGitHub(name, u); err != nil {
			return err
		}
	case "url":
		if err := validateEngineUpdateURL(name, u); err != nil {
			return err
		}
	case "":
		return fmt.Errorf("engines.%s.update.source is required (github or url)", name)
	default:
		return fmt.Errorf("engines.%s.update.source: unknown source kind %q (expected github or url)", name, u.Source)
	}

	if u.Version != "" && strings.TrimSpace(u.Version) == "" {
		return fmt.Errorf("engines.%s.update.version must not be blank", name)
	}
	return nil
}

func validateEngineUpdateGitHub(name string, u *EngineUpdateConfig) error {
	if !engineRepoPattern.MatchString(u.Repo) {
		return fmt.Errorf("engines.%s.update.repo: %q is not a GitHub owner/name repository", name, u.Repo)
	}

	if strings.TrimSpace(u.AssetRegex) == "" {
		return fmt.Errorf("engines.%s.update.asset_regex is required for github sources", name)
	}
	if _, err := regexp.Compile(u.AssetRegex); err != nil {
		return fmt.Errorf("engines.%s.update.asset_regex: %w", name, err)
	}

	if strings.TrimSpace(u.TagRegex) != "" {
		if _, err := regexp.Compile(u.TagRegex); err != nil {
			return fmt.Errorf("engines.%s.update.tag_regex: %w", name, err)
		}
	}

	switch policy := strings.TrimSpace(u.SHA256); policy {
	case "", "auto":
		// "auto" is the github default: use the release API digest,
		// and require a pin when a release has none.
	default:
		if err := validatePinnedSHA256(name, policy); err != nil {
			return err
		}
	}

	if strings.TrimSpace(u.APIBase) != "" {
		parsed, err := url.Parse(u.APIBase)
		if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" {
			return fmt.Errorf("engines.%s.update.api_base: %q is not a base http(s) URL", name, u.APIBase)
		}
	}
	return nil
}

func validateEngineUpdateURL(name string, u *EngineUpdateConfig) error {
	parsed, err := url.Parse(u.URL)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" {
		return fmt.Errorf("engines.%s.update.url: %q is not a valid http(s) URL", name, u.URL)
	}

	policy := strings.TrimSpace(u.SHA256)
	if policy == "" {
		return fmt.Errorf("engines.%s.update.sha256 is required for url sources (a direct URL exposes no digest to auto-trust; pin the checksum)", name)
	}
	if policy == "auto" {
		return fmt.Errorf("engines.%s.update.sha256: \"auto\" is not allowed for url sources; pin the checksum as 64 hex characters", name)
	}
	if err := validatePinnedSHA256(name, policy); err != nil {
		return err
	}
	return nil
}

// validatePinnedSHA256 accepts exactly sha256.Size hexadecimal
// characters, so a truncated or non-hex checksum fails at load instead
// of at download time.
func validatePinnedSHA256(name, policy string) error {
	if len(policy) != sha256.Size*2 {
		return fmt.Errorf("engines.%s.update.sha256: expected 64 hex characters, got %d", name, len(policy))
	}
	if _, err := hex.DecodeString(policy); err != nil {
		return fmt.Errorf("engines.%s.update.sha256: not valid hex: %w", name, err)
	}
	return nil
}
