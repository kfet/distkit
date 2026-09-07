// Package installsh generates a consumer repo's root install.sh from one
// canonical template.
//
// Shell cannot be imported, so the honest form of "shared" is
// generated-and-verified. The template lives here; each consumer repo holds a
// small spec file and a generated install.sh at its root (it must be at the
// root for `curl …/main/install.sh | sh` to resolve), plus a drift check that
// fails when the checked-in copy no longer matches what the template would
// produce.
//
// The template is the union of the six hand-written scripts it replaces —
// curl-or-wget, uname mapping including the three 32-bit ARM spellings,
// GitHub API version resolution with optional token, sha256 verification
// against checksums.txt, optional tarball unpacking, BIN_DIR with the legacy
// PREFIX alias, sudo escalation, PATH warning, runtime-dependency warnings
// and post-install hints — with every point of divergence turned into a Spec
// field.
package installsh

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/template"

	"github.com/kfet/distkit"
)

//go:embed template.sh
var templateSource string

// Dep is a runtime dependency the installed binary needs but this script will
// not install: the package name differs per distro, and silently pulling in a
// terminal multiplexer (acp-tmux's case) is not an installer's business. The
// script warns instead.
type Dep struct {
	// Name is the command looked for on PATH, e.g. "tmux".
	Name string `json:"name"`
	// Message is the warning shown when it is absent.
	Message string `json:"message"`
}

// Spec is everything a consumer repo's install.sh varies by. The zero value
// is not usable: Repo and Binary are required.
type Spec struct {
	// Repo is the GitHub "owner/name", e.g. "kfet/harb".
	Repo string `json:"repo"`
	// Binary is the installed file name.
	Binary string `json:"binary"`
	// AssetStem defaults to Binary. See distkit.Config.AssetStem.
	AssetStem string `json:"asset_stem,omitempty"`
	// AssetTemplate defaults to distkit.DefaultAssetTemplate and uses the
	// same placeholders, so a consumer's Go config and its install.sh
	// cannot drift apart on asset naming.
	AssetTemplate string `json:"asset_template,omitempty"`
	// ArmSuffix is the asset-name token for 32-bit ARM; defaults to
	// "armv6".
	ArmSuffix string `json:"arm_suffix,omitempty"`
	// NoChecksums disables sha256 verification. Two of the six originals
	// shipped without it; that was a bug, not a variation, so it must be
	// asked for explicitly.
	NoChecksums bool `json:"no_checksums,omitempty"`
	// ChecksumsAsset defaults to distkit.DefaultChecksums.
	ChecksumsAsset string `json:"checksums_asset,omitempty"`
	// Darwin includes macOS in the supported-OS list. Default true.
	// Pointer so the JSON zero value can still mean "yes".
	Darwin *bool `json:"darwin,omitempty"`
	// FreeBSD includes FreeBSD (airan and mintick published for it).
	FreeBSD bool `json:"freebsd,omitempty"`
	// Arch386 includes 32-bit x86.
	Arch386 bool `json:"arch_386,omitempty"`
	// BrewFormula, when set (e.g. "kfet/tap/harb"), adds the macOS
	// "prefer brew" note to the header.
	BrewFormula string `json:"brew_formula,omitempty"`
	// RuntimeDeps are warned about when missing from PATH.
	RuntimeDeps []Dep `json:"runtime_deps,omitempty"`
	// VersionFlag is run against the freshly installed binary as a smoke
	// test, e.g. "-version". Empty skips it.
	VersionFlag string `json:"version_flag,omitempty"`
	// NextSteps are printed at the end, one per line, e.g.
	// "harb init    # bootstrap config".
	NextSteps []string `json:"next_steps,omitempty"`
	// ExampleTag appears in the usage header, e.g. "v0.1.0".
	ExampleTag string `json:"example_tag,omitempty"`
}

// FromConfig derives the install.sh spec from the same Config the binary
// self-updates with, so asset naming has exactly one definition per project.
func FromConfig(cfg distkit.Config) Spec {
	s := Spec{
		Repo:          cfg.Repo,
		Binary:        cfg.Binary,
		AssetStem:     cfg.AssetStem,
		AssetTemplate: cfg.AssetTemplate,
		ArmSuffix:     cfg.ArmSuffix,
	}
	s.NoChecksums = cfg.SkipChecksums
	s.ChecksumsAsset = cfg.ChecksumsAsset
	return s
}

// LoadSpec reads a spec from a JSON file, the form a consumer repo checks in
// next to its install.sh.
func LoadSpec(path string) (Spec, error) {
	var s Spec
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// shellUnsafe matches anything that must not reach the generated script
// unquoted. Spec values are interpolated into shell source, so a stray quote
// or a `$(` would be executed on every host that runs the installer.
var shellUnsafe = regexp.MustCompile(`["'` + "`" + `$\\;&|<>\n\r]`)

// view is the template's data: Spec, resolved and pre-rendered into the exact
// strings the shell needs.
type view struct {
	Spec
	OSCases       []osCase
	OSList        string
	ArchList      string
	AssetExpr     string
	Checksums     bool
	Archive       bool
	DefaultBinDir string
}

type osCase struct {
	Match string
	Value string
}

// Render produces the install.sh contents for spec.
func Render(spec Spec) ([]byte, error) {
	v, err := newView(spec)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("install.sh").Parse(templateSource)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newView(spec Spec) (*view, error) {
	if spec.Repo == "" {
		return nil, fmt.Errorf("installsh: spec.Repo is required")
	}
	if spec.Binary == "" {
		return nil, fmt.Errorf("installsh: spec.Binary is required")
	}
	if spec.AssetStem == "" {
		spec.AssetStem = spec.Binary
	}
	if spec.AssetTemplate == "" {
		spec.AssetTemplate = distkit.DefaultAssetTemplate
	}
	if spec.ArmSuffix == "" {
		spec.ArmSuffix = "armv6"
	}
	if spec.ChecksumsAsset == "" {
		spec.ChecksumsAsset = distkit.DefaultChecksums
	}
	if spec.ExampleTag == "" {
		spec.ExampleTag = "v0.1.0"
	}
	for _, f := range []struct{ name, val string }{
		{"repo", spec.Repo},
		{"binary", spec.Binary},
		{"asset_stem", spec.AssetStem},
		{"asset_template", spec.AssetTemplate},
		{"arm_suffix", spec.ArmSuffix},
		{"checksums_asset", spec.ChecksumsAsset},
		{"brew_formula", spec.BrewFormula},
		{"version_flag", spec.VersionFlag},
		{"example_tag", spec.ExampleTag},
	} {
		if shellUnsafe.MatchString(f.val) {
			return nil, fmt.Errorf("installsh: %s %q contains characters unsafe to interpolate into shell", f.name, f.val)
		}
	}
	for _, d := range spec.RuntimeDeps {
		if d.Name == "" {
			return nil, fmt.Errorf("installsh: runtime dep needs a name")
		}
		if shellUnsafe.MatchString(d.Name) {
			return nil, fmt.Errorf("installsh: runtime dep name %q is unsafe", d.Name)
		}
	}

	v := &view{
		Spec:          spec,
		Checksums:     !spec.NoChecksums,
		Archive:       isArchiveName(spec.AssetTemplate),
		DefaultBinDir: "/usr/local/bin, or $HOME/.local/bin when that is not writable",
	}

	v.OSCases = append(v.OSCases, osCase{"Linux", "linux"})
	oses := []string{"linux"}
	if spec.Darwin == nil || *spec.Darwin {
		v.OSCases = append(v.OSCases, osCase{"Darwin", "darwin"})
		oses = append(oses, "darwin")
	}
	if spec.FreeBSD {
		v.OSCases = append(v.OSCases, osCase{"FreeBSD", "freebsd"})
		oses = append(oses, "freebsd")
	}
	v.OSList = strings.Join(oses, " | ")

	arches := []string{"amd64", "arm64"}
	if spec.Arch386 {
		arches = append(arches, "386")
	}
	arches = append(arches, spec.ArmSuffix)
	v.ArchList = strings.Join(arches, " | ")

	v.AssetExpr = assetExpr(spec)

	// NextSteps are printed through printf with each line as a separate
	// argument, so single-quote them for the shell rather than trusting the
	// unsafe-character screen above.
	quoted := make([]string, 0, len(spec.NextSteps))
	for _, s := range spec.NextSteps {
		if strings.ContainsAny(s, "\n\r") {
			return nil, fmt.Errorf("installsh: next step %q must be a single line", s)
		}
		quoted = append(quoted, shellQuote(s))
	}
	v.Spec.NextSteps = quoted

	// A dep message is prose — "tmux is not on $PATH" is a perfectly
	// reasonable thing to say — so it is single-quoted for the shell rather
	// than screened for metacharacters.
	deps := make([]Dep, 0, len(spec.RuntimeDeps))
	for _, d := range spec.RuntimeDeps {
		if strings.ContainsAny(d.Message, "\n\r") {
			return nil, fmt.Errorf("installsh: runtime dep message for %s must be a single line", d.Name)
		}
		deps = append(deps, Dep{Name: d.Name, Message: shellQuote(d.Message)})
	}
	v.Spec.RuntimeDeps = deps
	return v, nil
}

// shellQuote wraps s in single quotes, which suppress every form of shell
// expansion; an embedded single quote is closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// assetExpr turns the AssetTemplate placeholders into shell expansions, so
// the generated script computes the asset name the same way the Go code does.
func assetExpr(spec Spec) string {
	return strings.NewReplacer(
		"{stem}", spec.AssetStem,
		"{binary}", "${BIN_NAME}",
		"{os}", "${OS}",
		"{arch}", "${ARCH}",
		"{version}", "${VERSION}",
		"{version_no_v}", "${VERSION_NO_V}",
	).Replace(spec.AssetTemplate)
}

func isArchiveName(name string) bool {
	for _, ext := range []string{".tar.gz", ".tgz"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// Write renders spec and writes it to path with mode 0755 — install.sh is
// executable in every repo that has one.
//
// The chmod is not redundant: os.WriteFile applies its mode only when it
// CREATES the file, so regenerating over an existing non-executable copy
// would otherwise leave a `./install.sh` that cannot be run.
func Write(path string, spec Spec) error {
	out, err := Render(spec)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// CheckDrift reports whether the file at path is byte-identical to what
// Render(spec) produces. This is the dev-only guard that keeps six copies of
// a script from diverging again: run it from a Make target or a test.
func CheckDrift(path string, spec Spec) error {
	want, err := Render(spec)
	if err != nil {
		return err
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w (run `make install.sh` to generate it)", path, err)
	}
	if bytes.Equal(got, want) {
		return nil
	}
	return fmt.Errorf("%s has drifted from the distkit template%s\nrun `make install.sh` to regenerate it",
		path, firstDiff(got, want))
}

// firstDiff locates the first differing line, so the failure names a line
// instead of dumping two whole scripts into a CI log.
func firstDiff(got, want []byte) string {
	g := strings.Split(string(got), "\n")
	w := strings.Split(string(want), "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return fmt.Sprintf("\nfirst difference at line %d:\n  have: %s\n  want: %s", i+1, g[i], w[i])
		}
	}
	return fmt.Sprintf("\nfile has %d lines, template produces %d", len(g), len(w))
}
