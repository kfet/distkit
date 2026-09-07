package installsh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kfet/distkit"
)

func mustRender(t *testing.T, spec Spec) string {
	t.Helper()
	out, err := Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

// shellSyntaxOK runs `sh -n`, which parses the script without executing it.
// A template edit that breaks quoting is otherwise only discovered by a user
// piping it into their shell.
func shellSyntaxOK(t *testing.T, script string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-n", p).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n failed: %v\n%s", err, out)
	}
}

func TestRenderRequiresRepoAndBinary(t *testing.T) {
	if _, err := Render(Spec{Binary: "x"}); err == nil {
		t.Error("want an error without a repo")
	}
	if _, err := Render(Spec{Repo: "k/x"}); err == nil {
		t.Error("want an error without a binary")
	}
}

func TestRenderDefaults(t *testing.T) {
	s := mustRender(t, Spec{Repo: "kfet/zulip-acp", Binary: "zulip-acp"})
	shellSyntaxOK(t, s)

	for _, want := range []string{
		`REPO="${REPO:-kfet/zulip-acp}"`,
		`BIN_NAME="zulip-acp"`,
		`ASSET="zulip-acp-${OS}-${ARCH}"`,
		"echo armv6 ;;",          // family default for 32-bit ARM
		"sha256sum",              // verification is on unless asked off
		"Darwin) echo darwin ;;", // macOS included by default
		"set -eu",
		"if have curl;",
		"elif have wget;",
		`if [ -z "${BIN_DIR:-}" ] && [ -n "${PREFIX:-}" ]; then`, // legacy alias
		"is not on \\$PATH",
		"elif have sudo;",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered script is missing %q", want)
		}
	}
	for _, unwanted := range []string{"FreeBSD", "i386|i686", "tar -C", "next:"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("rendered script should not contain %q by default", unwanted)
		}
	}
}

func TestRenderCoversEveryDivergence(t *testing.T) {
	// One spec exercising every knob the six originals differed on.
	no := false
	s := mustRender(t, Spec{
		Repo:          "kfet/harb",
		Binary:        "harb",
		AssetStem:     "harb",
		AssetTemplate: "{stem}-{version_no_v}-{os}-{arch}.tar.gz",
		ArmSuffix:     "arm",
		Darwin:        &no,
		FreeBSD:       true,
		Arch386:       true,
		BrewFormula:   "kfet/tap/harb",
		VersionFlag:   "-version",
		ExampleTag:    "v0.4.0",
		RuntimeDeps:   []Dep{{Name: "tmux", Message: "tmux is not on $PATH; harb needs it"}},
		NextSteps:     []string{"harb init    # bootstrap config", "harb serve"},
	})
	shellSyntaxOK(t, s)

	for _, want := range []string{
		`ASSET="harb-${VERSION_NO_V}-${OS}-${ARCH}.tar.gz"`,
		"tar -C \"$tmpdir\" -xzf",
		"FreeBSD) echo freebsd ;;",
		"i386|i686)     echo 386 ;;",
		"echo arm ;;",
		"brew install kfet/tap/harb",
		"VERSION=v0.4.0 sh",
		"if ! have tmux; then",
		`warn 'tmux is not on $PATH; harb needs it'`, // single-quoted: no expansion
		`"$dest" -version`,
		`printf '  %s\n' 'harb init    # bootstrap config'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered script is missing %q", want)
		}
	}
	if strings.Contains(s, "Darwin)") {
		t.Error("Darwin was disabled but still appears in the OS table")
	}
}

func TestRenderNoChecksums(t *testing.T) {
	s := mustRender(t, Spec{Repo: "k/x", Binary: "x", NoChecksums: true})
	shellSyntaxOK(t, s)
	if strings.Contains(s, "checksum mismatch") {
		t.Error("verification should be absent when NoChecksums is set")
	}
	if strings.Contains(s, "one of sha256sum/shasum") {
		t.Error("the requirements line should not mention sha256 tools")
	}
}

func TestRenderRejectsShellInjection(t *testing.T) {
	for _, spec := range []Spec{
		{Repo: "k/x; rm -rf /", Binary: "x"},
		{Repo: "k/x", Binary: "x\"; id; \""},
		{Repo: "k/x", Binary: "x", ArmSuffix: "$(id)"},
		{Repo: "k/x", Binary: "x", VersionFlag: "-v `id`"},
		{Repo: "k/x", Binary: "x", BrewFormula: "a/b\nrm -rf /"},
	} {
		if _, err := Render(spec); err == nil {
			t.Errorf("spec %+v must be rejected: it interpolates into shell source", spec)
		}
	}
}

func TestRenderQuotesProseSafely(t *testing.T) {
	// A next step or dep message is prose and may legitimately contain a
	// dollar sign or a quote; it must be quoted, not rejected.
	s := mustRender(t, Spec{
		Repo:        "k/x",
		Binary:      "x",
		NextSteps:   []string{`x run --flag="$HOME/it's"`},
		RuntimeDeps: []Dep{{Name: "tmux", Message: `set $TMUX; it's needed`}},
	})
	shellSyntaxOK(t, s)
	if !strings.Contains(s, `'x run --flag="$HOME/it'\''s"'`) {
		t.Errorf("embedded quote not escaped:\n%s", s)
	}
	if _, err := Render(Spec{Repo: "k/x", Binary: "x", NextSteps: []string{"a\nb"}}); err == nil {
		t.Error("a multi-line next step must be rejected")
	}
}

func TestFromConfigKeepsAssetNamingInSync(t *testing.T) {
	cfg := distkit.Config{
		Repo:          "kfet/harb",
		Binary:        "harb",
		Version:       "v0.4.0",
		AssetTemplate: "{stem}-{version_no_v}-{os}-{arch}.tar.gz",
		ArmSuffix:     "arm",
	}
	spec := FromConfig(cfg)
	if spec.AssetTemplate != cfg.AssetTemplate || spec.ArmSuffix != cfg.ArmSuffix {
		t.Fatalf("spec %+v does not mirror the config", spec)
	}
	s := mustRender(t, spec)
	if !strings.Contains(s, `ASSET="harb-${VERSION_NO_V}-${OS}-${ARCH}.tar.gz"`) {
		t.Error("the generated script must compute the same asset name the binary does")
	}

	noSums := distkit.Config{Repo: "k/x", Binary: "x", Version: "v1", SkipChecksums: true}
	if !FromConfig(noSums).NoChecksums {
		t.Error("SkipChecksums must disable verification in the script too")
	}
}

func TestLoadSpec(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "install.sh.json")
	if err := os.WriteFile(p, []byte(`{"repo":"kfet/x","binary":"x","next_steps":["x help"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Repo != "kfet/x" || len(spec.NextSteps) != 1 {
		t.Fatalf("got %+v", spec)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"repo":"kfet/x","typo_field":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSpec(bad); err == nil {
		t.Error("an unknown field must be rejected: a silently ignored typo means a silently wrong script")
	}
	if _, err := LoadSpec(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("want an error for a missing spec")
	}
}

func TestWriteAndCheckDrift(t *testing.T) {
	spec := Spec{Repo: "kfet/x", Binary: "x"}
	p := filepath.Join(t.TempDir(), "install.sh")

	if err := CheckDrift(p, spec); err == nil || !strings.Contains(err.Error(), "make install.sh") {
		t.Fatalf("a missing file must say how to create it: %v", err)
	}
	if err := Write(p, spec); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("install.sh should be executable, got %v", fi.Mode())
	}
	if err := CheckDrift(p, spec); err != nil {
		t.Fatalf("a freshly generated file must not be drifted: %v", err)
	}

	// The whole point: a hand edit is caught, and the message names the line.
	body, _ := os.ReadFile(p)
	edited := strings.Replace(string(body), `BIN_NAME="x"`, `BIN_NAME="x-hacked"`, 1)
	if err := os.WriteFile(p, []byte(edited), 0o755); err != nil {
		t.Fatal(err)
	}
	err = CheckDrift(p, spec)
	if err == nil {
		t.Fatal("a hand edit must be detected")
	}
	if !strings.Contains(err.Error(), "first difference at line") || !strings.Contains(err.Error(), "x-hacked") {
		t.Errorf("drift error should locate the change: %v", err)
	}

	// A truncated file is caught too.
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckDrift(p, spec); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Errorf("got %v", err)
	}

	// A file that is a strict prefix of the template reports the length
	// difference, since there is no differing line to point at.
	full, _ := Render(spec)
	head := strings.SplitN(string(full), "\n", 4)
	if err := os.WriteFile(p, []byte(strings.Join(head[:3], "\n")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckDrift(p, spec); err == nil || !strings.Contains(err.Error(), "lines") {
		t.Errorf("got %v", err)
	}
}

// TestGeneratedScriptInstallsEndToEnd actually runs the generated installer
// against a fake GitHub. It is the only way to know the script works: `sh -n`
// proves it parses, not that it installs.
func TestGeneratedScriptInstallsEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("no sha256 tool available")
		}
	}

	payload := []byte("#!/bin/sh\necho installed-ok\n")
	asset := "testtool-linux-amd64"
	h := sha256.Sum256(payload)
	manifest := []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), asset))
	files := map[string][]byte{asset: payload, "checksums.txt": manifest}

	// files is mutated by a subtest (the checksum-mismatch case) while the
	// server goroutine is still serving, so every access goes through the
	// lock. A map read racing a map write is not merely unsynchronised — it
	// can abort the process outright.
	var filesMu sync.RWMutex
	fileBytes := func(name string) ([]byte, bool) {
		filesMu.RLock()
		defer filesMu.RUnlock()
		data, ok := files[name]
		return data, ok
	}
	setFile := func(name string, data []byte) {
		filesMu.Lock()
		defer filesMu.Unlock()
		files[name] = data
	}

	// Asset ids are assigned once, up front, rather than in the handler:
	// concurrent requests would otherwise write this map while another read
	// it, and the ids would differ between two /releases/latest calls.
	names := slices.Sorted(maps.Keys(files))
	assetIDs := map[string]string{}
	idFor := map[string]string{}
	for i, name := range names {
		id := fmt.Sprint(101 + i)
		assetIDs[id] = name
		idFor[name] = id
	}

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Anything not under this repo's path is a 404, so a bogus
		// GITHUB_API really does fail.
		if !strings.HasPrefix(r.URL.Path, "/repos/kfet/testtool/") &&
			!strings.HasPrefix(r.URL.Path, "/kfet/testtool/releases/download/") {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			// Real GitHub asset URLs end in a numeric id, and the
			// script's sed pattern relies on that.
			var assets []map[string]string
			for _, name := range names {
				assets = append(assets, map[string]string{
					"name": name,
					"url":  srv.URL + "/repos/kfet/testtool/releases/assets/" + idFor[name],
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.2.3", "assets": assets})
		case strings.Contains(r.URL.Path, "/releases/assets/"):
			name := assetIDs[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if data, ok := fileBytes(name); ok {
				_, _ = w.Write(data)
				return
			}
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/releases/download/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if data, ok := fileBytes(name); ok {
				_, _ = w.Write(data)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	script := mustRender(t, Spec{Repo: "kfet/testtool", Binary: "testtool", VersionFlag: ""})
	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")

	run := func(extraEnv ...string) (string, error) {
		cmd := exec.Command("sh", path)
		cmd.Env = append(os.Environ(),
			"GITHUB_API="+srv.URL,
			"GITHUB_HOST="+srv.URL,
			"BIN_DIR="+binDir,
			"OS=linux",
			"ARCH=amd64",
			"GITHUB_TOKEN=",
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("a VERSION that is not a tag is refused", func(t *testing.T) {
		// It would otherwise be pasted into two URL paths, and the checksum
		// manifest fetched from the same wrong place would agree with the
		// wrong binary.
		for _, bad := range []string{"../../other/repo/releases/download/v1", "v1 v2", "-v1"} {
			out, err := run("VERSION=" + bad)
			if err == nil {
				t.Fatalf("VERSION=%q was accepted:\n%s", bad, out)
			}
			if !strings.Contains(out, "bad VERSION") {
				t.Errorf("VERSION=%q: output:\n%s", bad, out)
			}
		}
	})

	t.Run("anonymous download path", func(t *testing.T) {
		out, err := run()
		if err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(filepath.Join(binDir, "testtool"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(payload) {
			t.Fatalf("installed %q", got)
		}
		fi, _ := os.Stat(filepath.Join(binDir, "testtool"))
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("installed binary is not executable: %v", fi.Mode())
		}
		if !strings.Contains(out, "checksum ok") {
			t.Errorf("no checksum confirmation:\n%s", out)
		}
	})

	t.Run("token path resolves the API asset url", func(t *testing.T) {
		// With a token the script must pick the asset's API URL out of the
		// release JSON — the branch a private repo depends on.
		out, err := run("GITHUB_TOKEN=tok-abc")
		if err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}
		got, _ := os.ReadFile(filepath.Join(binDir, "testtool"))
		if string(got) != string(payload) {
			t.Fatalf("installed %q", got)
		}
	})

	t.Run("PREFIX is honoured as a legacy alias", func(t *testing.T) {
		prefix := filepath.Join(dir, "legacy")
		cmd := exec.Command("sh", path)
		cmd.Env = append(os.Environ(),
			"GITHUB_API="+srv.URL, "GITHUB_HOST="+srv.URL,
			"PREFIX="+prefix, "OS=linux", "ARCH=amd64", "GITHUB_TOKEN=", "BIN_DIR=")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(prefix, "bin", "testtool")); err != nil {
			t.Fatalf("PREFIX did not resolve to $PREFIX/bin: %v", err)
		}
	})

	t.Run("checksum mismatch aborts", func(t *testing.T) {
		setFile("checksums.txt", []byte(strings.Repeat("0", 64)+"  "+asset+"\n"))
		defer setFile("checksums.txt", manifest)
		out, err := run("BIN_DIR=" + filepath.Join(dir, "bad"))
		if err == nil {
			t.Fatalf("a bad checksum must fail the install:\n%s", out)
		}
		if !strings.Contains(out, "checksum mismatch") {
			t.Errorf("output:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "bad", "testtool")); err == nil {
			t.Error("nothing may be installed after a checksum failure")
		}
	})

	t.Run("unresolvable release aborts and explains itself", func(t *testing.T) {
		cmd := exec.Command("sh", path)
		cmd.Env = append(os.Environ(),
			"GITHUB_API="+srv.URL+"/nowhere", "GITHUB_HOST="+srv.URL,
			"BIN_DIR="+filepath.Join(dir, "none"), "OS=linux", "ARCH=amd64", "GITHUB_TOKEN=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("want a failure:\n%s", out)
		}
		// The two real causes are a private repo and a spent per-IP
		// unauthenticated rate limit. Under `set -e` the download used to
		// abort the script with curl's bare "error: 22" before any of that
		// could be said, which sends an operator hunting in the wrong place.
		if !strings.Contains(string(out), "GITHUB_TOKEN") {
			t.Errorf("the failure must name the token, got:\n%s", out)
		}
	})
}

// TestGeneratedArchiveScriptInstallsEndToEnd covers the tarball branch, which
// is harb's shape and the one the raw-binary consumers never exercise.
func TestGeneratedArchiveScriptInstallsEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	payload := []byte("#!/bin/sh\necho archived-ok\n")
	dir := t.TempDir()

	// Build a real tarball with the binary one directory down.
	stage := filepath.Join(dir, "stage", "testtool-1.2.3-linux-amd64")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "testtool"), payload, 0o755); err != nil {
		t.Fatal(err)
	}
	tgz := filepath.Join(dir, "asset.tar.gz")
	cmd := exec.Command("tar", "-czf", tgz, "-C", filepath.Join(dir, "stage"), "testtool-1.2.3-linux-amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("tar unavailable: %v %s", err, out)
	}
	tgzBytes, err := os.ReadFile(tgz)
	if err != nil {
		t.Fatal(err)
	}
	asset := "testtool-1.2.3-linux-amd64.tar.gz"
	h := sha256.Sum256(tgzBytes)
	files := map[string][]byte{
		asset:           tgzBytes,
		"checksums.txt": []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), asset)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.2.3"})
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if data, ok := files[name]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	script := mustRender(t, Spec{
		Repo:          "kfet/testtool",
		Binary:        "testtool",
		AssetTemplate: "{stem}-{version_no_v}-{os}-{arch}.tar.gz",
	})
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	run := exec.Command("sh", path)
	run.Env = append(os.Environ(),
		"GITHUB_API="+srv.URL, "GITHUB_HOST="+srv.URL,
		"BIN_DIR="+binDir, "OS=linux", "ARCH=amd64", "GITHUB_TOKEN=")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(binDir, "testtool"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed %q", got)
	}
}

// os.WriteFile applies its mode only when it creates the file, so
// regenerating over an existing non-executable copy must still leave a
// script the repo can run.
func TestWriteRestoresTheExecutableBit(t *testing.T) {
	spec := Spec{Repo: "kfet/x", Binary: "x"}
	p := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(p, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, spec); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("regenerated install.sh is not executable: %v", fi.Mode())
	}
}

// TestAnonymousResolveSurvivesASpentAPIRateLimit pins the reason the
// redirect path exists: GitHub's unauthenticated REST limit is 60/hour per
// IP address, and a NAT'd fleet or a CI runner routinely arrives with it
// already spent. An anonymous `curl … | sh` for a public repo must still
// work — it resolves "latest" from the releases/latest redirect on the
// download host and never touches the API.
func TestAnonymousResolveSurvivesASpentAPIRateLimit(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	payload := []byte("#!/bin/sh\necho anon-ok\n")
	asset := "testtool-linux-amd64"
	files := map[string][]byte{asset: payload}
	h := sha256.Sum256(payload)
	files["checksums.txt"] = []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), asset))

	// Counted from the server goroutine and read by the test goroutine, so
	// it has to be atomic rather than merely eventually-correct.
	var apiHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			apiHits.Add(1)
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			http.Redirect(w, r, "/kfet/testtool/releases/tag/v1.2.3", http.StatusFound)
		case strings.Contains(r.URL.Path, "/releases/tag/"):
			_, _ = w.Write([]byte("<html>release page</html>"))
		case strings.Contains(r.URL.Path, "/releases/download/"):
			if !strings.Contains(r.URL.Path, "/v1.2.3/") {
				http.Error(w, "wrong tag: "+r.URL.Path, http.StatusNotFound)
				return
			}
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if data, ok := files[name]; ok {
				_, _ = w.Write(data)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(mustRender(t, Spec{Repo: "kfet/testtool", Binary: "testtool"})), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(),
		"GITHUB_API="+srv.URL, "GITHUB_HOST="+srv.URL,
		"BIN_DIR="+binDir, "OS=linux", "ARCH=amd64", "GITHUB_TOKEN=", "PREFIX=", "VERSION=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install failed with the API rate-limited: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(binDir, "testtool")); string(got) != string(payload) {
		t.Fatalf("installed %q", got)
	}
	if n := apiHits.Load(); n != 0 {
		t.Errorf("anonymous install spent %d API request(s); it must not touch the API", n)
	}
}

// TestRedirectTagWithASlashIsNotTruncated pins a silent-wrong-install: the
// curl branch used to reduce the redirect URL to its last path segment, so a
// latest release tagged "release/v1" resolved to "v1". When a DIFFERENT tag
// "v1" also exists with its own assets and checksums, nothing fails — the
// wrong binary installs quietly. The tag must survive whole so the guard can
// reject it and fall back to the API.
func TestRedirectTagWithASlashIsNotTruncated(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	asset := "testtool-linux-amd64"
	build := func(body string) map[string][]byte {
		payload := []byte("#!/bin/sh\necho " + body + "\n")
		h := sha256.Sum256(payload)
		return map[string][]byte{
			asset:           payload,
			"checksums.txt": []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), asset)),
		}
	}
	// Both tags are complete and self-consistent, so a truncated tag
	// installs the wrong binary without any error to notice.
	byTag := map[string]map[string][]byte{"release/v1": build("correct"), "v1": build("WRONG")}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/kfet/testtool/releases/latest"):
			_, _ = w.Write([]byte(`{"tag_name":"release/v1","assets":[]}`))
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			http.Redirect(w, r, "/kfet/testtool/releases/tag/release/v1", http.StatusFound)
		case strings.Contains(r.URL.Path, "/releases/tag/"):
			_, _ = w.Write([]byte("<html>release page</html>"))
		case strings.Contains(r.URL.Path, "/releases/download/"):
			rest := r.URL.Path[strings.Index(r.URL.Path, "/releases/download/")+len("/releases/download/"):]
			cut := strings.LastIndex(rest, "/")
			files, ok := byTag[rest[:cut]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if data, ok := files[rest[cut+1:]]; ok {
				_, _ = w.Write(data)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(mustRender(t, Spec{Repo: "kfet/testtool", Binary: "testtool"})), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(),
		"GITHUB_API="+srv.URL, "GITHUB_HOST="+srv.URL,
		"BIN_DIR="+binDir, "OS=linux", "ARCH=amd64", "GITHUB_TOKEN=", "PREFIX=", "VERSION=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(binDir, "testtool"))
	if !strings.Contains(string(got), "correct") {
		t.Fatalf("installed the wrong tag's binary: %q", got)
	}
}
