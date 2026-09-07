package distkit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxBinaryBytes caps what will be written out of an archive. A release
// binary is ~10-40 MB; 512 MB is far past any legitimate one and stops a
// malformed or hostile archive from filling the install disk. The sha256
// check has already passed by this point, so this is a belt-and-braces bound
// on a trusted-but-corrupt input, not a security boundary.
const maxBinaryBytes = 512 << 20

// isArchive reports whether an asset name denotes an archive that must be
// unpacked rather than a raw binary that can be installed directly.
func isArchive(name string) bool {
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// extractBinary pulls the file named `binary` out of the archive at src and
// writes it to dst, mode 0755.
//
// The entry is matched on base name at any depth: harb's tarball nests the
// binary under "harb-<version>-<os>-<arch>/", while a flat archive puts it at
// the root, and both are legitimate. Only regular files are considered, so a
// symlink or device entry claiming the name cannot be what gets installed.
func extractBinary(src, dst, binary string) error {
	switch {
	case strings.HasSuffix(src, ".zip"):
		return extractZip(src, dst, binary)
	default:
		return extractTarGz(src, dst, binary)
	}
}

func extractTarGz(src, dst, binary string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip %s: %w", filepath.Base(src), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(src), err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != binary {
			continue
		}
		return writeBinary(dst, tr)
	}
	return fmt.Errorf("no file named %q in %s", binary, filepath.Base(src))
}

func extractZip(src, dst, binary string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(src), err)
	}
	defer zr.Close()
	for _, e := range zr.File {
		if e.FileInfo().IsDir() || path.Base(e.Name) != binary {
			continue
		}
		rc, err := e.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeBinary(dst, rc)
	}
	return fmt.Errorf("no file named %q in %s", binary, filepath.Base(src))
}

// writeBinary streams r into dst, created executable so no separate chmod is
// needed — os.Rename preserves the mode, and a binary that lands
// non-executable is an install that fails at the next exec instead of here.
func writeBinary(dst string, r io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(r, maxBinaryBytes+1))
	if err != nil {
		return err
	}
	if n > maxBinaryBytes {
		return fmt.Errorf("archived binary exceeds %d bytes", int64(maxBinaryBytes))
	}
	if n == 0 {
		return fmt.Errorf("archived binary is empty")
	}
	return out.Sync()
}
