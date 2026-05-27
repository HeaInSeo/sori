package catalogutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrInit_InvalidJSONTypedError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(path, []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadOrInit(root, "catalog.json", struct{}{})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestSave_CreateDirTypedError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file-root")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := Save(root, "catalog.json", map[string]string{"k": "v"})
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("expected ErrTransport, got %v", err)
	}
}

func TestLoadOrInit_FileNotExist_ReturnsZero(t *testing.T) {
	root := t.TempDir()
	type entry struct {
		Name string
	}
	zero := entry{}
	got, err := LoadOrInit(root, "missing.json", zero)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil zero value pointer")
	}
	if got.Name != "" {
		t.Fatalf("expected zero value, got %+v", got)
	}
}

func TestLoadOrInit_Success(t *testing.T) {
	root := t.TempDir()
	type entry struct {
		Name string `json:"name"`
	}
	if err := os.WriteFile(filepath.Join(root, "c.json"), []byte(`{"name":"hello"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := LoadOrInit(root, "c.json", entry{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "hello" {
		t.Fatalf("expected Name=hello, got %q", got.Name)
	}
}

func TestSave_RoundTrip(t *testing.T) {
	root := t.TempDir()
	type entry struct {
		Value int `json:"value"`
	}
	if err := Save(root, "c.json", entry{Value: 42}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadOrInit(root, "c.json", entry{})
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	if got.Value != 42 {
		t.Fatalf("expected Value=42, got %d", got.Value)
	}
}

func TestWriteFileAtomic_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := writeFileAtomic(path, []byte(`{"k":"v"}`), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != `{"k":"v"}` {
		t.Fatalf("content mismatch: %q", data)
	}
}

func TestWriteFileAtomic_NoTempSurvivor(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test read-only dir as root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_ = writeFileAtomic(filepath.Join(dir, "out.json"), []byte("{}"), 0o644)

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if len(e.Name()) > 5 && e.Name()[:5] == ".tmp-" {
			t.Errorf("stale temp file: %s", e.Name())
		}
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
