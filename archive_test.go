package distkit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarGz(t *testing.T, entries []tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, h := range entries {
		h.Size = int64(len(bodies[i]))
		if h.Mode == 0 {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestIsArchive(t *testing.T) {
	for _, yes := range []string{"x.tar.gz", "x.tgz", "x.zip", "harb-0.4.0-linux-arm64.tar.gz"} {
		if !isArchive(yes) {
			t.Errorf("%s should be an archive", yes)
		}
	}
	for _, no := range []string{"zulip-acp-linux-amd64", "checksums.txt", "x.gz", "x.tar"} {
		if isArchive(no) {
			t.Errorf("%s should not be an archive", no)
		}
	}
}

func TestExtractBinaryFromTarGz(t *testing.T) {
	want := []byte("the real binary\n")
	data := tarGz(t,
		[]tar.Header{
			{Name: "harb-0.4.0/", Typeflag: tar.TypeDir},
			{Name: "harb-0.4.0/README.md", Typeflag: tar.TypeReg},
			{Name: "harb-0.4.0/harb", Typeflag: tar.TypeReg},
		},
		[][]byte{nil, []byte("docs"), want},
	)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "harb")
	if err := extractBinary(src, dst, "harb"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 — the extracted file is what gets renamed into place", fi.Mode())
	}
}

func TestExtractBinaryIgnoresNonRegularEntries(t *testing.T) {
	// A symlink named like the binary must not be what gets installed.
	data := tarGz(t,
		[]tar.Header{
			{Name: "pkg/harb", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
			{Name: "pkg/nested/harb", Typeflag: tar.TypeReg},
		},
		[][]byte{nil, []byte("real\n")},
	)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "harb")
	if err := extractBinary(src, dst, "harb"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "real\n" {
		t.Fatalf("got %q — a symlink entry must be skipped", got)
	}
}

func TestExtractBinaryMissing(t *testing.T) {
	data := tarGz(t, []tar.Header{{Name: "pkg/other", Typeflag: tar.TypeReg}}, [][]byte{[]byte("x")})
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err := extractBinary(src, filepath.Join(dir, "harb"), "harb")
	if err == nil || !strings.Contains(err.Error(), `no file named "harb"`) {
		t.Fatalf("got %v", err)
	}
}

func TestExtractBinaryRejectsEmpty(t *testing.T) {
	data := tarGz(t, []tar.Header{{Name: "harb", Typeflag: tar.TypeReg}}, [][]byte{nil})
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err := extractBinary(src, filepath.Join(dir, "harb"), "harb")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("got %v — installing a zero-byte binary would break the host", err)
	}
}

func TestExtractBinaryNotAnArchive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(src, []byte("this is not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractBinary(src, filepath.Join(dir, "harb"), "harb"); err == nil {
		t.Fatal("want a gunzip error")
	}
}

func TestExtractBinaryFromZip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.zip")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("tool-1.0/tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("zipped\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "tool")
	if err := extractBinary(src, dst, "tool"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "zipped\n" {
		t.Fatalf("got %q", got)
	}
	if err := extractBinary(src, dst, "absent"); err == nil {
		t.Fatal("want an error for a missing member")
	}
}
