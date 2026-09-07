// Package distkit owns binary distribution and self-update for the kfet
// family of tools: fir, harb, mintick, and the ACP relays.
//
// It replaces four hand-written self-update implementations, each of which
// was missing a different piece, and it carries the union of what they got
// right:
//
//   - With a token every byte moves through the GitHub REST API, asset
//     payloads included (`Accept: application/octet-stream`). That is the
//     only shape that works against a repo that is still private. Without
//     one — the ordinary public case — the API is skipped entirely: the
//     unauthenticated limit is 60 requests/hour per IP ADDRESS, so a NAT'd
//     fleet spends it between its own hosts, and an update must not fail
//     for that. "latest" then comes from the /releases/latest redirect and
//     assets from the download host, at zero API cost.
//   - sha256 of the downloaded asset is checked against the release's
//     `checksums.txt` before anything is moved into place.
//   - The replacement is an ETXTBSY-safe atomic swap: stage in a temp dir
//     NEXT TO the running binary (same directory → same filesystem) and
//     os.Rename over it. A running executable cannot be written or truncated
//     in place, but renaming a sibling over it replaces the directory entry
//     while the live process keeps its old inode mapped.
//   - A Homebrew-managed install is detected and UPGRADED THROUGH BREW
//     rather than merely refused: self-updating a keg would be silently
//     reverted by the next `brew upgrade`.
//   - An install this process does not own — a package-manager prefix, a
//     directory belonging to another user — is refused up front, with the
//     command to use instead, rather than failing three network round-trips
//     later with "permission denied".
//
// The package depends on nothing outside the standard library, and on
// nothing from ACP, any relay, or fir: every tool in the family must be able
// to import it.
//
// The usual consumer wires it up in one call:
//
//	distkit.Main(distkit.Config{
//	    Repo:        "kfet/zulip-acp",
//	    Binary:      "zulip-acp",
//	    AssetStem:   "zulip-acp",
//	    Version:     version,
//	    RestartHint: "systemctl --user reload zulip-acp",
//	})
//
// Check, Download, Apply, DetectBrewInstall and UpgradeViaBrew are exported
// separately for callers that want the pieces without the CLI.
package distkit

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// DefaultAPIBase is the GitHub REST API root.
const DefaultAPIBase = "https://api.github.com"

// DefaultDownloadBase is the host serving release downloads and the
// /releases/latest redirect. Unlike the API it has no per-IP request budget,
// which is what makes the anonymous path in github.go possible.
const DefaultDownloadBase = "https://github.com"

// DefaultAssetTemplate names a raw-binary release asset, which is what
// goreleaser's `formats: [binary]` produces. See Config.AssetTemplate.
const DefaultAssetTemplate = "{stem}-{os}-{arch}"

// DefaultStallTimeout is how long a download may make no progress at all
// before it is abandoned. See Config.StallTimeout.
const DefaultStallTimeout = 2 * time.Minute

// DefaultChecksums is the release asset listing sha256 sums, in the
// `sha256sum` output format that goreleaser writes.
const DefaultChecksums = "checksums.txt"

// repoRe matches a github "owner/name". Anything else would be pasted
// straight into an API path.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Config describes one consumer's distribution setup. Repo, Binary and
// Version are required; everything else has a working default.
type Config struct {
	// Repo is the GitHub "owner/name" releases are taken from, e.g.
	// "kfet/zulip-acp". Required.
	Repo string

	// Binary is the installed file name, e.g. "zulip-acp". It is also the
	// name searched for inside an archive asset. Required.
	Binary string

	// AssetStem is the "{stem}" of the release asset name. Defaults to
	// Binary, which is right for every consumer today.
	AssetStem string

	// Version is the compiled-in version of the running binary, with or
	// without a leading "v". Required.
	Version string

	// RestartHint is the command that makes a swapped binary take effect,
	// e.g. "systemctl --user reload zulip-acp". A replaced binary is inert
	// until the process re-execs — the running one still has the old inode
	// mapped — so this is printed after every successful update. Optional:
	// a one-shot CLI has nothing to recycle.
	RestartHint string

	// AssetTemplate names the release asset. Placeholders:
	//
	//	{stem}        AssetStem
	//	{binary}      Binary
	//	{os}          runtime.GOOS
	//	{arch}        release arch suffix (see ArmSuffix)
	//	{version}     resolved tag, with leading v
	//	{version_no_v} resolved tag, without leading v
	//
	// Defaults to DefaultAssetTemplate ("{stem}-{os}-{arch}"), a raw
	// binary. When the rendered name ends in .tar.gz, .tgz or .zip the
	// asset is treated as an archive and Binary is extracted from it —
	// harb publishes "{stem}-{version_no_v}-{os}-{arch}.tar.gz".
	AssetTemplate string

	// ChecksumsAsset is the name of the checksum manifest in the release.
	// Defaults to DefaultChecksums.
	ChecksumsAsset string

	// SkipChecksums installs the asset without verifying it. No consumer
	// should set this: the bytes are only trustworthy once the manifest
	// agrees. It exists for a project whose release process genuinely
	// publishes no manifest.
	SkipChecksums bool

	// ArmSuffix is the arch token used in asset names for 32-bit ARM,
	// where GOARCH is the uninformative "arm". The family builds at
	// GOARM=6 and publishes "armv6"; that is the default. A project whose
	// assets say plain "arm" sets it to "arm".
	ArmSuffix string

	// DisableBrew turns off Homebrew detection. With it set, a keg install
	// is refused by the managed-prefix guard instead of being upgraded
	// through `brew upgrade`. Only useful for a binary that is never
	// distributed as a formula.
	DisableBrew bool

	// Token is a GitHub token. Empty means discover one from the
	// environment (GITHUB_TOKEN, GH_TOKEN) and then from a logged-in `gh`.
	Token string

	// tokenResolved records that discovery has already run, so the nested
	// entry points (Update → Check → Download) do not re-exec `gh auth
	// token` once per call on a host that has no token to find.
	tokenResolved bool

	// APIBase is the GitHub API root; empty means DefaultAPIBase. Tests
	// point it at an httptest server.
	APIBase string

	// DownloadBase is the host release assets are fetched from when no
	// token is in play, and the host whose /releases/latest redirect
	// resolves "latest" without spending API quota. Empty means
	// DefaultDownloadBase. Tests point it at an httptest server; a GitHub
	// Enterprise install points it at its own web host.
	DownloadBase string

	// HTTPClient overrides the default client. The default deliberately
	// has NO total-request deadline: a release binary is ~10 MB and a Pi
	// on a slow link legitimately takes minutes. It bounds the part that
	// can actually hang — a server that accepts the connection and then
	// never answers — with ResponseHeaderTimeout.
	HTTPClient *http.Client

	// StallTimeout bounds how long a download may make NO progress before it
	// is abandoned. It is not a total deadline — a slow link is fine, a
	// stalled one is not — and it is what stops a dropped connection
	// mid-body from wedging a systemd timer forever. Defaults to
	// DefaultStallTimeout; a negative value disables the check.
	StallTimeout time.Duration

	// Stdout and Stderr default to the process's. Progress and results go
	// to Stdout, diagnostics to Stderr.
	Stdout io.Writer
	Stderr io.Writer

	// ExecPath overrides os.Executable. Tests use it; production does not.
	ExecPath func() (string, error)

	// Args are the CLI arguments AFTER the "update" subcommand word. nil
	// means take them from os.Args[2:], which is what a consumer that
	// dispatches `<tool> update ...` straight into Main wants. A consumer
	// with global flags BEFORE the subcommand (`tool -config x update -check`)
	// must set this explicitly — os.Args[2:] would be wrong there.
	Args []string

	// TargetVersion pins the release to install, e.g. "v0.19.0". Empty
	// means latest. The -version flag sets it when going through Main.
	TargetVersion string

	// CheckOnly resolves and reports without downloading or replacing
	// anything. The -check flag sets it when going through Main.
	CheckOnly bool
}

// Status is what Check resolved, without touching the installed binary.
type Status struct {
	// Current is the running version, with a leading v.
	Current string
	// Target is the resolved release tag, with a leading v.
	Target string
	// Available reports whether Target differs from Current. With
	// TargetVersion pinned this may be a downgrade — the field says
	// "different", not "newer", because installing a pin is legitimate.
	Available bool
	// Release is the resolved release, carrying the asset API URLs.
	Release *Release
}

// Result reports what an update run did, so the caller can decide follow-up
// actions such as recycling a service.
type Result struct {
	// Current is the version that was running, with a leading v.
	Current string
	// Target is the version resolved for installation, with a leading v.
	// Empty when ViaBrew is set (brew resolves the version itself) and
	// when the run failed before the release was resolved.
	Target string
	// Updated is true iff the binary was actually replaced (or upgraded
	// through brew).
	Updated bool
	// ExecPath is the resolved path of the binary, symlinks followed.
	ExecPath string
	// ViaBrew is true when the work was done by `brew upgrade` rather than
	// by downloading a release asset.
	ViaBrew bool
	// Available is set by a CheckOnly run that found a different release.
	// It is the machine-readable form of what -check prints.
	Available bool
}

// normalise fills defaults and rejects a Config that cannot work. It is
// called by every entry point, so a caller never has to.
func (c *Config) normalise() error {
	if c.Repo == "" {
		return fmt.Errorf("distkit: Config.Repo is required")
	}
	if !repoRe.MatchString(c.Repo) {
		return fmt.Errorf("distkit: bad repo %q: want owner/name", c.Repo)
	}
	if c.Binary == "" {
		return fmt.Errorf("distkit: Config.Binary is required")
	}
	if c.Version == "" {
		return fmt.Errorf("distkit: Config.Version is required")
	}
	if c.AssetStem == "" {
		c.AssetStem = c.Binary
	}
	if c.AssetTemplate == "" {
		c.AssetTemplate = DefaultAssetTemplate
	}
	if c.ChecksumsAsset == "" {
		c.ChecksumsAsset = DefaultChecksums
	}
	if c.ArmSuffix == "" {
		c.ArmSuffix = "armv6"
	}
	if c.APIBase == "" {
		c.APIBase = DefaultAPIBase
	}
	if c.DownloadBase == "" {
		c.DownloadBase = DefaultDownloadBase
	}
	// Both are joined with "/" + a path, so a configured trailing slash
	// would produce "//" in every URL built from them.
	c.APIBase = strings.TrimSuffix(c.APIBase, "/")
	c.DownloadBase = strings.TrimSuffix(c.DownloadBase, "/")
	if c.HTTPClient == nil {
		c.HTTPClient = defaultHTTPClient()
	}
	if c.StallTimeout == 0 {
		c.StallTimeout = DefaultStallTimeout
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.ExecPath == nil {
		c.ExecPath = os.Executable
	}
	if c.Token == "" && !c.tokenResolved {
		c.Token = DiscoverToken()
	}
	// Set unconditionally: a host with no token must not re-run discovery
	// (an exec of `gh`) on every nested call.
	c.tokenResolved = true
	return nil
}

// defaultHTTPClient bounds the hang, not the transfer. See Config.HTTPClient.
func defaultHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
}

// AssetName renders Config.AssetTemplate for the current platform and the
// given resolved tag.
func (c *Config) AssetName(tag string) string {
	return c.assetNameFor(tag, runtime.GOOS, runtime.GOARCH)
}

func (c *Config) assetNameFor(tag, goos, goarch string) string {
	v := EnsureV(tag)
	return strings.NewReplacer(
		"{stem}", c.AssetStem,
		"{binary}", c.Binary,
		"{os}", goos,
		"{arch}", c.assetArch(goarch),
		"{version}", v,
		"{version_no_v}", strings.TrimPrefix(v, "v"),
	).Replace(c.AssetTemplate)
}

// assetArch maps GOARCH to the arch token used in release asset names.
func (c *Config) assetArch(goarch string) string {
	if goarch == "arm" {
		return c.ArmSuffix
	}
	return goarch
}

// devVersions are the placeholder versions a non-release build carries.
var devVersions = map[string]bool{
	"dev": true, "devel": true, "(devel)": true, "unknown": true, "none": true, "snapshot": true,
}

// devSuffixes mark a version DERIVED from a release tag that is nonetheless
// not one: "0.1.0-dev" compiled into a working tree, or git-describe's
// "-dirty". A release prerelease ("-rc1", "-beta.2") is a real tag with real
// assets and is deliberately not listed.
var devSuffixes = []string{"-dev", "+dev", "-devel", "-snapshot", "-dirty"}

// IsDevBuild reports whether v is a placeholder or a working-tree build
// rather than a released tag.
//
// Self-updating one would rename a release binary over a developer's own
// build — it can never equal a tag, so every check reports "available" —
// which is a surprising thing to do to someone's working tree.
func IsDevBuild(v string) bool {
	v = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if devVersions[v] {
		return true
	}
	for _, s := range devSuffixes {
		if strings.HasSuffix(v, s) {
			return true
		}
	}
	return false
}

// EnsureV prepends "v" to a version string that lacks it, so that a tag
// written "0.4.1" and one written "v0.4.1" compare equal.
func EnsureV(s string) string {
	if s == "" || strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}
