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
	"regexp"
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
	TagName string `json:"tag_name"`
	// Assets is the release's asset list as the API reports it. It is EMPTY
	// for a release resolved without a token, where no such list is ever
	// fetched — use AssetURL, which covers both cases, rather than ranging
	// over this.
	Assets []Asset `json:"assets"`

	// downloadBase is set only for a release resolved anonymously, where
	// there is no asset list to look a URL up in. It is the directory URL
	// assets hang off — ".../releases/download/<tag>" — so AssetURL can
	// name a file without ever having been told it exists.
	downloadBase string
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

// AssetURL returns the URL of the named asset: its API URL for a release
// resolved through the API, or its download URL for one resolved
// anonymously, where no asset list was ever fetched. In the anonymous case
// the name is not validated here — a file that is not in the release surfaces
// as a 404 from the download itself.
func (r *Release) AssetURL(name string) (string, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, nil
		}
	}
	if r.downloadBase != "" {
		return r.downloadBase + "/" + name, nil
	}
	return "", fmt.Errorf("release %s has no asset %q", r.TagName, name)
}

// FetchRelease resolves a release: the latest one when tag is empty,
// otherwise that exact tag.
//
// With a token this goes through the GitHub REST API, which is the only
// thing that works against a private repo. WITHOUT one it deliberately does
// not: the unauthenticated API limit is 60 requests/hour PER IP ADDRESS, so a
// fleet behind one NAT, a CI runner, or a shared office link can arrive with
// it already spent, and `update` must not fail for that. Anonymously a
// pinned tag needs no lookup at all, and "latest" is read from the
// /releases/latest redirect on the download host, which costs no quota. The
// API remains the fallback for when that redirect yields nothing usable.
//
// Anonymously a pinned tag is therefore taken at its word: it is not checked
// to exist, so a Check of one that does not will report it as available and
// only the download will fail (with an error that says so).
func FetchRelease(ctx context.Context, cfg Config, tag string) (*Release, error) {
	if err := cfg.normalise(); err != nil {
		return nil, err
	}
	return fetchRelease(ctx, &cfg, tag)
}

func fetchRelease(ctx context.Context, cfg *Config, tag string) (*Release, error) {
	// Validated once, before either branch: both paste the tag into a URL
	// path — the API's /releases/tags/<tag>, the download host's
	// /releases/download/<tag>/ — so a "../.." in a -version flag would
	// escape to a path neither caller intended, and the checksum manifest
	// would be fetched from that same wrong place rather than catching it.
	if tag != "" && !tagRe.MatchString(tag) {
		return nil, fmt.Errorf("resolve release: bad version %q: want a release tag", tag)
	}
	if cfg.Token == "" {
		if rel := anonRelease(ctx, cfg, tag); rel != nil {
			return rel, nil
		}
	}
	return apiRelease(ctx, cfg, tag)
}

// anonRelease resolves a release without touching the API, or returns nil
// when it cannot — a redirect that yields no tag, or a download host that
// does not redirect at all. A nil return is not an error: the caller falls
// back to the API, which reports the real failure.
func anonRelease(ctx context.Context, cfg *Config, tag string) *Release {
	if tag == "" {
		// Used exactly as the redirect gave it. EnsureV belongs only on a
		// tag a human typed: a project that tags "1.2.3" without the v
		// would otherwise have its own release renamed into a 404.
		tag = latestTagFromRedirect(ctx, cfg)
		if tag == "" {
			return nil
		}
	} else {
		tag = EnsureV(tag)
	}
	return &Release{
		TagName:      tag,
		downloadBase: cfg.DownloadBase + "/" + cfg.Repo + "/releases/download/" + tag,
	}
}

// tagRe matches what may be treated as a resolved tag. A redirect that lands
// somewhere unexpected — a login page, an error page, a test double echoing
// the request — must not have its last path segment installed as a version.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// latestTagFromRedirect reads the tag out of the Location of
// <download base>/<repo>/releases/latest, which GitHub 302s to
// ".../releases/tag/<tag>". Returns "" when there is no usable tag.
func latestTagFromRedirect(ctx context.Context, cfg *Config) string {
	u := cfg.DownloadBase + "/" + cfg.Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	// Stop at the redirect itself: following it would download the release
	// page's HTML for a string already sitting in the Location header.
	client := *cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	// The body is a few bytes of redirect boilerplate, but draining it is
	// what lets the connection be reused for the download that follows.
	_, _ = io.Copy(io.Discard, resp.Body)
	loc, err := resp.Location()
	if err != nil {
		return ""
	}
	// Only a .../releases/tag/<tag> destination on THIS repo is a resolved
	// release. Checking the prefix rather than merely "something before the
	// marker" is what stops a redirect that lands on another repo — a
	// transfer, a rename, anything surprising — from being installed from.
	before, tag, ok := strings.Cut(loc.Path, "/releases/tag/")
	if !ok || before != "/"+cfg.Repo || !tagRe.MatchString(tag) {
		return ""
	}
	return tag
}

func apiRelease(ctx context.Context, cfg *Config, tag string) (*Release, error) {
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
		return nil, fmt.Errorf("resolve release: github api: %s (%s)", resp.Status, releaseHint(cfg, resp.StatusCode))
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

// releaseHint explains a non-200 from the releases API in the terms that
// actually apply, which depend on whether a token was sent.
//
// The 403 case is the one that misleads: GitHub's unauthenticated rate limit
// is per IP ADDRESS, so a fleet behind one NAT exhausts it between them and
// every host then fails with what reads like a permissions error. A private
// repo, by contrast, answers 404 to an unauthenticated caller — never 403.
func releaseHint(cfg *Config, status int) string {
	if cfg.Token == "" {
		if status == http.StatusForbidden || status == http.StatusTooManyRequests {
			return "GitHub's unauthenticated API rate limit is per IP address and looks spent; set GITHUB_TOKEN, GH_TOKEN, or log in with `gh`"
		}
		return "a private repo needs GITHUB_TOKEN, GH_TOKEN or a logged-in `gh`"
	}
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return "rate-limited, or the token may not read " + cfg.Repo
	}
	return "no such release, or the token cannot read " + cfg.Repo
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
		return "", fmt.Errorf("GET %s: %s%s", src, resp.Status, assetHint(cfg, resp.StatusCode))
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

// assetHint explains a failed asset download. Anonymously the release was
// never looked up — a pinned tag is taken at its word and named directly —
// so a 404 here is the first sign that the tag or the asset does not exist,
// and a bare "404 Not Found" would leave the operator guessing.
func assetHint(cfg *Config, status int) string {
	if cfg.Token == "" && status == http.StatusNotFound {
		return " (no such release or asset in " + cfg.Repo + "; check the version tag)"
	}
	return ""
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
