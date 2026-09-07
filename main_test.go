package distkit

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMainUpdatesAndPrintsTheRecycleHint(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new\n")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)
	exe := installedBinary(t, "testtool")

	var out bytes.Buffer
	cfg := testConfig(f, exe, &out)
	cfg.RestartHint = "systemctl --user reload testtool"
	cfg.Args = []string{}
	if code := Main(cfg); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "systemctl --user reload testtool") {
		t.Errorf("a swapped binary is inert until a recycle; the hint must be printed: %q", out.String())
	}
}

func TestMainRunsRestartCommand(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new\n")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)
	exe := installedBinary(t, "testtool")
	marker := filepath.Join(t.TempDir(), "recycled")

	var out bytes.Buffer
	cfg := testConfig(f, exe, &out)
	cfg.Args = []string{"-restart-cmd", "touch " + marker}
	if code := Main(cfg); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("restart command did not run: %v", err)
	}
}

func TestMainRestartFailureIsAnError(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new\n")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v1.1.0", assets)
	exe := installedBinary(t, "testtool")

	var out bytes.Buffer
	cfg := testConfig(f, exe, &out)
	cfg.Args = []string{"-restart-cmd", "exit 7"}
	if code := Main(cfg); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out.String())
	}
	// The binary was still swapped — a failed recycle does not undo it, and
	// saying otherwise would send the operator looking for the wrong problem.
	if got, _ := os.ReadFile(exe); string(got) != "new\n" {
		t.Fatalf("binary should still be updated: %q", got)
	}
}

func TestMainFlags(t *testing.T) {
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new\n")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v2.0.0", assets)
	exe := installedBinary(t, "testtool")

	t.Run("check installs nothing", func(t *testing.T) {
		var out bytes.Buffer
		cfg := testConfig(f, exe, &out)
		cfg.Args = []string{"-check"}
		if code := Main(cfg); code != ExitUpdateAvailable {
			t.Fatalf("exit %d, want %d: %s", code, ExitUpdateAvailable, out.String())
		}
		if !strings.Contains(out.String(), "update available") {
			t.Errorf("output %q", out.String())
		}
		if got, _ := os.ReadFile(exe); string(got) != "old binary\n" {
			t.Fatalf("binary touched: %q", got)
		}
	})

	t.Run("help exits zero", func(t *testing.T) {
		var out bytes.Buffer
		cfg := testConfig(f, exe, &out)
		cfg.Args = []string{"-h"}
		if code := Main(cfg); code != 0 {
			t.Fatalf("exit %d — -h is a request, not a usage error", code)
		}
	})

	t.Run("version pins the tag", func(t *testing.T) {
		var out bytes.Buffer
		cfg := testConfig(f, exe, &out)
		cfg.Args = []string{"-check", "-version", "v9.9.9"}
		if code := Main(cfg); code != 1 {
			t.Fatalf("exit %d, want 1 for a missing tag: %s", code, out.String())
		}
	})

	t.Run("repo overrides the source", func(t *testing.T) {
		var out bytes.Buffer
		cfg := testConfig(f, exe, &out)
		cfg.Args = []string{"-check", "-repo", "nonsense"}
		if code := Main(cfg); code != 1 {
			t.Fatalf("exit %d, want 1 for a malformed repo", code)
		}
		if !strings.Contains(out.String(), "want owner/name") {
			t.Errorf("output %q", out.String())
		}
	})

	t.Run("unknown flag exits 2", func(t *testing.T) {
		var out bytes.Buffer
		cfg := testConfig(f, exe, &out)
		cfg.Args = []string{"-nope"}
		if code := Main(cfg); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})
}

func TestMainRefusalIsExplained(t *testing.T) {
	var out bytes.Buffer
	cfg := Config{
		Repo:     "kfet/testtool",
		Binary:   "testtool",
		Version:  "v1.0.0",
		Token:    "seeded",
		Stdout:   &out,
		Stderr:   &out,
		Args:     []string{},
		ExecPath: func() (string, error) { return "/usr/bin/testtool", nil },
	}
	if code := Main(cfg); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	s := out.String()
	if !strings.Contains(s, "refusing to self-update") || !strings.Contains(s, "nothing was changed") {
		t.Errorf("a refusal must read as a decision, not a crash: %q", s)
	}
}

func TestMainDefaultsWriters(t *testing.T) {
	// A caller that supplies no writers must not panic on nil.
	cfg := Config{Repo: "kfet/testtool", Binary: "testtool", Version: "v1.0.0", Args: []string{"-nope"}}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = stderr }()
	if code := Main(cfg); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestMainDefaultsArgsFromOsArgs(t *testing.T) {
	// Production calls Main with no Args: everything after the subcommand
	// word must be picked up from os.Args.
	asset := assetFor("testtool")
	assets := map[string][]byte{asset: []byte("new\n")}
	assets["checksums.txt"] = checksums(assets)
	f := newFakeGitHub(t, "v2.0.0", assets)
	exe := installedBinary(t, "testtool")

	saved := os.Args
	os.Args = []string{"testtool", "update", "-check"}
	defer func() { os.Args = saved }()

	var out bytes.Buffer
	cfg := testConfig(f, exe, &out)
	if code := Main(cfg); code != ExitUpdateAvailable {
		t.Fatalf("exit %d, want %d: %s", code, ExitUpdateAvailable, out.String())
	}
	if !strings.Contains(out.String(), "update available") {
		t.Errorf("os.Args flags were ignored: %q", out.String())
	}

	// And a bare `testtool update` with nothing after it must not panic.
	os.Args = []string{"testtool"}
	out.Reset()
	cfg2 := testConfig(f, exe, &out)
	cfg2.CheckOnly = true
	if code := Main(cfg2); code != ExitUpdateAvailable {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{Repo: "kfet/harb", Binary: "harb", Version: "0.4.0", Token: "x"}
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	if cfg.AssetStem != "harb" {
		t.Errorf("AssetStem = %q, want the binary name", cfg.AssetStem)
	}
	if cfg.AssetTemplate != DefaultAssetTemplate || cfg.ChecksumsAsset != DefaultChecksums {
		t.Errorf("asset defaults wrong: %+v", cfg)
	}
	if cfg.ArmSuffix != "armv6" {
		t.Errorf("ArmSuffix = %q — the family builds GOARM=6 and publishes armv6", cfg.ArmSuffix)
	}
	if cfg.APIBase != DefaultAPIBase {
		t.Errorf("APIBase = %q", cfg.APIBase)
	}
	if cfg.HTTPClient == nil || cfg.Stdout == nil || cfg.Stderr == nil || cfg.ExecPath == nil {
		t.Errorf("nil defaults left in %+v", cfg)
	}
}

func TestDefaultHTTPClientHasNoTotalDeadline(t *testing.T) {
	// A 40 MB binary over a Pi's link legitimately takes minutes; only the
	// hang is bounded.
	c := defaultHTTPClient()
	if c.Timeout != 0 {
		t.Fatalf("client Timeout = %v, want 0 — a total deadline breaks slow links", c.Timeout)
	}
}

func TestAssetName(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		tag    string
		goos   string
		goarch string
		want   string
	}{
		{
			name: "default raw binary",
			cfg:  Config{Repo: "k/x", Binary: "zulip-acp", Version: "v1"},
			tag:  "v0.19.0", goos: "linux", goarch: "amd64",
			want: "zulip-acp-linux-amd64",
		},
		{
			name: "arm publishes as armv6",
			cfg:  Config{Repo: "k/x", Binary: "zulip-acp", Version: "v1"},
			tag:  "v0.19.0", goos: "linux", goarch: "arm",
			want: "zulip-acp-linux-armv6",
		},
		{
			name: "a project whose assets say plain arm",
			cfg:  Config{Repo: "k/x", Binary: "acp-tmux", Version: "v1", ArmSuffix: "arm"},
			tag:  "v0.1.2", goos: "linux", goarch: "arm",
			want: "acp-tmux-linux-arm",
		},
		{
			name: "harb tarball, version without the v",
			cfg: Config{Repo: "k/x", Binary: "harb", Version: "v1",
				AssetTemplate: "{stem}-{version_no_v}-{os}-{arch}.tar.gz"},
			tag: "v0.4.0", goos: "linux", goarch: "arm64",
			want: "harb-0.4.0-linux-arm64.tar.gz",
		},
		{
			name: "asset stem differing from the binary",
			cfg: Config{Repo: "k/x", Binary: "fir", AssetStem: "fir-dist", Version: "v1",
				AssetTemplate: "{stem}-{version}-{os}-{arch}"},
			tag: "1.3.2", goos: "darwin", goarch: "arm64",
			want: "fir-dist-v1.3.2-darwin-arm64",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if err := cfg.normalise(); err != nil {
				t.Fatal(err)
			}
			if got := cfg.assetNameFor(tc.tag, tc.goos, tc.goarch); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAssetNameUsesTheRunningPlatform(t *testing.T) {
	cfg := Config{Repo: "k/x", Binary: "x", Version: "v1"}
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.AssetName("v1.0.0"); !strings.Contains(got, runtime.GOOS) {
		t.Fatalf("AssetName = %q, want it to name %s", got, runtime.GOOS)
	}
}

func TestEnsureV(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3": "v1.2.3", "v1.2.3": "v1.2.3", "": "",
		"v": "v", "0.0.1-rc1": "v0.0.1-rc1",
	} {
		if got := EnsureV(in); got != want {
			t.Errorf("EnsureV(%q) = %q, want %q", in, got, want)
		}
	}
}
