package distkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGitHub is a stand-in for the GitHub REST API: it serves one release and
// its assets, over the same two-step shape the real thing uses (resolve the
// release, then GET the asset's API URL with Accept: octet-stream).
type fakeGitHub struct {
	*httptest.Server
	// tag is the release the server reports.
	tag string
	// assets maps asset name to its bytes.
	assets map[string][]byte
	// private makes every request without a bearer token 404, exactly as a
	// private repo does.
	private bool
	// seenAuth records the Authorization header of the last request.
	seenAuth string
	// missing names assets to omit from the release JSON.
	missing map[string]bool
	// truncate names assets served with a Content-Length longer than the
	// body, so the client sees an unexpected EOF mid-copy.
	truncate map[string]bool
	// status overrides the response code for the release lookup.
	status int
	// body overrides the release lookup response entirely.
	body string
}

func newFakeGitHub(t *testing.T, tag string, assets map[string][]byte) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tag: tag, assets: assets, missing: map[string]bool{}, truncate: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.seenAuth = r.Header.Get("Authorization")
		if f.private && f.seenAuth == "" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			name := strings.TrimPrefix(r.URL.Path, "/assets/")
			data, ok := f.assets[name]
			if !ok {
				http.Error(w, "no such asset", http.StatusNotFound)
				return
			}
			if f.truncate[name] {
				w.Header().Set("Content-Length", fmt.Sprint(len(data)+64))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
			_, _ = w.Write(data)
		case strings.Contains(r.URL.Path, "/releases/"):
			if f.status != 0 {
				http.Error(w, "boom", f.status)
				return
			}
			if f.body != "" {
				_, _ = w.Write([]byte(f.body))
				return
			}
			if strings.Contains(r.URL.Path, "/tags/") &&
				!strings.HasSuffix(r.URL.Path, "/tags/"+f.tag) {
				http.Error(w, "no such tag", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(f.release())
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) release() map[string]any {
	var assets []map[string]string
	for name := range f.assets {
		if f.missing[name] {
			continue
		}
		assets = append(assets, map[string]string{
			"name": name,
			"url":  f.URL + "/assets/" + name,
		})
	}
	return map[string]any{"tag_name": f.tag, "assets": assets}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// checksums renders a goreleaser-style manifest for the given assets.
func checksums(assets map[string][]byte) []byte {
	var b strings.Builder
	for name, data := range assets {
		if name == "checksums.txt" {
			continue
		}
		fmt.Fprintf(&b, "%s  %s\n", sum(data), name)
	}
	return []byte(b.String())
}

// installedBinary writes a fake "currently installed" binary in a temp dir
// and returns its path. The dir is owned by the test user, so the ownership
// guard passes.
func installedBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("old binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// testConfig wires a Config at the fake server and the fake install.
func testConfig(f *fakeGitHub, exe string, out *bytes.Buffer) Config {
	return Config{
		Repo:       "kfet/testtool",
		Binary:     "testtool",
		Version:    "v1.0.0",
		APIBase:    f.URL,
		Token:      "seeded", // skip DiscoverToken's environment sniffing
		Stdout:     out,
		Stderr:     out,
		ExecPath:   func() (string, error) { return exe, nil },
		HTTPClient: f.Client(),
	}
}

// assetFor names the asset the running platform would download.
func assetFor(stem string) string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv6"
	}
	return fmt.Sprintf("%s-%s-%s", stem, runtime.GOOS, arch)
}

func TestUpdateReplacesBinaryAtomically(t *testing.T) {
	asset := assetFor("testtool")
	newBytes := []byte("#!/bin/sh\necho new\n")
	assets := map[string][]byte{asset: newBytes}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)

	exe := installedBinary(t, "testtool")
	// A hard link to the old inode proves the swap was a rename rather than
	// an in-place rewrite: the link must still see the OLD contents.
	old := exe + ".oldinode"
	if err := os.Link(exe, old); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Update(t.Context(), testConfig(f, exe, &out))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.Updated || res.Target != "v1.1.0" || res.Current != "v1.0.0" {
		t.Fatalf("unexpected result %+v", res)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBytes) {
		t.Fatalf("binary not replaced: %q", got)
	}
	if stale, _ := os.ReadFile(old); string(stale) != "old binary\n" {
		t.Fatalf("old inode was overwritten in place: %q — the swap must be a rename", stale)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replacement is not executable: %v", fi.Mode())
	}
	// The staging directory must not survive a successful run.
	entries, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".testtool-update-") {
			t.Fatalf("staging dir %s left behind", e.Name())
		}
	}
	if !strings.Contains(out.String(), "checksum verified") {
		t.Errorf("no checksum confirmation in output: %q", out.String())
	}
}

func TestUpdateArchiveAsset(t *testing.T) {
	// harb's shape: a .tar.gz whose binary is nested one directory down.
	binBytes := []byte("#!/bin/sh\necho harb\n")
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		body []byte
		typ  byte
	}{
		{"testtool-1.1.0-linux-amd64/README", []byte("not the binary"), tar.TypeReg},
		{"testtool-1.1.0-linux-amd64/testtool", binBytes, tar.TypeReg},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: e.typ,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	cfgTemplate := "{stem}-{version_no_v}-{os}-{arch}.tar.gz"
	exe := installedBinary(t, "testtool")
	var out bytes.Buffer

	// Build the asset name the way the config will.
	probe := Config{Repo: "k/t", Binary: "testtool", Version: "v1", AssetTemplate: cfgTemplate}
	if err := probe.normalise(); err != nil {
		t.Fatal(err)
	}
	asset := probe.AssetName("v1.1.0")

	assets := map[string][]byte{asset: tgz.Bytes()}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)

	cfg := testConfig(f, exe, &out)
	cfg.AssetTemplate = cfgTemplate
	res, err := Update(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.Updated {
		t.Fatal("not updated")
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, binBytes) {
		t.Fatalf("wrong file extracted: %q", got)
	}
}

func TestUpdateAlreadyCurrentAndPinnedVersion(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.0.0", assets)

	exe := installedBinary(t, "testtool")
	var out bytes.Buffer
	res, err := Update(t.Context(), testConfig(f, exe, &out))
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatal("updated despite matching version")
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("output %q", out.String())
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Fatalf("binary touched: %q", got)
	}

	// Pinning the same tag without the leading v must compare equal.
	out.Reset()
	cfg := testConfig(f, exe, &out)
	cfg.TargetVersion = "1.0.0"
	if _, err := Update(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("pinned run: %q", out.String())
	}
}

func TestUpdateCheckOnly(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v2.0.0", assets)

	exe := installedBinary(t, "testtool")
	var out bytes.Buffer
	cfg := testConfig(f, exe, &out)
	cfg.CheckOnly = true
	res, err := Update(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatal("-check must not install")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Fatalf("binary touched: %q", got)
	}
	if !strings.Contains(out.String(), "update available: v1.0.0 → v2.0.0") {
		t.Errorf("output %q", out.String())
	}
}

func TestCheckDoesNotTouchDisk(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v2.0.0", assets)
	exe := installedBinary(t, "testtool")
	var out bytes.Buffer

	st, err := Check(t.Context(), testConfig(f, exe, &out))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Available || st.Target != "v2.0.0" || st.Current != "v1.0.0" {
		t.Fatalf("unexpected status %+v", st)
	}
	if st.Release == nil || len(st.Release.Assets) == 0 {
		t.Fatal("status must carry the resolved release")
	}
	if out.Len() != 0 {
		t.Errorf("Check must be silent, wrote %q", out.String())
	}
}

func TestUpdateChecksumMismatch(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{
		asset:           []byte("tampered payload"),
		"checksums.txt": []byte(strings.Repeat("0", 64) + "  " + assetFor("testtool") + "\n"),
	}
	f := newFakeGitHub(t, "v1.1.0", assets)
	exe := installedBinary(t, "testtool")
	var out bytes.Buffer

	_, err := Update(t.Context(), testConfig(f, exe, &out))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Fatalf("binary replaced despite bad checksum: %q", got)
	}
}

func TestUpdatePrivateRepoNeedsToken(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)
	f.private = true

	exe := installedBinary(t, "testtool")
	var out bytes.Buffer
	cfg := testConfig(f, exe, &out)
	cfg.Token = ""
	// Keep DiscoverToken from finding a real token on the dev machine.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	restore := tokenCommand
	tokenCommand = func() ([]byte, error) { return nil, fmt.Errorf("no gh") }
	t.Cleanup(func() { tokenCommand = restore })

	_, err := Update(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("want the private-repo hint, got %v", err)
	}

	// With a token the same server serves everything, asset bytes included.
	out.Reset()
	t.Setenv("GITHUB_TOKEN", "tok-123")
	if _, err := Update(t.Context(), testConfigWithEnvToken(f, exe, &out)); err != nil {
		t.Fatalf("with token: %v", err)
	}
	if f.seenAuth != "Bearer tok-123" {
		t.Fatalf("asset download was not authenticated: %q", f.seenAuth)
	}
}

func testConfigWithEnvToken(f *fakeGitHub, exe string, out *bytes.Buffer) Config {
	cfg := testConfig(f, exe, out)
	cfg.Token = ""
	return cfg
}

func TestUpdateRefusesBrewPrefix(t *testing.T) {
	// A /usr/local/Homebrew path has no Cellar component, so brew detection
	// declines to claim it and the managed-prefix backstop must catch it.
	cfg := Config{
		Repo:     "kfet/testtool",
		Binary:   "testtool",
		Version:  "v1.0.0",
		Stdout:   &bytes.Buffer{},
		ExecPath: func() (string, error) { return "/usr/local/Homebrew/bin/testtool", nil },
	}
	_, err := Update(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "Homebrew prefix") {
		t.Fatalf("want a Homebrew-prefix refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "not a keg") {
		t.Errorf("suggesting `brew upgrade` for a non-keg would send the operator "+
			"to a formula that does not exist: %v", err)
	}
}

func TestUpdateRefusesSystemPrefix(t *testing.T) {
	for _, p := range []string{"/usr/bin/testtool", "/usr/sbin/testtool", "/bin/testtool"} {
		cfg := Config{
			Repo:     "kfet/testtool",
			Binary:   "testtool",
			Version:  "v1.0.0",
			Stdout:   &bytes.Buffer{},
			ExecPath: func() (string, error) { return p, nil },
		}
		_, err := Update(t.Context(), cfg)
		if err == nil || !strings.Contains(err.Error(), "package manager") {
			t.Fatalf("%s: want a package-manager refusal, got %v", p, err)
		}
	}
}

func TestCheckWritableRefusesForeignDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "testtool")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := geteuid
	geteuid = func() int { return 999999 } // never the owner of a t.TempDir
	t.Cleanup(func() { geteuid = restore })

	err := CheckWritable(p, "testtool")
	if err == nil || !strings.Contains(err.Error(), "not you") {
		t.Fatalf("want an ownership refusal, got %v", err)
	}
	var managed *ErrManaged
	if !errors.As(err, &managed) {
		t.Fatal("refusal must be an *ErrManaged so callers can distinguish it")
	}
	if managed.Hint == "" {
		t.Error("an ownership refusal must tell the operator what to do instead")
	}
}

func TestCheckWritableVanishedDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone", "testtool")
	err := CheckWritable(p, "testtool")
	if err == nil || !strings.Contains(err.Error(), "cannot stat install dir") {
		t.Fatalf("want a stat failure, got %v", err)
	}
}

func TestUpdateRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no repo", Config{Binary: "x", Version: "v1"}, "Repo is required"},
		{"bad repo", Config{Repo: "not a repo", Binary: "x", Version: "v1"}, "want owner/name"},
		{"path traversal repo", Config{Repo: "kfet/x/../../y", Binary: "x", Version: "v1"}, "want owner/name"},
		{"no binary", Config{Repo: "k/x", Version: "v1"}, "Binary is required"},
		{"no version", Config{Repo: "k/x", Binary: "x"}, "Version is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Update(t.Context(), tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestUpdateFollowsSymlink(t *testing.T) {
	// ~/.local/bin/tool -> /opt/tools/tool-1.0.0/tool is a common layout;
	// replacing the symlink instead of its target would be a real bug.
	asset := assetFor("testtool")
	newBytes := []byte("new binary\n")
	assets := map[string][]byte{asset: newBytes}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)

	real := installedBinary(t, "testtool")
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "testtool")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cfg := testConfig(f, link, &out)
	res, err := Update(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExecPath != real {
		t.Fatalf("ExecPath = %s, want the symlink target %s", res.ExecPath, real)
	}
	if got, _ := os.ReadFile(real); !bytes.Equal(got, newBytes) {
		t.Fatalf("target not replaced: %q", got)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink itself was replaced")
	}
}

func TestUpdateToleratesUnresolvablePath(t *testing.T) {
	// os.Executable can name a path that no longer resolves (a deleted
	// upgrade directory). EvalSymlinks fails; the run must still produce a
	// sensible refusal rather than crash.
	cfg := Config{
		Repo:     "kfet/testtool",
		Binary:   "testtool",
		Version:  "v1.0.0",
		Stdout:   &bytes.Buffer{},
		ExecPath: func() (string, error) { return "/nonexistent/deleted/testtool", nil },
	}
	_, err := Update(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot stat install dir") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateExecPathError(t *testing.T) {
	cfg := Config{
		Repo:     "kfet/testtool",
		Binary:   "testtool",
		Version:  "v1.0.0",
		Stdout:   &bytes.Buffer{},
		ExecPath: func() (string, error) { return "", fmt.Errorf("no exe") },
	}
	if _, err := Update(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "locate self") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateAPIErrors(t *testing.T) {
	exe := installedBinary(t, "testtool")
	asset := assetFor("testtool")

	t.Run("non-200", func(t *testing.T) {
		f := newFakeGitHub(t, "v1.1.0", map[string][]byte{})
		f.status = http.StatusInternalServerError
		var out bytes.Buffer
		_, err := Update(t.Context(), testConfig(f, exe, &out))
		if err == nil || !strings.Contains(err.Error(), "github api") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		f := newFakeGitHub(t, "v1.1.0", map[string][]byte{})
		f.body = "{not json"
		var out bytes.Buffer
		_, err := Update(t.Context(), testConfig(f, exe, &out))
		if err == nil || !strings.Contains(err.Error(), "resolve release") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("empty tag", func(t *testing.T) {
		f := newFakeGitHub(t, "v1.1.0", map[string][]byte{})
		f.body = `{"tag_name":"","assets":[]}`
		var out bytes.Buffer
		_, err := Update(t.Context(), testConfig(f, exe, &out))
		if err == nil || !strings.Contains(err.Error(), "empty tag_name") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("missing binary asset", func(t *testing.T) {
		assets := map[string][]byte{asset: []byte("new")}
		assets["checksums.txt"] = checksums(assets)
		f := newFakeGitHub(t, "v1.1.0", assets)
		f.missing[asset] = true
		var out bytes.Buffer
		_, err := Update(t.Context(), testConfig(f, exe, &out))
		if err == nil || !strings.Contains(err.Error(), "has no asset") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("missing checksums asset", func(t *testing.T) {
		assets := map[string][]byte{asset: []byte("new")}
		assets["checksums.txt"] = checksums(assets)
		f := newFakeGitHub(t, "v1.1.0", assets)
		f.missing["checksums.txt"] = true
		var out bytes.Buffer
		_, err := Update(t.Context(), testConfig(f, exe, &out))
		if err == nil || !strings.Contains(err.Error(), "has no asset") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("no checksum entry", func(t *testing.T) {
		assets := map[string][]byte{asset: []byte("new"), "checksums.txt": []byte("deadbeef  other-file\n")}
		f := newFakeGitHub(t, "v1.1.0", assets)
		var out bytes.Buffer
		_, err := Update(t.Context(), testConfig(f, exe, &out))
		if err == nil || !strings.Contains(err.Error(), "no checksum entry") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("truncated body", func(t *testing.T) {
		assets := map[string][]byte{asset: []byte("new payload")}
		assets["checksums.txt"] = checksums(assets)
		f := newFakeGitHub(t, "v1.1.0", assets)
		f.truncate[asset] = true
		var out bytes.Buffer
		_, err := Update(t.Context(), testConfig(f, exe, &out))
		if err == nil || !strings.Contains(err.Error(), "download asset") {
			t.Fatalf("got %v", err)
		}
		if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
			t.Fatalf("binary replaced from a truncated download: %q", got)
		}
	})

	t.Run("pinned tag that does not exist", func(t *testing.T) {
		assets := map[string][]byte{asset: []byte("new")}
		assets["checksums.txt"] = checksums(assets)
		f := newFakeGitHub(t, "v1.1.0", assets)
		var out bytes.Buffer
		cfg := testConfig(f, exe, &out)
		cfg.TargetVersion = "v9.9.9"
		_, err := Update(t.Context(), cfg)
		if err == nil || !strings.Contains(err.Error(), "resolve release") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDownloadNeedsARelease(t *testing.T) {
	cfg := Config{Repo: "k/x", Binary: "x", Version: "v1"}
	if _, err := Download(t.Context(), cfg, nil, t.TempDir()); err == nil {
		t.Fatal("want an error for a nil release")
	}
}

func TestDownloadWriteError(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)
	var out bytes.Buffer
	cfg := testConfig(f, installedBinary(t, "testtool"), &out)
	rel, err := FetchRelease(t.Context(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	// A staging path that is not a directory: the create must fail.
	_, err = Download(t.Context(), cfg, rel, filepath.Join(t.TempDir(), "not-a-dir"))
	if err == nil || !strings.Contains(err.Error(), "download asset") {
		t.Fatalf("got %v", err)
	}
}

func TestApplyAcrossFilesystemsIsReported(t *testing.T) {
	// Renaming a path that does not exist is the cheapest way to prove the
	// error is wrapped with the target, which is what an operator needs.
	err := Apply(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "target"))
	if err == nil || !strings.Contains(err.Error(), "replace ") {
		t.Fatalf("got %v", err)
	}
}

func TestStagingDirIsASiblingOfTheTarget(t *testing.T) {
	exe := installedBinary(t, "testtool")
	dir, err := StagingDir(exe, "testtool")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != filepath.Dir(exe) {
		t.Fatalf("staging dir %s is not beside %s — the rename would not be atomic", dir, exe)
	}
	if !strings.HasPrefix(filepath.Base(dir), ".") {
		t.Errorf("staging dir %s should be hidden", dir)
	}
}
