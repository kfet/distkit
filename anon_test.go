package distkit

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeDownloads stands in for github.com (the download host, not the API):
// it 302s /<repo>/releases/latest to the release page and serves assets from
// /<repo>/releases/download/<tag>/<name>.
type fakeDownloads struct {
	*httptest.Server
	tag    string
	assets map[string][]byte
	// noRedirect makes /releases/latest answer 200 instead of redirecting,
	// which is how a GitHub Enterprise host or a test double that knows
	// nothing about releases behaves.
	noRedirect bool
	// redirectTo overrides the Location path, to drive a destination that
	// carries no tag.
	redirectTo string
	// hits counts every request, so a test can assert what was fetched.
	hits int
}

func newFakeDownloads(t *testing.T, repo, tag string, assets map[string][]byte) *fakeDownloads {
	t.Helper()
	f := &fakeDownloads{tag: tag, assets: assets}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		switch {
		case r.URL.Path == "/"+repo+"/releases/latest":
			if f.noRedirect {
				_, _ = w.Write([]byte("<html>no redirect here</html>"))
				return
			}
			loc := "/" + repo + "/releases/tag/" + f.tag
			if f.redirectTo != "" {
				loc = f.redirectTo
			}
			http.Redirect(w, r, loc, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/"+repo+"/releases/download/"+f.tag+"/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			data, ok := f.assets[name]
			if !ok {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
			_, _ = w.Write(data)
		default:
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// deadAPI is an API root that fails the test if it is called at all. The
// whole point of the anonymous path is that the 60/hour per-IP budget is
// never touched, and only a server that refuses to answer can prove it.
func deadAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the anonymous path must not call the GitHub API, but it requested %s", r.URL.Path)
		http.Error(w, "rate limit exceeded", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// anonConfig wires an anonymous Config at a fake download host, with the API
// pointed at a server that must never be reached.
func anonConfig(t *testing.T, d *fakeDownloads, exe string, out *bytes.Buffer) Config {
	return Config{
		Repo:          "kfet/testtool",
		Binary:        "testtool",
		Version:       "v1.0.0",
		APIBase:       deadAPI(t).URL,
		DownloadBase:  d.URL,
		Token:         "",
		tokenResolved: true, // do not sniff the developer's own environment
		Stdout:        out,
		Stderr:        out,
		ExecPath:      func() (string, error) { return exe, nil },
		HTTPClient:    d.Client(),
	}
}

// The bug this covers: a NAT'd fleet shares one unauthenticated API budget of
// 60 requests/hour, so `update` failed with a 403 that reads like a
// permissions error on hosts that had done nothing wrong.
func TestUpdateAnonymouslySpendsNoAPIQuota(t *testing.T) {
	asset := assetFor("testtool")
	newBytes := []byte("#!/bin/sh\necho new\n")
	assets := map[string][]byte{asset: newBytes}
	assets["checksums.txt"] = checksums(assets)
	d := newFakeDownloads(t, "kfet/testtool", "v1.1.0", assets)

	exe := installedBinary(t, "testtool")
	var out bytes.Buffer
	cfg := anonConfig(t, d, exe, &out)
	cfg.DisableBrew = true

	res, err := Update(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Update: %v (%s)", err, out.String())
	}
	if !res.Updated || res.Target != "v1.1.0" {
		t.Fatalf("got %+v", res)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBytes) {
		t.Fatalf("binary not replaced: %q", got)
	}
	if !strings.Contains(out.String(), "checksum verified") {
		t.Fatalf("the anonymous path must still verify checksums: %s", out.String())
	}
}

// A pinned version needs no resolution at all anonymously: the tag is known,
// so the only requests are the two asset downloads.
func TestAnonymousPinnedVersionSkipsResolution(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("pinned\n")}
	assets["checksums.txt"] = checksums(assets)
	d := newFakeDownloads(t, "kfet/testtool", "v0.9.0", assets)

	var out bytes.Buffer
	cfg := anonConfig(t, d, installedBinary(t, "testtool"), &out)
	cfg.TargetVersion = "v0.9.0"
	cfg.DisableBrew = true

	if _, err := Update(t.Context(), cfg); err != nil {
		t.Fatalf("Update: %v (%s)", err, out.String())
	}
	if d.hits != 2 {
		t.Fatalf("want exactly 2 requests (asset + checksums), got %d", d.hits)
	}
}

// The tag is taken from the redirect without a leading "v" being assumed, and
// a destination that is not a release page must not be installed as one.
func TestLatestTagFromRedirect(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeDownloads)
		want  string
	}{
		{"ordinary release", func(*fakeDownloads) {}, "v1.1.0"},
		{"no redirect falls through", func(d *fakeDownloads) { d.noRedirect = true }, ""},
		{"non-release destination", func(d *fakeDownloads) { d.redirectTo = "/login" }, ""},
		{"empty tag", func(d *fakeDownloads) { d.redirectTo = "/kfet/testtool/releases/tag/" }, ""},
		{"junk tag", func(d *fakeDownloads) { d.redirectTo = "/kfet/testtool/releases/tag/../x" }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newFakeDownloads(t, "kfet/testtool", "v1.1.0", nil)
			c.setup(d)
			var out bytes.Buffer
			cfg := anonConfig(t, d, "", &out)
			if err := cfg.normalise(); err != nil {
				t.Fatal(err)
			}
			if got := latestTagFromRedirect(t.Context(), &cfg); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// An unreachable download host is not fatal: the API fallback still runs, and
// it is the one that reports a real failure.
func TestAnonymousFallsBackToAPI(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("via api\n")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)

	cfg := Config{
		Repo:          "kfet/testtool",
		Binary:        "testtool",
		Version:       "v1.0.0",
		APIBase:       f.URL,
		DownloadBase:  "http://127.0.0.1:1", // nothing listens here
		tokenResolved: true,
		HTTPClient:    f.Client(),
	}
	rel, err := FetchRelease(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("FetchRelease: %v", err)
	}
	if rel.TagName != "v1.1.0" || len(rel.Assets) == 0 {
		t.Fatalf("want the API-resolved release with its asset list, got %+v", rel)
	}
}

// A bad request URL cannot be built through the public API, so drive it
// directly: the resolver must degrade to "" rather than panic.
func TestLatestTagFromRedirectBadURL(t *testing.T) {
	cfg := Config{Repo: "kfet/testtool", Binary: "testtool", Version: "v1", DownloadBase: "://nope"}
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	if got := latestTagFromRedirect(t.Context(), &cfg); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Anonymously the release is never looked up, so a typo'd -version first
// shows up as a 404 on the download. It has to say what that means.
func TestAnonymousMissingAssetExplainsItself(t *testing.T) {
	d := newFakeDownloads(t, "kfet/testtool", "v9.9.9", map[string][]byte{})
	var out bytes.Buffer
	cfg := anonConfig(t, d, installedBinary(t, "testtool"), &out)
	cfg.TargetVersion = "v9.9.9"
	cfg.DisableBrew = true

	_, err := Update(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "check the version tag") {
		t.Fatalf("want an explained 404, got %v", err)
	}
}

// A resolved release still carries a usable URL for an asset nobody listed.
func TestAssetURLFromDownloadBase(t *testing.T) {
	rel := &Release{TagName: "v1.0.0", downloadBase: "https://github.com/k/t/releases/download/v1.0.0"}
	got, err := rel.AssetURL("t-linux-amd64")
	if err != nil || got != "https://github.com/k/t/releases/download/v1.0.0/t-linux-amd64" {
		t.Fatalf("got (%q, %v)", got, err)
	}
}

// A tag is pasted into a URL path on both the API and the download host, so
// one that is not tag-shaped has to be refused rather than escaping to some
// other repo's release — where the checksum manifest would be fetched from
// the same wrong place and agree with itself.
func TestPinnedVersionIsValidated(t *testing.T) {
	for _, bad := range []string{
		"../../other/repo/releases/download/v1",
		"v1/../v2",
		"v1 v2",
		"-v1",
	} {
		t.Run(bad, func(t *testing.T) {
			for _, token := range []string{"", "seeded"} {
				cfg := Config{
					Repo: "kfet/testtool", Binary: "testtool", Version: "v1.0.0",
					APIBase: deadAPI(t).URL, DownloadBase: deadAPI(t).URL,
					Token: token, tokenResolved: true,
				}
				_, err := FetchRelease(t.Context(), cfg, bad)
				if err == nil || !strings.Contains(err.Error(), "bad version") {
					t.Fatalf("token=%q: want a rejection, got %v", token, err)
				}
			}
		})
	}
}

// A project that tags "1.2.3" without the leading v redirects to
// /releases/tag/1.2.3, and its assets live under that exact path. Rewriting
// the resolved tag to "v1.2.3" would 404 on every download.
func TestResolvedTagIsUsedVerbatim(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("no v prefix\n")}
	assets["checksums.txt"] = checksums(assets)
	d := newFakeDownloads(t, "kfet/testtool", "1.2.3", assets)

	var out bytes.Buffer
	cfg := anonConfig(t, d, installedBinary(t, "testtool"), &out)
	cfg.DisableBrew = true

	res, err := Update(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Update: %v (%s)", err, out.String())
	}
	if !res.Updated {
		t.Fatalf("got %+v (%s)", res, out.String())
	}
}

// A redirect that lands on a different repo is not this repo's latest
// release, whatever it looks like.
func TestRedirectToAnotherRepoIsRejected(t *testing.T) {
	d := newFakeDownloads(t, "kfet/testtool", "v1.1.0", nil)
	d.redirectTo = "/someone/else/releases/tag/v9.9.9"
	var out bytes.Buffer
	cfg := anonConfig(t, d, "", &out)
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	if got := latestTagFromRedirect(t.Context(), &cfg); got != "" {
		t.Fatalf("got %q", got)
	}
}

// A configured base with a trailing slash must not produce "//" in URLs.
func TestBaseURLsLoseTrailingSlash(t *testing.T) {
	cfg := Config{
		Repo: "kfet/testtool", Binary: "testtool", Version: "v1",
		APIBase: "https://api.example.com/", DownloadBase: "https://example.com/",
		tokenResolved: true,
	}
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	if cfg.APIBase != "https://api.example.com" || cfg.DownloadBase != "https://example.com" {
		t.Fatalf("got %q and %q", cfg.APIBase, cfg.DownloadBase)
	}
}

// A tag written without the leading v resolves to the same release.
func TestAnonReleaseNormalisesTag(t *testing.T) {
	cfg := Config{Repo: "kfet/testtool", Binary: "testtool", Version: "v1", tokenResolved: true}
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	rel := anonRelease(t.Context(), &cfg, "1.2.3")
	if rel == nil || rel.TagName != "v1.2.3" ||
		!strings.HasSuffix(rel.downloadBase, "/releases/download/v1.2.3") {
		t.Fatalf("got %+v", rel)
	}
}
