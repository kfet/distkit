//go:build unix

package distkit

import (
	"fmt"
	"os"
	"syscall"
)

// geteuid is a seam: the ownership refusal must be testable on a machine of
// any uid, including a root CI container.
var geteuid = os.Geteuid

// checkOwnership refuses an install directory that does not belong to the
// effective user, since the swap must create a temp file there and rename it
// over the binary.
func checkOwnership(path, binary string) error {
	fi, dir, err := statDir(path)
	if err != nil {
		return &ErrManaged{Path: path, Reason: fmt.Sprintf("cannot stat install dir %s: %v", dir, err)}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Refuse rather than silently skip the check: no ownership
		// guarantee means no safe swap.
		return &ErrManaged{Path: path, Reason: "cannot determine ownership of install dir " + dir}
	}
	uid := geteuid()
	if int(st.Uid) == uid {
		return nil
	}
	// A root-owned directory is the layout install.sh itself creates when
	// /usr/local/bin is writable by root only. "Use the package manager" is
	// the wrong advice there; sudo is the right one.
	hint := fmt.Sprintf("use the package manager that installed it, or install %s under your own home", binary)
	if st.Uid == 0 && uid != 0 {
		hint = fmt.Sprintf("re-run as root: `sudo %s update`", binary)
	}
	return &ErrManaged{
		Path:   path,
		Reason: fmt.Sprintf("install dir %s is owned by uid %d, not you (uid %d)", dir, st.Uid, uid),
		Hint:   hint,
	}
}
