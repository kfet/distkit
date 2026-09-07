package distkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitCellarPath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		wantOK bool
		prefix string
		keg    string
		kegDir string
	}{
		{
			name:   "standard macOS arm64 keg",
			path:   "/opt/homebrew/Cellar/fir/1.3.2/bin/fir",
			wantOK: true, prefix: "/opt/homebrew", keg: "fir",
			kegDir: "/opt/homebrew/Cellar/fir/1.3.2",
		},
		{
			name:   "linuxbrew",
			path:   "/home/linuxbrew/.linuxbrew/Cellar/harb/0.4.0/bin/harb",
			wantOK: true, prefix: "/home/linuxbrew/.linuxbrew", keg: "harb",
			kegDir: "/home/linuxbrew/.linuxbrew/Cellar/harb/0.4.0",
		},
		{
			name:   "no version component",
			path:   "/opt/homebrew/Cellar/fir",
			wantOK: true, prefix: "/opt/homebrew", keg: "fir", kegDir: "",
		},
		{
			name: "cellar at the filesystem root",
			// Degenerate but well-defined: the prefix collapses to "/",
			// which is not a standard prefix, so detection declines it
			// later unless `brew --prefix` says otherwise.
			path:   "/Cellar/fir/1.0/bin/fir",
			wantOK: true, prefix: "/", keg: "fir", kegDir: "/Cellar/fir/1.0",
		},
		{
			name:   "trailing Cellar with no keg",
			path:   "/opt/homebrew/Cellar",
			wantOK: false,
		},
		{name: "not brew at all", path: "/home/kfet/.local/bin/fir", wantOK: false},
		{name: "lowercase cellar is not Cellar", path: "/opt/homebrew/cellar/fir/1.0/bin/fir", wantOK: false},
		{name: "empty", path: "", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := splitCellarPath(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (%+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if got.prefix != tc.prefix || got.keg != tc.keg || got.kegDir != tc.kegDir {
				t.Errorf("got %+v, want prefix=%s keg=%s kegDir=%s", got, tc.prefix, tc.keg, tc.kegDir)
			}
		})
	}
}

func TestSplitCellarPathNestedCellarUsesFirstMatch(t *testing.T) {
	got, ok := splitCellarPath("/opt/homebrew/Cellar/fir/1.0/libexec/Cellar/other/2.0/bin/x")
	if !ok {
		t.Fatal("want ok")
	}
	if got.prefix != "/opt/homebrew" || got.keg != "fir" {
		t.Fatalf("got %+v — the outermost Cellar owns the keg", got)
	}
}

func TestIsStandardBrewPrefix(t *testing.T) {
	for _, p := range []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew", "/opt/homebrew/"} {
		if !isStandardBrewPrefix(p) {
			t.Errorf("%s should be standard", p)
		}
	}
	for _, p := range []string{"/opt/brew", "/usr", "/home/linuxbrew", "", "/"} {
		if isStandardBrewPrefix(p) {
			t.Errorf("%s should not be standard", p)
		}
	}
}

// brewEnvStub builds a detection environment with everything stubbed and a
// counter for each exec, so a test can assert that the cheap path really is
// cheap.
type brewEnvStub struct {
	env          brewEnv
	prefixCalls  int
	infoCalls    int
	lookPathCall int
}

func newBrewEnvStub(exe string) *brewEnvStub {
	s := &brewEnvStub{}
	s.env = brewEnv{
		executablePath: func() (string, error) { return exe, nil },
		evalSymlinks:   func(p string) (string, error) { return p, nil },
		lookPath: func(string) (string, error) {
			s.lookPathCall++
			return "/opt/homebrew/bin/brew", nil
		},
		brewPrefix: func(context.Context, string) (string, error) {
			s.prefixCalls++
			return "", fmt.Errorf("no brew")
		},
		fullFormulaName: func(context.Context, string, string) (string, error) {
			s.infoCalls++
			return "", fmt.Errorf("no info")
		},
		readFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
	}
	return s
}

func TestDetectBrewInstall(t *testing.T) {
	t.Run("standard prefix is trusted without exec", func(t *testing.T) {
		s := newBrewEnvStub("/opt/homebrew/Cellar/fir/1.3.2/bin/fir")
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil {
			t.Fatal(err)
		}
		if inst == nil {
			t.Fatal("want a detected install")
		}
		if inst.Prefix != "/opt/homebrew" || inst.Formula != "fir" {
			t.Errorf("got %+v", inst)
		}
		if s.prefixCalls != 0 {
			t.Errorf("`brew --prefix` was run %d times for a standard prefix", s.prefixCalls)
		}
	})

	t.Run("non-cellar path never execs brew", func(t *testing.T) {
		s := newBrewEnvStub("/home/kfet/.local/bin/fir")
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil || inst != nil {
			t.Fatalf("got %+v, %v — a non-brew host must fall through", inst, err)
		}
		if s.lookPathCall != 0 || s.prefixCalls != 0 || s.infoCalls != 0 {
			t.Errorf("execs on the common non-brew path: %+v", s)
		}
	})

	t.Run("unusual prefix confirmed by brew --prefix", func(t *testing.T) {
		s := newBrewEnvStub("/srv/brew/Cellar/fir/1.0/bin/fir")
		s.env.brewPrefix = func(context.Context, string) (string, error) {
			s.prefixCalls++
			return "/srv/brew\n", nil
		}
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil {
			t.Fatal(err)
		}
		if inst == nil || inst.Prefix != "/srv/brew" {
			t.Fatalf("got %+v", inst)
		}
	})

	t.Run("unusual prefix brew disagrees is not brew", func(t *testing.T) {
		s := newBrewEnvStub("/srv/notbrew/Cellar/fir/1.0/bin/fir")
		s.env.brewPrefix = func(context.Context, string) (string, error) { return "/opt/homebrew", nil }
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil || inst != nil {
			t.Fatalf("got %+v, %v — an unconfirmed Cellar must not be claimed", inst, err)
		}
	})

	t.Run("unresolvable symlink means not brew", func(t *testing.T) {
		s := newBrewEnvStub("/opt/homebrew/Cellar/fir/1.0/bin/fir")
		s.env.evalSymlinks = func(string) (string, error) { return "", os.ErrNotExist }
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil || inst != nil {
			t.Fatalf("got %+v, %v — unsure must mean not-brew, never an error", inst, err)
		}
	})

	t.Run("brew missing from PATH is still detected", func(t *testing.T) {
		s := newBrewEnvStub("/opt/homebrew/Cellar/fir/1.3.2/bin/fir")
		s.env.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil {
			t.Fatal(err)
		}
		if inst == nil || inst.BrewPath != "" {
			t.Fatalf("got %+v — a keg with no brew must be detected so it can be refused", inst)
		}
	})

	t.Run("receipt supplies the fully-qualified formula and skips brew info", func(t *testing.T) {
		s := newBrewEnvStub("/opt/homebrew/Cellar/fir/1.3.2/bin/fir")
		s.env.readFile = func(p string) ([]byte, error) {
			want := filepath.Join("/opt/homebrew/Cellar/fir/1.3.2", "INSTALL_RECEIPT.json")
			if p != want {
				t.Errorf("read %s, want %s", p, want)
			}
			return []byte(`{"source":{"tap":"kfet/ai"}}`), nil
		}
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil {
			t.Fatal(err)
		}
		if inst.Formula != "kfet/ai/fir" {
			t.Errorf("Formula = %s, want kfet/ai/fir", inst.Formula)
		}
		if s.infoCalls != 0 {
			t.Errorf("`brew info` was run despite a usable receipt")
		}
	})

	t.Run("brew info fills in when the receipt is unusable", func(t *testing.T) {
		s := newBrewEnvStub("/opt/homebrew/Cellar/fir/1.3.2/bin/fir")
		s.env.fullFormulaName = func(context.Context, string, string) (string, error) {
			s.infoCalls++
			return "kfet/ai/fir", nil
		}
		inst, err := detectBrewInstall(t.Context(), s.env)
		if err != nil {
			t.Fatal(err)
		}
		if inst.Formula != "kfet/ai/fir" || s.infoCalls != 1 {
			t.Errorf("got %+v after %d info calls", inst, s.infoCalls)
		}
	})

	t.Run("executablePath error surfaces", func(t *testing.T) {
		s := newBrewEnvStub("")
		s.env.executablePath = func() (string, error) { return "", fmt.Errorf("nope") }
		if _, err := detectBrewInstall(t.Context(), s.env); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestReceiptFormulaName(t *testing.T) {
	ok := func(data string) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(data), nil }
	}
	tests := []struct {
		name   string
		read   func(string) ([]byte, error)
		kegDir string
		keg    string
		want   string
		wantOK bool
	}{
		{"good", ok(`{"source":{"tap":"kfet/ai"}}`), "/k/1.0", "fir", "kfet/ai/fir", true},
		{"padded tap", ok(`{"source":{"tap":" kfet/ai "}}`), "/k/1.0", "fir", "kfet/ai/fir", true},
		{"no receipt", func(string) ([]byte, error) { return nil, os.ErrNotExist }, "/k/1.0", "fir", "", false},
		{"malformed json", ok(`{`), "/k/1.0", "fir", "", false},
		{"empty tap", ok(`{"source":{"tap":""}}`), "/k/1.0", "fir", "", false},
		{"tap with one component", ok(`{"source":{"tap":"kfet"}}`), "/k/1.0", "fir", "", false},
		{"tap that looks like a flag", ok(`{"source":{"tap":"-rf/ai"}}`), "/k/1.0", "fir", "", false},
		{"tap with a space", ok(`{"source":{"tap":"kfet/a i"}}`), "/k/1.0", "fir", "", false},
		{"dotdot tap", ok(`{"source":{"tap":"../ai"}}`), "/k/1.0", "fir", "", false},
		{"no kegDir", ok(`{"source":{"tap":"kfet/ai"}}`), "", "fir", "", false},
		{"no keg", ok(`{"source":{"tap":"kfet/ai"}}`), "/k/1.0", "", "", false},
		{"no reader", nil, "/k/1.0", "fir", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := receiptFormulaName(tc.read, tc.kegDir, tc.keg)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestUpgradeViaBrewRefusals(t *testing.T) {
	var out bytes.Buffer
	if err := UpgradeViaBrew(t.Context(), nil, "fir", &out, &out); err == nil {
		t.Fatal("want an error for a nil install")
	}
	inst := &BrewInstall{ExePath: "/opt/homebrew/Cellar/fir/1.0/bin/fir", Formula: "kfet/ai/fir"}
	err := UpgradeViaBrew(t.Context(), inst, "fir", &out, &out)
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "reverted by the next") {
		t.Errorf("the refusal must explain why self-updating a keg is wrong: %v", err)
	}
}

func TestUpgradeViaBrewRunsUpdateThenUpgrade(t *testing.T) {
	// A fake `brew` that records its arguments proves the order, which
	// matters: `brew upgrade` without a preceding `brew update` can resolve
	// to a stale formula.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	brew := filepath.Join(dir, "brew")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(brew, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := &BrewInstall{ExePath: "/opt/homebrew/Cellar/fir/1.0/bin/fir", Formula: "kfet/ai/fir", BrewPath: brew}
	var out bytes.Buffer
	if err := UpgradeViaBrew(t.Context(), inst, "fir", &out, &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "update\nupgrade kfet/ai/fir\n" {
		t.Fatalf("brew calls were %q", got)
	}
}

func TestUpgradeViaBrewReportsFailure(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	if err := os.WriteFile(brew, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := &BrewInstall{Formula: "kfet/ai/fir", BrewPath: brew}
	var out bytes.Buffer
	err := UpgradeViaBrew(t.Context(), inst, "fir", &out, &out)
	if err == nil || !strings.Contains(err.Error(), "brew update") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateTakesTheBrewPath(t *testing.T) {
	// A brew-managed install must be UPGRADED, not refused and not
	// self-updated: writing into the keg would be reverted by the next
	// `brew upgrade`, leaving a host reporting one version and running
	// another. This is the behaviour poe-acp and harb lacked.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	brew := filepath.Join(dir, "brew")
	if err := os.WriteFile(brew, []byte("#!/bin/sh\necho \"$@\" >> "+logPath+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Build a real Cellar layout under a temp root so EvalSymlinks resolves,
	// and inject that root as a confirmed prefix through brew --prefix.
	kegBin := filepath.Join(dir, "brewroot", "Cellar", "testtool", "1.0.0", "bin")
	if err := os.MkdirAll(kegBin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(kegBin, "testtool")
	if err := os.WriteFile(exe, []byte("keg binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := brewEnv{
		executablePath:  func() (string, error) { return exe, nil },
		evalSymlinks:    func(p string) (string, error) { return p, nil },
		lookPath:        func(string) (string, error) { return brew, nil },
		brewPrefix:      func(context.Context, string) (string, error) { return filepath.Join(dir, "brewroot"), nil },
		fullFormulaName: func(context.Context, string, string) (string, error) { return "kfet/tap/testtool", nil },
		readFile:        func(string) ([]byte, error) { return nil, os.ErrNotExist },
	}
	inst, err := detectBrewInstall(t.Context(), env)
	if err != nil || inst == nil {
		t.Fatalf("detect: %+v %v", inst, err)
	}
	var out bytes.Buffer
	if err := UpgradeViaBrew(t.Context(), inst, "testtool", &out, &out); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "upgrade kfet/tap/testtool") {
		t.Fatalf("brew calls: %q", calls)
	}
	if got, _ := os.ReadFile(exe); string(got) != "keg binary\n" {
		t.Fatalf("the keg binary was rewritten: %q", got)
	}
}

func TestUpdateBrewCheckOnlyReports(t *testing.T) {
	cfg := Config{
		Repo:     "kfet/testtool",
		Binary:   "testtool",
		Version:  "v1.0.0",
		Token:    "seeded",
		Stdout:   &bytes.Buffer{},
		ExecPath: func() (string, error) { return "/opt/homebrew/Cellar/testtool/1.0.0/bin/testtool", nil },
	}
	// The path does not exist, so EvalSymlinks fails and detection declines;
	// with DisableBrew the managed-prefix backstop must still refuse it.
	cfg.DisableBrew = true
	if _, err := Update(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "refusing to self-update") {
		t.Fatalf("got %v", err)
	}
}

func TestBrewInfoV2Unmarshal(t *testing.T) {
	var info brewInfoV2
	if err := json.Unmarshal([]byte(`{"formulae":[{"full_name":"kfet/ai/fir"}]}`), &info); err != nil {
		t.Fatal(err)
	}
	if len(info.Formulae) != 1 || info.Formulae[0].FullName != "kfet/ai/fir" {
		t.Fatalf("got %+v", info)
	}
}
