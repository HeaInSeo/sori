package chunked_test

// Failure-path and negative tests for the chunked package.
// These cover error branches in fetch.go and publish.go that require
// corruption, invalid inputs, or missing resources.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"

	"github.com/HeaInSeo/sori/chunked"
)

func computeDigest(data []byte) string {
	return digest.FromBytes(data).String()
}

// TestFetch_InvalidStorePath verifies that Fetch returns an error when the
// OCI store path does not exist.
func TestFetch_InvalidStorePath(t *testing.T) {
	ctx := context.Background()
	err := chunked.Fetch(ctx, "/nonexistent/store/path", t.TempDir(), "v1", chunked.FetchOptions{})
	if err == nil {
		t.Fatal("expected error for invalid store path, got nil")
	}
}

// TestFetch_TagNotFound verifies that Fetch returns an error when the tag does
// not exist in the store.
func TestFetch_TagNotFound(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newTestStore(t)
	defer cleanup()
	err := chunked.Fetch(ctx, store, t.TempDir(), "does-not-exist", chunked.FetchOptions{})
	if err == nil {
		t.Fatal("expected error for missing tag, got nil")
	}
}

// TestFetch_LegacyManifestRejected verifies that Fetch returns ErrValidation
// when pointed at a legacy OCI image (MediaTypeImageConfig) instead of a
// chunked CAS artifact.
func TestFetch_LegacyManifestRejected(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()
	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("oci.New: %v", err)
	}

	// Push a config blob with the legacy mediaType.
	cfgData := []byte("{}")
	cfgDesc := ocispec.Descriptor{
		MediaType: chunked.MediaTypeLegacyConfig,
		Digest:    digest.FromBytes(cfgData),
		Size:      int64(len(cfgData)),
	}
	if err := store.Push(ctx, cfgDesc, bytes.NewReader(cfgData)); err != nil {
		t.Fatalf("push legacy config: %v", err)
	}
	layerData := []byte("fake tar.gz layer")
	layerDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(layerData),
		Size:      int64(len(layerData)),
	}
	if err := store.Push(ctx, layerDesc, bytes.NewReader(layerData)); err != nil {
		t.Fatalf("push layer: %v", err)
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Config:    cfgDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestBytes),
		Size:      int64(len(manifestBytes)),
	}
	if err := store.Push(ctx, manifestDesc, bytes.NewReader(manifestBytes)); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	if err := store.Tag(ctx, manifestDesc, "legacy-tag"); err != nil {
		t.Fatalf("tag: %v", err)
	}

	fetchErr := chunked.Fetch(ctx, storePath, t.TempDir(), "legacy-tag", chunked.FetchOptions{})
	if fetchErr == nil {
		t.Fatal("expected error for legacy format, got nil")
	}
	if !errors.Is(fetchErr, chunked.ErrValidation) {
		t.Errorf("expected ErrValidation, got %T: %v", fetchErr, fetchErr)
	}
}

// TestFetch_ChunkDigestMismatch verifies that Fetch returns ErrIntegrity when
// a chunk blob in the store has been corrupted after publishing.
func TestFetch_ChunkDigestMismatch(t *testing.T) {
	ctx := context.Background()
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"data.bin": bytes.Repeat([]byte{0xCC}, 512),
	})

	_, err := chunked.Publish(ctx, storePath, srcDir, "corrupt:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Corrupt an actual chunk blob, identified via the manifest layer list
	// rather than a size heuristic. The original size-based heuristic
	// (skip blobs <=100 bytes) could - and did, when this test was
	// tightened - land on the chunk-index.json blob (MediaTypeChunkIndex)
	// or another non-chunk blob instead, which fails store
	// initialization/manifest parsing entirely before Fetch's per-chunk
	// digest loop (fetch.go step 3) ever runs. That produces a non-nil
	// error too, so the original bare "err != nil" assertion passed
	// without ever actually exercising the per-chunk check this test is
	// named for.
	manifest := fetchManifest(t, storePath, "corrupt:v1")
	var chunkDigest digest.Digest
	for _, layer := range manifest.Layers {
		if layer.MediaType == chunked.MediaTypeChunk {
			chunkDigest = layer.Digest
			break
		}
	}
	if chunkDigest == "" {
		t.Fatal("manifest has no MediaTypeChunk layer to corrupt")
	}
	blobPath := filepath.Join(storePath, "blobs", chunkDigest.Algorithm().String(), chunkDigest.Encoded())
	if err := os.Chmod(blobPath, 0o644); err != nil {
		t.Fatalf("chmod blob: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("corrupted content"), 0o644); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}

	// Record every progress event so the assertions below can prove the
	// *per-chunk* digest check (fetch.go's Step 3 loop) is what actually
	// caught the corruption, not the separate whole-file sha256 check
	// (Step 5) - a corrupted chunk blob would independently fail *both*
	// checks, since the final reconstructed file's bytes are wrong either
	// way, so a bare err != nil assertion can't distinguish which branch
	// fired. If the per-chunk check ran and failed, no ChunkFetched event
	// for the corrupted chunk (or FileDone) should ever be emitted, since
	// Fetch returns immediately on that error.
	var events []chunked.ChunkProgress
	fetchErr := chunked.Fetch(ctx, storePath, t.TempDir(), "corrupt:v1", chunked.FetchOptions{
		Progress: func(cp chunked.ChunkProgress) { events = append(events, cp) },
	})
	if fetchErr == nil {
		t.Fatal("expected error for corrupted chunk, got nil")
	}
	if !errors.Is(fetchErr, chunked.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %T: %v", fetchErr, fetchErr)
	}
	// The per-chunk message format is "...: chunk %d of %s: digest mismatch...";
	// the whole-file format is "...: file %s: digest mismatch..." (no "chunk "
	// substring). Only the per-chunk branch produces this exact shape.
	if !strings.Contains(fetchErr.Error(), "chunk ") {
		t.Fatalf("error does not look like the per-chunk digest-mismatch branch (fetch.go step 3): %v", fetchErr)
	}
	for _, ev := range events {
		if ev.Event == "FileDone" {
			t.Fatalf("FileDone was emitted despite a corrupted chunk - the per-chunk check did not stop Fetch before file completion: %+v", events)
		}
	}
}

// TestFetch_VerifyDestTree_MissingFile verifies that VerifyDestTree returns
// an error when a file listed in the index is absent from destRoot.
func TestFetch_VerifyDestTree_MissingFile(t *testing.T) {
	ctx := context.Background()
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"alpha.txt": []byte("hello"),
		"beta.txt":  []byte("world"),
	})

	_, err := chunked.Publish(ctx, storePath, srcDir, "tree:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	destDir := t.TempDir()
	if err := chunked.Fetch(ctx, storePath, destDir, "tree:v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Remove a file to simulate post-fetch corruption.
	if err := os.Remove(filepath.Join(destDir, "alpha.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// VerifyDestTree should detect the missing file.
	_, verifyErr := chunked.VerifyDestTree(destDir, []chunked.ChunkIndexFile{
		{Path: "alpha.txt", Digest: "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
		{Path: "beta.txt", Digest: computeDigest([]byte("world"))},
	})
	if verifyErr == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestFetch_VerifyDestTree_ContentMismatch verifies that VerifyDestTree returns
// ErrIntegrity when a file's content has been modified after fetch.
func TestFetch_VerifyDestTree_ContentMismatch(t *testing.T) {
	ctx := context.Background()
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	original := []byte("original content")
	srcDir := newTestSrcDir(t, map[string][]byte{"file.txt": original})

	_, err := chunked.Publish(ctx, storePath, srcDir, "verify:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	destDir := t.TempDir()
	if err := chunked.Fetch(ctx, storePath, destDir, "verify:v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Tamper with the file content.
	if err := os.WriteFile(filepath.Join(destDir, "file.txt"), []byte("tampered!"), 0o644); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	_, verifyErr := chunked.VerifyDestTree(destDir, []chunked.ChunkIndexFile{
		{Path: "file.txt", Digest: computeDigest(original)},
	})
	if !errors.Is(verifyErr, chunked.ErrIntegrity) {
		t.Errorf("expected ErrIntegrity for tampered file, got %v", verifyErr)
	}
}

// TestPublish_InvalidSourceDir verifies that Publish returns an error when
// the source directory does not exist.
func TestPublish_InvalidSourceDir(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newTestStore(t)
	defer cleanup()

	_, err := chunked.Publish(ctx, store, "/nonexistent/src/dir", "v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err == nil {
		t.Fatal("expected error for invalid source dir, got nil")
	}
}

// TestPublish_EmptySourceDir verifies that Publish returns ErrValidation when
// the source directory contains no regular files.
func TestPublish_EmptySourceDir(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newTestStore(t)
	defer cleanup()

	emptyDir := t.TempDir()
	_, err := chunked.Publish(ctx, store, emptyDir, "v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err == nil {
		t.Fatal("expected error for empty source dir, got nil")
	}
	if !errors.Is(err, chunked.ErrValidation) {
		t.Errorf("expected ErrValidation, got %T: %v", err, err)
	}
}

// TestPublish_InvalidStorePath verifies that Publish returns an error when the
// OCI store path cannot be initialised.
func TestPublish_InvalidStorePath(t *testing.T) {
	ctx := context.Background()
	srcDir := newTestSrcDir(t, map[string][]byte{"f.txt": []byte("data")})

	// Use a path that exists as a file, not a directory.
	tmpFile, err := os.CreateTemp(t.TempDir(), "notadir")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	_, err = chunked.Publish(ctx, tmpFile.Name(), srcDir, "v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err == nil {
		t.Fatal("expected error when store path is a file, got nil")
	}
}

// TestFetch_ContextCancelled verifies that Fetch respects context cancellation.
func TestFetch_ContextCancelled(t *testing.T) {
	ctx := context.Background()
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"a.bin": bytes.Repeat([]byte{0xAA}, 1024),
		"b.bin": bytes.Repeat([]byte{0xBB}, 1024),
	})
	if _, err := chunked.Publish(ctx, storePath, srcDir, "ctx:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel() // cancel immediately
	err := chunked.Fetch(cancelCtx, storePath, t.TempDir(), "ctx:v1", chunked.FetchOptions{})
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}
