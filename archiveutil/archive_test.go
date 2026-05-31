package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureJoinArchivePath_PathTraversalTypedError(t *testing.T) {
	_, err := SecureJoinArchivePath(t.TempDir(), "../evil")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestUntarGzDir_InvalidGzipTypedError(t *testing.T) {
	err := UntarGzDir(bytes.NewReader([]byte("not gzip")), t.TempDir())
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %v", err)
	}
}

func TestUntarGzDir_SymlinkEntryRejected(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     "link",
		Typeflag: tar.TypeSymlink,
		Linkname: "target",
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	// Symlinks are never allowed in sori artifacts (package and extract policy match).
	err := UntarGzDir(bytes.NewReader(buf.Bytes()), t.TempDir())
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for symlink entry, got %v", err)
	}
}

func TestTarGzDir_RoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	subdir := filepath.Join(src, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	data, err := TarGzDir(src, "mydir")
	if err != nil {
		t.Fatalf("TarGzDir: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty archive")
	}

	dest := t.TempDir()
	if err := UntarGzDir(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("UntarGzDir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "mydir", "a.txt"))
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("a.txt content mismatch: got %q", got)
	}

	got, err = os.ReadFile(filepath.Join(dest, "mydir", "sub", "b.txt"))
	if err != nil {
		t.Fatalf("read b.txt: %v", err)
	}
	if string(got) != "world" {
		t.Fatalf("b.txt content mismatch: got %q", got)
	}
}

// TestTarGzDirTo_EmptyPrefix verifies that TarGzDirTo with prefixPath="" produces
// a valid archive that UntarGzDir can extract without error. This was the root
// cause of the RunLegacy benchmark failure (root dir entry got name "" which
// SecureJoinArchivePath rejected).
func TestTarGzDirTo_EmptyPrefix(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "data.bin"), []byte("bench payload"), 0o644); err != nil {
		t.Fatalf("write data.bin: %v", err)
	}
	sub := filepath.Join(src, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatalf("write nested.txt: %v", err)
	}

	var buf bytes.Buffer
	if err := TarGzDirTo(&buf, src, ""); err != nil {
		t.Fatalf("TarGzDirTo: %v", err)
	}

	dest := t.TempDir()
	if err := UntarGzDir(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("UntarGzDir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "data.bin"))
	if err != nil {
		t.Fatalf("read data.bin: %v", err)
	}
	if string(got) != "bench payload" {
		t.Fatalf("data.bin content mismatch: got %q", got)
	}
	got, err = os.ReadFile(filepath.Join(dest, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("read nested.txt: %v", err)
	}
	if string(got) != "nested" {
		t.Fatalf("nested.txt content mismatch: got %q", got)
	}
}

func TestTarGzDir_Deterministic(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "z.txt"), []byte("z"), 0o644); err != nil {
		t.Fatalf("write z.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	data1, err := TarGzDir(src, "p")
	if err != nil {
		t.Fatalf("first TarGzDir: %v", err)
	}
	data2, err := TarGzDir(src, "p")
	if err != nil {
		t.Fatalf("second TarGzDir: %v", err)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatal("TarGzDir output is not deterministic")
	}
}

func TestTarGzDir_NonExistentDir(t *testing.T) {
	_, err := TarGzDir(filepath.Join(t.TempDir(), "nonexistent"), "p")
	if err == nil {
		t.Fatal("expected error for non-existent source dir")
	}
}

func TestTarGzDirFiles_BasicRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file1.txt"), []byte("content1"), 0o644); err != nil {
		t.Fatalf("write file1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "file2.txt"), []byte("content2"), 0o644); err != nil {
		t.Fatalf("write file2: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	data, err := TarGzDirFiles(src, "prefix", nil)
	if err != nil {
		t.Fatalf("TarGzDirFiles: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty archive")
	}

	dest := t.TempDir()
	if err := UntarGzDir(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("UntarGzDir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "prefix", "file1.txt"))
	if err != nil {
		t.Fatalf("read file1: %v", err)
	}
	if string(got) != "content1" {
		t.Fatalf("file1 content mismatch: got %q", got)
	}

	if _, err := os.Stat(filepath.Join(dest, "prefix", "subdir")); !os.IsNotExist(err) {
		t.Fatal("subdirectory should not be included in TarGzDirFiles output")
	}
}

func TestTarGzDirFiles_SkipNames(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "skip.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatalf("write skip.txt: %v", err)
	}

	data, err := TarGzDirFiles(src, "p", map[string]struct{}{"skip.txt": {}})
	if err != nil {
		t.Fatalf("TarGzDirFiles: %v", err)
	}

	dest := t.TempDir()
	if err := UntarGzDir(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("UntarGzDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "p", "skip.txt")); !os.IsNotExist(err) {
		t.Fatal("skip.txt should have been excluded")
	}
	if _, err := os.Stat(filepath.Join(dest, "p", "keep.txt")); err != nil {
		t.Fatalf("keep.txt should exist: %v", err)
	}
}

func TestTarGzDirFiles_AllSkipped_ReturnsNil(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "skip.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatalf("write skip.txt: %v", err)
	}

	data, err := TarGzDirFiles(src, "p", map[string]struct{}{"skip.txt": {}})
	if err != nil {
		t.Fatalf("TarGzDirFiles: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil for no-file archive, got %d bytes", len(data))
	}
}

func TestTarGzDirFiles_EmptyDir_ReturnsNil(t *testing.T) {
	data, err := TarGzDirFiles(t.TempDir(), "p", nil)
	if err != nil {
		t.Fatalf("TarGzDirFiles: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil for empty dir, got %d bytes", len(data))
	}
}

func TestTarGzDirFiles_NonExistentDir(t *testing.T) {
	_, err := TarGzDirFiles(filepath.Join(t.TempDir(), "nonexistent"), "p", nil)
	if err == nil {
		t.Fatal("expected error for non-existent directory")
	}
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("expected ErrTransport, got %v", err)
	}
}

func TestTarGzDirTo_RoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	subdir := filepath.Join(src, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "world.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write world.txt: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := TarGzDirTo(buf, src, "mydir"); err != nil {
		t.Fatalf("TarGzDirTo: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected non-empty archive")
	}

	dest := t.TempDir()
	if err := UntarGzDir(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("UntarGzDir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "mydir", "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("hello.txt content mismatch: got %q", got)
	}

	got, err = os.ReadFile(filepath.Join(dest, "mydir", "sub", "world.txt"))
	if err != nil {
		t.Fatalf("read world.txt: %v", err)
	}
	if string(got) != "world" {
		t.Fatalf("world.txt content mismatch: got %q", got)
	}
}

func TestTarGzDirFilesTo_HasFilesTrue(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "alpha.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write alpha.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "beta.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatalf("write beta.txt: %v", err)
	}
	// Subdir should not be included.
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	buf := &bytes.Buffer{}
	hasFiles, err := TarGzDirFilesTo(buf, src, "pfx", nil)
	if err != nil {
		t.Fatalf("TarGzDirFilesTo: %v", err)
	}
	if !hasFiles {
		t.Fatal("expected hasFiles=true for non-empty dir")
	}
	if buf.Len() == 0 {
		t.Fatal("expected non-empty archive bytes")
	}

	dest := t.TempDir()
	if err := UntarGzDir(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("UntarGzDir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "pfx", "alpha.txt"))
	if err != nil {
		t.Fatalf("read alpha.txt: %v", err)
	}
	if string(got) != "alpha" {
		t.Fatalf("alpha.txt content mismatch: got %q", got)
	}

	got, err = os.ReadFile(filepath.Join(dest, "pfx", "beta.txt"))
	if err != nil {
		t.Fatalf("read beta.txt: %v", err)
	}
	if string(got) != "beta" {
		t.Fatalf("beta.txt content mismatch: got %q", got)
	}

	if _, err := os.Stat(filepath.Join(dest, "pfx", "subdir")); !os.IsNotExist(err) {
		t.Fatal("subdir should not be present in TarGzDirFilesTo output")
	}
}

func TestTarGzDirFilesTo_HasFilesFalse(t *testing.T) {
	src := t.TempDir()
	// Only a subdir — no regular files.
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	buf := &bytes.Buffer{}
	hasFiles, err := TarGzDirFilesTo(buf, src, "pfx", nil)
	if err != nil {
		t.Fatalf("TarGzDirFilesTo: %v", err)
	}
	if hasFiles {
		t.Fatal("expected hasFiles=false for dir with no regular files")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected nothing written to w, got %d bytes", buf.Len())
	}
}

func TestError_ErrorString_AllBranches(t *testing.T) {
	wrapped := fmt.Errorf("cause")
	cases := []struct {
		e    *Error
		want string
	}{
		{nil, "<nil>"},
		{&Error{Op: "op", Message: "msg", Err: wrapped}, "op: msg: cause"},
		{&Error{Op: "op", Message: "msg"}, "op: msg"},
		{&Error{Op: "op", Err: wrapped}, "op: cause"},
		{&Error{Op: "op"}, "op"},
	}
	for _, c := range cases {
		got := c.e.Error()
		if got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

func TestError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("cause")
	e := &Error{Err: cause}
	if e.Unwrap() != cause {
		t.Fatalf("Unwrap: expected %v, got %v", cause, e.Unwrap())
	}

	var nilErr *Error
	if nilErr.Unwrap() != nil {
		t.Fatal("nil Error.Unwrap() must return nil")
	}
}

func TestError_Is_NonError(t *testing.T) {
	e := &Error{Kind: KindTransport, Op: "op"}
	if errors.Is(e, fmt.Errorf("other")) {
		t.Fatal("Is() must return false for non-*Error target")
	}
}

func TestTransportError_IsErrTransport(t *testing.T) {
	err := transportError("op", "msg", nil)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("expected ErrTransport, got %v", err)
	}
}

func TestSecureJoinArchivePath_AbsoluteEntry(t *testing.T) {
	_, err := SecureJoinArchivePath(t.TempDir(), "/abs/path")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for absolute path, got %v", err)
	}
}

func TestSecureJoinArchivePath_EmptyEntry(t *testing.T) {
	_, err := SecureJoinArchivePath(t.TempDir(), ".")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for '.' entry, got %v", err)
	}
}

func TestSecureJoinArchivePath_ValidEntry(t *testing.T) {
	dest := t.TempDir()
	got, err := SecureJoinArchivePath(dest, "sub/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := filepath.Join(dest, "sub", "file.txt")
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}

func TestUntarGzDir_UnsupportedEntryTypeRejected(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	// TypeFifo is an unsupported entry type.
	if err := tw.WriteHeader(&tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	err := UntarGzDir(bytes.NewReader(buf.Bytes()), t.TempDir())
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for unsupported entry type, got %v", err)
	}
}

func TestUntarGzDirRootFilesOnly_RejectsTopLevelDirectory(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	// vol/ is the prefix (allowed); vol/subdir/ is a directory under the prefix (not allowed).
	_ = tw.WriteHeader(&tar.Header{Name: "vol/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "vol/subdir/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.Close()
	_ = gw.Close()

	err := UntarGzDirRootFilesOnly(bytes.NewReader(buf.Bytes()), t.TempDir(), "vol")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for top-level directory in root-files layer, got %v", err)
	}
}

func TestUntarGzDirFiltered_PrefixEntryMustBeDirectory(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	// "vol/docs" appears as a regular file instead of a directory — malformed artifact.
	content := []byte("data")
	_ = tw.WriteHeader(&tar.Header{
		Name: "vol/docs", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gw.Close()

	err := UntarGzDirUnderPrefix(bytes.NewReader(buf.Bytes()), t.TempDir(), "vol/docs")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity when prefix entry is a regular file, got %v", err)
	}
}
