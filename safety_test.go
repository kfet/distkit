package distkit

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateRefusesDevBuild(t *testing.T) {
	// A dev build can never equal a tag, so every run would report an update
	// and then rename a release binary over the developer's own build.
	// The -dev suffix is the common case in practice: every consumer of this
	// package compiles a working tree as "<last tag>-dev", which is not a
	// placeholder and used to sail straight past this guard.
	for _, v := range []string{
		"dev", "DEV", "vdev", "(devel)", "unknown", "snapshot",
		"v0.1.0-dev", "0.69.1-DEV", "v1.2.3+dev", "v1.2.3-dirty", "v1.2.3-snapshot",
	} {
		cfg := Config{
			Repo:     "kfet/testtool",
			Binary:   "testtool",
			Version:  v,
			Token:    "seeded",
			Stdout:   &bytes.Buffer{},
			ExecPath: func() (string, error) { return "/nonexistent/testtool", nil },
		}
		_, err := Update(t.Context(), cfg)
		if err == nil || !strings.Contains(err.Error(), "not a release build") {
			t.Errorf("version %q: got %v", v, err)
		}
	}
	// A prerelease is a real tag with real assets; it must still update.
	for _, v := range []string{"v1.2.3", "0.1.0", "v1.2.3-rc1", "v2.0.0-beta.2", "v1.0.0-development"} {
		if IsDevBuild(v) {
			t.Errorf("%q is a real tag and must not be mistaken for a dev build", v)
		}
	}
}

func TestCheckWritableSuggestsSudoForARootOwnedDirectory(t *testing.T) {
	// /tmp is root-owned on every host this runs on, and is not a
	// package-manager prefix — the same shape as a root-owned
	// /usr/local/bin, which is what this project's own install.sh creates.
	restore := geteuid
	geteuid = func() int { return 4242 }
	t.Cleanup(func() { geteuid = restore })

	err := CheckWritable("/tmp/testtool", "testtool")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "sudo testtool update") {
		t.Fatalf("a root-owned install dir must point at sudo, not at a package manager: %v", err)
	}
}

func TestCheckWritableAllowsUsrLocalBin(t *testing.T) {
	// /usr/local/bin is where install.sh puts binaries and is owned by no
	// package manager; only the ownership check may refuse it.
	restore := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = restore })
	err := CheckWritable("/usr/local/bin/testtool", "testtool")
	if err != nil && strings.Contains(err.Error(), "package-manager prefix") {
		t.Fatalf("/usr/local/bin must not be treated as package-manager owned: %v", err)
	}
}

func TestTokenDiscoveryRunsOnce(t *testing.T) {
	// Update → Check → Download each normalise the config. On a host with no
	// token that must not exec `gh auth token` three times.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	calls := 0
	restore := tokenCommand
	tokenCommand = func() ([]byte, error) {
		calls++
		return nil, fmt.Errorf("gh: not logged in")
	}
	t.Cleanup(func() { tokenCommand = restore })

	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new\n")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)
	exe := installedBinary(t, "testtool")
	var out bytes.Buffer
	cfg := testConfig(f, exe, &out)
	cfg.Token = ""

	if _, err := Update(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("`gh auth token` ran %d times, want 1", calls)
	}
}

func TestUpdateSkipChecksums(t *testing.T) {
	asset := assetFor("testtool")
	// No checksums.txt in the release at all.
	assets := map[string][]byte{asset: []byte("unverified\n")}
	f := newFakeGitHub(t, "v1.1.0", assets)
	exe := installedBinary(t, "testtool")
	var out bytes.Buffer

	cfg := testConfig(f, exe, &out)
	if _, err := Update(t.Context(), cfg); err == nil {
		t.Fatal("without SkipChecksums a missing manifest must fail the update")
	}

	out.Reset()
	cfg = testConfig(f, exe, &out)
	cfg.SkipChecksums = true
	if _, err := Update(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "unverified\n" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(out.String(), "checksum verified") {
		t.Error("must not claim verification that did not happen")
	}
}

func TestDownloadAbandonsAStalledTransfer(t *testing.T) {
	// A connection that dies mid-body must not wedge the process. Without a
	// stall guard this test hangs until the test binary's own timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/assets/") {
			w.Header().Set("Content-Length", "1000000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			<-r.Context().Done() // never send the rest
			return
		}
		_, _ = fmt.Fprintf(w, `{"tag_name":"v1.1.0","assets":[{"name":%q,"url":%q}]}`,
			assetFor("testtool"), "http://"+r.Host+"/assets/bin")
	}))
	defer srv.Close()

	exe := installedBinary(t, "testtool")
	var out bytes.Buffer
	cfg := Config{
		Repo:          "kfet/testtool",
		Binary:        "testtool",
		Version:       "v1.0.0",
		Token:         "seeded",
		APIBase:       srv.URL,
		StallTimeout:  150 * time.Millisecond,
		SkipChecksums: true,
		Stdout:        &out,
		Stderr:        &out,
		ExecPath:      func() (string, error) { return exe, nil },
		HTTPClient:    srv.Client(),
	}

	done := make(chan error, 1)
	go func() {
		_, err := Update(t.Context(), cfg)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stalled") {
			t.Fatalf("got %v, want a stall report", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled download was not abandoned")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
		t.Fatalf("a partial download was installed: %q", got)
	}
}

func TestStallGuardDoesNotFireOnASlowButLiveTransfer(t *testing.T) {
	// Slow is fine; only the absence of progress is fatal.
	r := &drip{chunks: 6, gap: 30 * time.Millisecond}
	sg := stallGuard(r, 200*time.Millisecond)
	defer sg.stop()
	buf := make([]byte, 64)
	total := 0
	for {
		n, err := sg.Read(buf)
		total += n
		if err != nil {
			break
		}
	}
	if sg.stalled() {
		t.Fatal("a steady trickle must not be reported as stalled")
	}
	if total != 6 {
		t.Fatalf("read %d bytes", total)
	}
}

// drip yields one byte at a time with a gap between reads.
type drip struct {
	chunks int
	gap    time.Duration
}

func (d *drip) Read(p []byte) (int, error) {
	if d.chunks == 0 {
		return 0, fmt.Errorf("EOF")
	}
	time.Sleep(d.gap)
	d.chunks--
	p[0] = 'x'
	return 1, nil
}

func TestApplyLeavesNoStagingDirOnFailure(t *testing.T) {
	// A failed run must not litter the install directory with dot-dirs.
	exe := installedBinary(t, "testtool")
	dir := filepath.Dir(exe)
	assets := map[string][]byte{assetFor("testtool"): []byte("x")}
	f := newFakeGitHub(t, "v1.1.0", assets) // no checksums.txt → fails
	var out bytes.Buffer
	if _, err := Update(t.Context(), testConfig(f, exe, &out)); err == nil {
		t.Fatal("expected failure")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".testtool-update-") {
			t.Fatalf("staging dir %s survived a failed run", e.Name())
		}
	}
}
