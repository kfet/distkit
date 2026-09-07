//go:build !unix

package distkit

import "fmt"

// geteuid exists on every platform so tests compile everywhere; on a non-unix
// target it is never consulted.
var geteuid = func() int { return -1 }

// checkOwnership refuses outright off unix. The whole swap design — directory
// ownership, rename over a running executable, ETXTBSY — is unix semantics,
// and the family cross-build matrix is linux and darwin only. A platform that
// reaches here gets an honest refusal rather than an unverified overwrite.
func checkOwnership(path, binary string) error {
	if _, dir, err := statDir(path); err != nil {
		return &ErrManaged{Path: path, Reason: fmt.Sprintf("cannot stat install dir %s: %v", dir, err)}
	}
	return &ErrManaged{
		Path:   path,
		Reason: "self-update is only supported on unix",
		Hint:   fmt.Sprintf("download the release asset and replace %s by hand", binary),
	}
}
