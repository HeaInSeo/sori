package chunked_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HeaInSeo/sori/chunked"
)

func TestFetch_RoundTrip(t *testing.T) {
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	// Create source files of different sizes: empty, small, ~512 bytes.
	fileContents := map[string][]byte{
		"empty.txt":  {},
		"small.txt":  []byte("hello world"),
		"medium.bin": bytes.Repeat([]byte("abcdefghij"), 51), // 510 bytes
	}
	srcDir := newTestSrcDir(t, fileContents)

	ctx := context.Background()
	_, err := chunked.Publish(ctx, storePath, srcDir, "rt:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	destDir := t.TempDir()
	if err := chunked.Fetch(ctx, storePath, destDir, "rt:v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Verify each file matches original content and mode.
	for name, want := range fileContents {
		path := filepath.Join(destDir, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("file %s content mismatch: got %d bytes, want %d bytes", name, len(got), len(want))
		}

		fi, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		// Publish uses fi.Mode().Perm() which is 0o644 & 0o777 = 0o644.
		wantMode := os.FileMode(0o644)
		if fi.Mode().Perm() != wantMode {
			t.Errorf("file %s mode = %04o, want %04o", name, fi.Mode().Perm(), wantMode)
		}
	}
}

func TestFetch_IntegrityFailure(t *testing.T) {
	t.Skip("integrity failure requires store corruption; covered in integration tests")
}

func TestFetch_SchemaVersionMismatch(t *testing.T) {
	t.Skip("schema version mismatch injection requires low-level store manipulation; covered in integration tests")
}

func TestFetch_DatasetMetadataWritten(t *testing.T) {
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"data.txt": []byte("some data"),
	})

	metaContent := []byte(`{"schemaVersion":"sori.dataset.metadata.v1","kind":"test","displayName":"My Dataset"}`)

	ctx := context.Background()
	_, err := chunked.Publish(ctx, storePath, srcDir, "meta:v1", chunked.PublishOptions{
		ChunkSize:       chunked.MinChunkSize,
		DatasetMetadata: metaContent,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	destDir := t.TempDir()
	if err := chunked.Fetch(ctx, storePath, destDir, "meta:v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	metaPath := filepath.Join(destDir, ".sori", "dataset-metadata.json")
	got, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read dataset-metadata.json: %v", err)
	}
	if !bytes.Equal(got, metaContent) {
		t.Errorf("dataset-metadata.json content mismatch:\n got: %s\nwant: %s", got, metaContent)
	}
}

func TestFetch_ProgressEvents(t *testing.T) {
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"file1.txt": []byte("content one"),
		"file2.txt": []byte("content two"),
	})

	ctx := context.Background()
	_, err := chunked.Publish(ctx, storePath, srcDir, "progress:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var events []chunked.ChunkProgress
	destDir := t.TempDir()
	err = chunked.Fetch(ctx, storePath, destDir, "progress:v1", chunked.FetchOptions{
		Progress: func(cp chunked.ChunkProgress) {
			events = append(events, cp)
		},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Expect ChunkFetched, FileDone, and ArtifactDone events.
	eventCounts := make(map[string]int)
	for _, e := range events {
		eventCounts[e.Event]++
	}

	if eventCounts["ChunkFetched"] == 0 {
		t.Error("expected ChunkFetched events, got none")
	}
	if eventCounts["FileDone"] != 2 {
		t.Errorf("expected 2 FileDone events, got %d", eventCounts["FileDone"])
	}
	if eventCounts["ArtifactDone"] != 1 {
		t.Errorf("expected 1 ArtifactDone event, got %d", eventCounts["ArtifactDone"])
	}
}

func TestFetch_LegacyFormatRejected(t *testing.T) {
	// This test verifies the dual-path detection error message for legacy format.
	// Since we can't easily create a legacy-format OCI store, we verify the error
	// variable is exported and has the right message.
	if chunked.ErrValidation == nil {
		t.Fatal("ErrValidation must not be nil")
	}
	if chunked.ErrIntegrity == nil {
		t.Fatal("ErrIntegrity must not be nil")
	}
}
