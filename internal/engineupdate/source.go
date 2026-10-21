package engineupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// checkTimeout bounds a single update-source query (API page or HEAD).
const checkTimeout = 30 * time.Second

// Release is one candidate artifact a source matched: everything the
// manager needs to decide and download. Tag is the release tag
// (GitHub sources); direct URLs leave it empty.
type Release struct {
	Tag       string
	AssetName string
	URL       string

	// SHA256Hex is the checksum the artifact will be verified against:
	// the pinned hex from the update block, or the digest the GitHub
	// release API exposes. Empty means the source could not provide
	// one and the update must be refused.
	SHA256Hex string

	// ETag and LastModified are the validators the artifact server
	// exposed during the check, recorded so later checks can detect
	// a changed artifact without downloading it.
	ETag         string
	LastModified string
}

// Source produces the newest matching Release for one engine.
// Implementations never touch the engine binary; they only answer
// "what is the latest artifact".
type Source interface {
	// Latest returns the newest release matching the configured
	// tag/asset regexes, or an error explaining why none matched.
	Latest(ctx context.Context) (Release, error)

	// Kind is the update block's source kind ("github" or "url"),
	// recorded into the state file.
	Kind() string
}

// NewSource builds the Source an engine's update block describes. The
// block is validated at config load, so this only fails on a nil
// update block.
func NewSource(engine string, u *config.EngineUpdateConfig, client *http.Client) (Source, error) {
	if u == nil {
		return nil, fmt.Errorf("engine %s has no update source; add an update: block to its engines entry", engine)
	}
	switch strings.TrimSpace(u.Source) {
	case "github":
		return NewGitHubSource(u, client), nil
	case "url":
		return NewURLSource(u, client), nil
	default:
		return nil, fmt.Errorf("engine %s: unknown update source kind %q", engine, u.Source)
	}
}

// compileRegex compiles a possibly-empty pattern into a nil-when-empty
// regexp, so callers can write `if re != nil && re.MatchString(...)`.
func compileRegex(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, nil
	}
	return regexp.Compile(pattern)
}

// GitHubSource matches releases from {api_base}/repos/{repo}/releases.
type GitHubSource struct {
	apiBase    string
	repo       string
	assetRegex *regexp.Regexp
	tagRegex   *regexp.Regexp
	pinnedSHA  string
	client     *http.Client
}

// NewGitHubSource builds a GitHub releases source. api_base defaults to
// the public API; tests and GitHub Enterprise installs point it
// elsewhere.
func NewGitHubSource(u *config.EngineUpdateConfig, client *http.Client) *GitHubSource {
	assetRegex, err := compileRegex(u.AssetRegex)
	if err != nil {
		panic(fmt.Sprintf("engineupdate: asset_regex %q must be validated at config load: %v", u.AssetRegex, err))
	}
	tagRegex, err := compileRegex(u.TagRegex)
	if err != nil {
		panic(fmt.Sprintf("engineupdate: tag_regex %q must be validated at config load: %v", u.TagRegex, err))
	}
	return &GitHubSource{
		apiBase:    u.GitHubAPIBase(),
		repo:       u.Repo,
		assetRegex: assetRegex,
		tagRegex:   tagRegex,
		pinnedSHA:  strings.ToLower(strings.TrimSpace(u.SHA256)),
		client:     client,
	}
}

func (s *GitHubSource) Kind() string { return "github" }

// ghAsset is the subset of the GitHub release asset object the updater
// reads. Digest is the checksum GitHub exposes as "sha256:<hex>" since
// the 2025 releases API.
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

// ghRelease is the subset of the GitHub release object the updater
// reads. The API lists releases newest-first.
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

// Latest walks the release list (newest first, following pagination)
// and returns the first release whose tag matches tag_regex — all
// non-draft releases when no tag_regex is set — with the first asset
// whose name matches asset_regex. Prereleases are only considered when
// tag_regex explicitly matches them.
func (s *GitHubSource) Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	var all []ghRelease
	for page := 1; page <= 10; page++ {
		url := fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", s.apiBase, s.repo, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return Release{}, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := s.client.Do(req)
		if err != nil {
			return Release{}, fmt.Errorf("querying %s: %w", url, err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return Release{}, fmt.Errorf("github releases API %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
		}

		var releases []ghRelease
		decodeErr := json.NewDecoder(resp.Body).Decode(&releases)
		resp.Body.Close()
		if decodeErr != nil {
			return Release{}, fmt.Errorf("decoding releases from %s: %w", url, decodeErr)
		}
		all = append(all, releases...)
		if len(releases) < 100 {
			break
		}
	}

	for _, rel := range all {
		if rel.Draft {
			continue
		}
		if rel.TagName == "" {
			continue
		}
		if s.tagRegex != nil {
			if matched := s.tagRegex.MatchString(rel.TagName); !matched {
				continue
			}
		} else if rel.Prerelease {
			// Without a tag_regex, pre-releases are skipped: a
			// plain "latest release" query must not pick an rc.
			continue
		}

		for _, asset := range rel.Assets {
			if !s.assetRegex.MatchString(asset.Name) {
				continue
			}
			if asset.BrowserDownloadURL == "" {
				continue
			}

			digest := normalizeDigest(asset.Digest)
			if s.pinnedSHA != "" {
				digest = s.pinnedSHA
			} else if digest == "" {
				return Release{}, fmt.Errorf(
					"release %s asset %s exposes no digest; pin sha256 in the engine's update block to update to it",
					rel.TagName, asset.Name)
			}

			return Release{
				Tag:       rel.TagName,
				AssetName: asset.Name,
				URL:       asset.BrowserDownloadURL,
				SHA256Hex: digest,
			}, nil
		}
	}

	return Release{}, fmt.Errorf("no release of %s matches (tag_regex: %q, asset_regex: %q)",
		s.repo, orNone(s.tagRegex), orNone(s.assetRegex))
}

// orNone renders a regex for error messages.
func orNone(re *regexp.Regexp) string {
	if re == nil {
		return "any"
	}
	return re.String()
}

// normalizeDigest accepts the GitHub digest field ("sha256:<hex>",
// sometimes bare hex or uppercase) and returns lowercase bare hex, or
// empty when absent or not a sha256 digest.
func normalizeDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return ""
	}
	if i := strings.IndexByte(digest, ':'); i >= 0 {
		if !strings.EqualFold(digest[:i], "sha256") {
			return ""
		}
		digest = digest[i+1:]
	}
	digest = strings.ToLower(digest)
	if len(digest) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ""
	}
	return digest
}

// URLSource is a single direct artifact URL with a pinned checksum.
type URLSource struct {
	url       string
	pinnedSHA string
	client    *http.Client
}

// NewURLSource builds a direct-URL source. The pinned checksum is
// required by config validation; there is nothing to auto-trust on a
// plain URL.
func NewURLSource(u *config.EngineUpdateConfig, client *http.Client) *URLSource {
	return &URLSource{
		url:       u.URL,
		pinnedSHA: strings.ToLower(strings.TrimSpace(u.SHA256)),
		client:    client,
	}
}

func (s *URLSource) Kind() string { return "url" }

// Latest probes the URL with HEAD to learn the artifact's current
// validators (ETag / Last-Modified) without downloading it. Some
// servers answer 405 to HEAD; the check then reports "unknown" and the
// update itself still works — it verifies the pinned sha256 after the
// download.
func (s *URLSource) Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.url, nil)
	if err != nil {
		return Release{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("probing %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Release{}, fmt.Errorf("probing %s: %s", s.url, resp.Status)
	}

	return Release{
		URL:          s.url,
		SHA256Hex:    s.pinnedSHA,
		AssetName:    "",
		ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
	}, nil
}
