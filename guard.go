package distkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// managedPrefixes are install locations owned by a package manager. A binary
// under one of them was not put there by us and must not be rewritten behind
// the package manager's back — the next `apt upgrade` or `brew upgrade` would
// silently revert it, leaving a host that reports one version and runs
// another.
//
// The Homebrew entries are a backstop only: a real keg install is caught
// earlier by DetectBrewInstall and UPGRADED rather than refused. These catch
// the shapes brew detection deliberately declines to claim (a bare
// /usr/local/Homebrew path with no Cellar component, say).
//
// This is the union of the four predecessor lists: zulip-acp guarded the brew
// prefixes and relied on the ownership check for system directories, while
// poe-acp and harb named /usr/bin and /usr/sbin explicitly. Naming them is
// strictly safer — a root-run update in /usr/bin passes the ownership check
// and would happily clobber a distro package.
//
// /usr/local/bin is deliberately NOT here: it is where this project's own
// install.sh puts things, and it is not owned by any package manager. It is
// covered by the ownership check instead, which knows to suggest sudo.
var managedPrefixes = []string{
	"/opt/homebrew/",
	"/usr/local/Cellar/",
	"/usr/local/Homebrew/",
	"/home/linuxbrew/",
	"/usr/bin/",
	"/usr/sbin/",
	"/bin/",
	"/sbin/",
}

// ErrManaged is returned when the install is owned by something other than
// this process. Callers can errors.As it to distinguish "you must not" from
// "it went wrong".
type ErrManaged struct {
	// Path is the resolved binary.
	Path string
	// Reason explains what makes it unsafe.
	Reason string
	// Hint is the command to use instead, if there is one.
	Hint string
}

func (e *ErrManaged) Error() string {
	msg := fmt.Sprintf("refusing to self-update: %s: %s", e.Path, e.Reason)
	if e.Hint != "" {
		msg += "; " + e.Hint
	}
	return msg
}

// CheckWritable reports whether the resolved binary at path may be replaced
// by this process: not under a package-manager prefix, and in a directory
// owned by the effective user.
//
// The directory ownership matters rather than the file's: the swap creates a
// temp file in the install directory and renames it over the binary, so a
// directory belonging to someone else fails no matter what the file's mode
// says. Checking it here turns a confusing "permission denied" after three
// network round-trips into an immediate, explainable refusal.
func CheckWritable(path, binary string) error {
	for _, p := range managedPrefixes {
		if !strings.HasPrefix(path, p) {
			continue
		}
		reason := "installed under the package-manager prefix " + p
		hint := "use the package manager that installed it"
		if strings.Contains(strings.ToLower(p), "brew") {
			// Under a brew prefix but not a keg — DetectBrewInstall would
			// have claimed a keg. Say so rather than suggesting an upgrade
			// that will report "no such formula".
			reason = "installed under the Homebrew prefix " + p + " but is not a keg"
			hint = fmt.Sprintf("reinstall it with `brew install %s`, or move it out of %s", binary, p)
		}
		return &ErrManaged{Path: path, Reason: reason, Hint: hint}
	}
	return checkOwnership(path, binary)
}

// resolveExe returns the running binary with symlinks followed, which is the
// file that must actually be replaced. A ~/.local/bin symlink into a versioned
// directory is common enough that swapping the symlink instead of its target
// would be a real bug.
//
// An unresolvable path is tolerated and returned as-is: it is what os.Rename
// will act on anyway, and the guards below still apply to it.
func resolveExe(cfg *Config) (string, error) {
	exe, err := cfg.ExecPath()
	if err != nil {
		return "", fmt.Errorf("locate self: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil
	}
	return resolved, nil
}

// statDir is a small helper shared by the platform-specific ownership checks.
func statDir(path string) (os.FileInfo, string, error) {
	dir := filepath.Dir(path)
	fi, err := os.Stat(dir)
	return fi, dir, err
}
