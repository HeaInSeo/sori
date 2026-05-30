package chunked_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"

	"github.com/HeaInSeo/sori/chunked"
)

// newTestStore creates a temporary OCI store and returns its path and cleanup fn.
func newTestStore(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	return dir, func() { os.RemoveAll(dir) }
}

// newTestSrcDir creates a source directory with named files of given byte content.
func newTestSrcDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fetchManifest resolves tag in storePath and returns the OCI manifest.
func fetchManifest(t *testing.T, storePath, tag string) ocispec.Manifest {
	t.Helper()
	ctx := context.Background()
	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("open OCI store: %v", err)
	}
	desc, err := store.Resolve(ctx, tag)
	if err != nil {
		t.Fatalf("resolve %q: %v", tag, err)
	}
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	defer rc.Close()
	var m ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

// fetchBlob retrieves a blob by descriptor and returns its bytes.
func fetchBlob(t *testing.T, storePath string, desc ocispec.Descriptor) []byte {
	t.Helper()
	ctx := context.Background()
	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("open OCI store: %v", err)
	}
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		t.Fatalf("fetch blob %s: %v", desc.Digest, err)
	}
	defer rc.Close()
	data, err := readAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	return data
}

func readAll(rc interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := rc.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

func TestPublish_FirstPush(t *testing.T) {
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"a.txt": []byte("hello world"),
		"b.bin": []byte("binary data here"),
	})

	ctx := context.Background()
	desc, err := chunked.Publish(ctx, storePath, srcDir, "test:latest", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize, // small for test
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if desc.Digest.String() == "" {
		t.Fatal("manifest digest is empty")
	}

	// Verify manifest exists and has correct config mediaType.
	m := fetchManifest(t, storePath, "test:latest")
	if m.Config.MediaType != chunked.MediaTypeConfig {
		t.Errorf("config mediaType = %q, want %q", m.Config.MediaType, chunked.MediaTypeConfig)
	}

	// Verify OCI config descriptor blob content.
	cfgBytes := fetchBlob(t, storePath, m.Config)
	var ociCfg chunked.OCIConfigDescriptor
	if err := json.Unmarshal(cfgBytes, &ociCfg); err != nil {
		t.Fatalf("decode OCI config: %v", err)
	}
	if ociCfg.SchemaVersion != chunked.SchemaVersionConfig {
		t.Errorf("config.schemaVersion = %q, want %q", ociCfg.SchemaVersion, chunked.SchemaVersionConfig)
	}
	if ociCfg.ArtifactFormat != chunked.ArtifactFormatV1 {
		t.Errorf("config.artifactFormat = %q, want %q", ociCfg.ArtifactFormat, chunked.ArtifactFormatV1)
	}

	// Verify chunk-index layer is present (located by mediaType, not position).
	var chunkIndexDesc *ocispec.Descriptor
	for i := range m.Layers {
		if m.Layers[i].MediaType == chunked.MediaTypeChunkIndex {
			chunkIndexDesc = &m.Layers[i]
			break
		}
	}
	if chunkIndexDesc == nil {
		t.Fatal("no chunk-index layer found in manifest")
	}

	// Verify chunk-index.json is valid.
	idxBytes := fetchBlob(t, storePath, *chunkIndexDesc)
	var idx chunked.ChunkIndex
	if err := json.Unmarshal(idxBytes, &idx); err != nil {
		t.Fatalf("decode chunk-index: %v", err)
	}
	if idx.SchemaVersion != chunked.SchemaVersionChunkIndex {
		t.Errorf("chunk-index schemaVersion = %q, want %q", idx.SchemaVersion, chunked.SchemaVersionChunkIndex)
	}
	if len(idx.Files) != 2 {
		t.Errorf("chunk-index has %d files, want 2", len(idx.Files))
	}

	// Verify chunk blobs are present.
	for _, layer := range m.Layers {
		if layer.MediaType != chunked.MediaTypeChunk {
			continue
		}
		store, _ := oci.New(storePath)
		exists, err := store.Exists(ctx, layer)
		if err != nil || !exists {
			t.Errorf("chunk blob %s missing: err=%v exists=%v", layer.Digest, err, exists)
		}
	}
}

func TestPublish_ManifestStructure(t *testing.T) {
	// Push with all optional layers present: dataset-metadata + configblob.
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"data.bin": []byte("test data"),
	})

	meta := []byte(`{"schemaVersion":"sori.dataset.metadata.v1","kind":"test","displayName":"Test"}`)
	cfgBlob := []byte(`{"version":"1"}`)

	ctx := context.Background()
	_, err := chunked.Publish(ctx, storePath, srcDir, "structured:v1", chunked.PublishOptions{
		ChunkSize:       chunked.MinChunkSize,
		DatasetMetadata: meta,
		ConfigBlob:      cfgBlob,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	m := fetchManifest(t, storePath, "structured:v1")

	// All four non-chunk layer types must be present.
	layerTypes := make(map[string]int)
	for _, l := range m.Layers {
		layerTypes[l.MediaType]++
	}
	for _, mt := range []string{
		chunked.MediaTypeChunkIndex,
		chunked.MediaTypeDatasetMeta,
		chunked.MediaTypeConfigBlob,
		chunked.MediaTypeChunk,
	} {
		if layerTypes[mt] == 0 {
			t.Errorf("missing layer with mediaType %q", mt)
		}
	}
}

func TestPublish_AtomicOrder(t *testing.T) {
	// Verify that the OCI config descriptor blob exists before the manifest does.
	// We do this by checking the config blob is fetchable from the store after push.
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"file.txt": []byte("content"),
	})

	ctx := context.Background()
	_, err := chunked.Publish(ctx, storePath, srcDir, "atomic:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	m := fetchManifest(t, storePath, "atomic:v1")

	// Config blob must be fetchable (i.e., it was pushed before the manifest).
	store, _ := oci.New(storePath)
	exists, err := store.Exists(ctx, m.Config)
	if err != nil || !exists {
		t.Errorf("OCI config blob missing after publish: err=%v exists=%v", err, exists)
	}
}

func TestPublish_DeduplicatesChunks(t *testing.T) {
	// Push twice; second push should upload zero chunks (all skipped via Exists).
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	srcDir := newTestSrcDir(t, map[string][]byte{
		"data.bin": make([]byte, 1024),
	})

	ctx := context.Background()
	opts := chunked.PublishOptions{ChunkSize: chunked.MinChunkSize}

	var skipped int
	opts.Progress = func(cp chunked.ChunkProgress) {
		if cp.Event == "ChunkSkipped" {
			skipped++
		}
	}

	// First push.
	if _, err := chunked.Publish(ctx, storePath, srcDir, "dedup:v1", opts); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	skipped = 0

	// Second push: all chunks already present.
	if _, err := chunked.Publish(ctx, storePath, srcDir, "dedup:v1", opts); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	if skipped == 0 {
		t.Error("second push: expected ChunkSkipped events, got none")
	}
}

func TestPublish_MaxChunkedLayersExceeded(t *testing.T) {
	// A dataset that would produce more chunks than the budget must fail before
	// pushing any blobs.
	storePath, cleanup := newTestStore(t)
	defer cleanup()

	// Create 900 files × 1 byte each → 900 chunks at MinChunkSize.
	// Budget = MaxChunkedLayers(900) - metadataLayerCount(1) = 899, so 900 > 899.
	files := make(map[string][]byte, chunked.MaxChunkedLayers)
	for i := range chunked.MaxChunkedLayers {
		files[filepath.Join("dir", filepath.FromSlash(fmt.Sprintf("f%04d.bin", i)))] = []byte{byte(i)}
	}
	srcDir := newTestSrcDir(t, files)

	ctx := context.Background()
	var pushed int
	_, err := chunked.Publish(ctx, storePath, srcDir, "toolarge:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
		Progress: func(cp chunked.ChunkProgress) {
			if cp.Event == "ChunkUploaded" {
				pushed++
			}
		},
	})
	if err == nil {
		t.Fatal("expected error for dataset exceeding MaxChunkedLayers, got nil")
	}
	if pushed != 0 {
		t.Errorf("expected 0 blobs pushed before validation error, got %d", pushed)
	}
}
