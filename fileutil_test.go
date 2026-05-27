package sori

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomic_Success(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	data := []byte(`{"key":"value"}`)

	if err := writeFileAtomic(target, data, 0o644); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch: got %q, want %q", got, data)
	}
}

func TestWriteFileAtomic_NoTempSurvivorOnSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	if err := writeFileAtomic(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("stale temp file found after success: %s", e.Name())
		}
	}
}

func TestWriteFileAtomic_NoTempSurvivorOnFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test read-only directory as root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	target := filepath.Join(dir, "out.json")
	err := writeFileAtomic(target, []byte("{}"), 0o644)
	if err == nil {
		t.Fatal("expected error writing to read-only dir, got nil")
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("stale temp file found after failure: %s", e.Name())
		}
	}
}

func TestWriteFileAtomic_Overwrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	if err := writeFileAtomic(target, []byte(`{"v":1}`), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeFileAtomic(target, []byte(`{"v":2}`), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"v":2}` {
		t.Fatalf("expected second write to win, got %q", got)
	}
}
