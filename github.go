package distkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Release is the subset of the GitHub release JSON this package uses.
//
// Asset.URL is the *API* URL (…/releases/assets/123), not the
// browser_download_url: the API URL serves the bytes to a bearer token,
// which is the only thing that works while the repo is private.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset is one file attached to a release.
type Asset struct {
	// Name is the file name, e.g. "zulip-acp-linux-amd64".
	Name string `json:"name"`
	// URL is the asset's API URL. Fetching it with
	// Accept: application/octet-stream returns the bytes; without that
	// header it returns the asset's JSON metadata.
	URL string `json:"url"`
}

// AssetURL returns the API URL of the named asset.
func (r *Release) AssetURL(name string) (string, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("release %s has no asset %q", r.TagName, name)
}

// FetchRelease resolves a release through the GitHub API: the latest one
// when tag is empty, otherwise that exact tag.
func FetchRelease(ctx context.Context, cfg Config, tag string) (*Release, error) {
	if err := cfg.normalise(); err != nil {
		return nil, err
	}
	return fetchRelease(ctx, &cfg, tag)
}

func fetchRelease(ctx context.Context, cfg *Config, tag string) (*Release, error) {
	u := cfg.APIBase + "/repos/" + cfg.Repo + "/releases/latest"
	if tag != "" {
		u = cfg.APIBase + "/repos/" + cfg.Repo + "/releases/tags/" + EnsureV(tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve release: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authorize(req, cfg.Token)
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resolve release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A private repo answers 404 to an unauthenticated caller, so the
		// advice differs entirely depending on whether we sent a token.
		hint := "a private repo needs GITHUB_TOKEN, GH_TOKEN or a logged-in `gh`"
		if cfg.Token != "" {
			hint = "no such release, or the token cannot read " + cfg.Repo
		}
		return nil, fmt.Errorf("resolve release: github api: %s (%s)", resp.Status, hint)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("resolve release: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("resolve release: github api: empty tag_name")
	}
	return &rel, nil
}

// fetch GETs src as raw bytes, writes them to dst with the given file mode,
// and returns the hex sha256 of what was written. The hash is computed
// inline, so the file just written is never read back — one pass, and no
// chance of hashing something other than what landed on disk.
func fetch(ctx context.Context, cfg *Config, src, dst string, mode os.FileMode) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	// An asset API URL serves the release JSON unless octet-stream is asked
	// for explicitly.
	req.Header.Set("Accept", "application/octet-stream")
	authorize(req, cfg.Token)
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", src, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	body := stallGuard(resp.Body, cfg.StallTimeout)
	defer body.stop()
	if _, err := io.Copy(io.MultiWriter(f, h), body); err != nil {
		if body.stalled() {
			return "", fmt.Errorf("download stalled: no data for %s", cfg.StallTimeout)
		}
		return "", err
	}
	// Best-effort durability hint. Integrity is already guaranteed by the
	// returned sha256 the caller checks against the manifest, and the update
	// is freely re-runnable, so we do not gate on it.
	_ = f.Sync()
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stalledReader abandons a read that makes no progress for d.
//
// ResponseHeaderTimeout only bounds the headers: a connection that dies
// mid-body (a Pi losing wifi, a CDN hiccup) leaves io.Copy blocked forever,
// which under a systemd timer is a unit stuck until someone notices. A total
// deadline is the wrong fix — a 40 MB binary over a slow link legitimately
// takes minutes — so bound the absence of progress instead.
type stalledReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
	// fired is set by the timer goroutine and read after io.Copy returns,
	// under mu.
	mu    sync.Mutex
	fired bool
}

func stallGuard(r io.Reader, d time.Duration) *stalledReader {
	sr := &stalledReader{r: r, d: d}
	if d <= 0 {
		return sr
	}
	// Closing the body is what unblocks a Read parked in the kernel.
	if c, ok := r.(io.Closer); ok {
		sr.timer = time.AfterFunc(d, func() {
			sr.mu.Lock()
			sr.fired = true
			sr.mu.Unlock()
			_ = c.Close()
		})
	}
	return sr
}

func (s *stalledReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if s.timer != nil && n > 0 {
		s.timer.Reset(s.d)
	}
	return n, err
}

func (s *stalledReader) stop() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

func (s *stalledReader) stalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired
}

// authorize attaches a bearer token when one is available. Without it a
// private repo answers 404 to every request, asset downloads included.
func authorize(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// tokenCommand is a seam so tests can drive DiscoverToken's `gh` fallback
// without a logged-in gh — or a gh at all.
var tokenCommand = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "gh", "auth", "token").Output()
}

// DiscoverToken finds a GitHub token the way the surrounding tooling does:
// the standard environment variables first, then a logged-in `gh`. A fleet
// host usually has gh configured but rarely exports a token. Returns "" when
// there is none, which is fine against a public repo.
func DiscoverToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	out, err := tokenCommand()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LookupChecksum returns the hex sha256 recorded for asset in a manifest in
// `sha256sum` format ("<hex>  <name>"), as written by goreleaser.
func LookupChecksum(manifest []byte, asset string) (string, error) {
	for line := range strings.SplitSeq(string(manifest), "\n") {
		fields := strings.Fields(line)
		// The name column may be prefixed with "*" (binary mode) by some
		// sha256sum implementations.
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s", asset)
}
