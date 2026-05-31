package sori_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sori "github.com/HeaInSeo/sori"
	"github.com/HeaInSeo/sori/chunked"
)

// newChunkedSrcDir creates a temp directory with a handful of files suitable
// for the chunked CAS path.  Does NOT create configblob.json so the source
// directory is clean (no legacy artefacts).
func newChunkedSrcDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"alpha.txt":     []byte("hello chunked dispatch"),
		"beta.bin":      bytes.Repeat([]byte{0xBE}, 512),
		"sub/gamma.txt": []byte("nested file"),
	}
	for rel, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// assertDirContentsEqual verifies that every file in srcDir appears in destDir
// with identical content.
func assertDirContentsEqual(t *testing.T, srcDir, destDir string) {
	t.Helper()
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(srcDir, path)
		srcData, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read src %s: %v", rel, err)
			return nil
		}
		dstData, err := os.ReadFile(filepath.Join(destDir, rel))
		if err != nil {
			t.Errorf("read dest %s: %v", rel, err)
			return nil
		}
		if !bytes.Equal(srcData, dstData) {
			t.Errorf("content mismatch for %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk srcDir: %v", err)
	}
}

// newChunkedClient returns a Client whose local store lives in a fresh temp dir.
func newChunkedClient(t *testing.T) (*sori.Client, string) {
	t.Helper()
	storePath := t.TempDir()
	return sori.NewClient(sori.WithLocalStorePath(storePath)), storePath
}

// TestDualPath_PackageVolumeWithOptions_ChunkedCAS verifies that
// Client.PackageVolumeWithOptions with ArtifactFormatChunkedCAS produces a
// chunked artifact (no partitions, VolumeRef == ManifestDigest).
func TestDualPath_PackageVolumeWithOptions_ChunkedCAS(t *testing.T) {
	ctx := context.Background()
	client, _ := newChunkedClient(t)
	srcDir := newChunkedSrcDir(t)

	pkg, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir:   srcDir,
		DisplayName: "Dual Path Test",
		Tag:         "dp:v1",
	}, sori.PackageOptions{
		Format: sori.ArtifactFormatChunkedCAS,
	})
	if err != nil {
		t.Fatalf("PackageVolumeWithOptions (chunked): %v", err)
	}
	if pkg.ManifestDigest == "" {
		t.Fatal("ManifestDigest must not be empty")
	}
	if pkg.VolumeIndex.VolumeRef != pkg.ManifestDigest {
		t.Errorf("VolumeRef=%q != ManifestDigest=%q", pkg.VolumeIndex.VolumeRef, pkg.ManifestDigest)
	}
	if len(pkg.Partitions) != 0 {
		t.Errorf("expected no partitions for chunked artifact, got %d", len(pkg.Partitions))
	}
	if pkg.TotalSize <= 0 {
		t.Errorf("expected positive TotalSize, got %d", pkg.TotalSize)
	}
}

// TestDualPath_FetchVolSeq_ChunkedCAS verifies that FetchVolSeq dispatches to
// the chunked fetcher and reconstructs files byte-identically.
func TestDualPath_FetchVolSeq_ChunkedCAS(t *testing.T) {
	ctx := context.Background()
	client, storePath := newChunkedClient(t)
	srcDir := newChunkedSrcDir(t)
	destDir := t.TempDir()

	pkg, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir:   srcDir,
		DisplayName: "SeqFetch Test",
		Tag:         "seq:v1",
	}, sori.PackageOptions{Format: sori.ArtifactFormatChunkedCAS})
	if err != nil {
		t.Fatalf("PackageVolumeWithOptions: %v", err)
	}

	vi, err := sori.FetchVolSeq(ctx, destDir, storePath, "seq:v1")
	if err != nil {
		t.Fatalf("FetchVolSeq: %v", err)
	}
	if vi.VolumeRef != pkg.ManifestDigest {
		t.Errorf("VolumeRef=%q want %q", vi.VolumeRef, pkg.ManifestDigest)
	}
	assertDirContentsEqual(t, srcDir, destDir)
}

// TestDualPath_FetchVolParallel_ChunkedCAS verifies that FetchVolParallel
// dispatches to the chunked fetcher and reconstructs files byte-identically.
func TestDualPath_FetchVolParallel_ChunkedCAS(t *testing.T) {
	ctx := context.Background()
	client, storePath := newChunkedClient(t)
	srcDir := newChunkedSrcDir(t)
	destDir := t.TempDir()

	pkg, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir:   srcDir,
		DisplayName: "ParallelFetch Test",
		Tag:         "par:v1",
	}, sori.PackageOptions{Format: sori.ArtifactFormatChunkedCAS})
	if err != nil {
		t.Fatalf("PackageVolumeWithOptions: %v", err)
	}

	vi, err := sori.FetchVolParallel(ctx, destDir, storePath, "par:v1", 4)
	if err != nil {
		t.Fatalf("FetchVolParallel: %v", err)
	}
	if vi.VolumeRef != pkg.ManifestDigest {
		t.Errorf("VolumeRef=%q want %q", vi.VolumeRef, pkg.ManifestDigest)
	}
	assertDirContentsEqual(t, srcDir, destDir)
}

// TestDualPath_LegacyUnchanged verifies that the default ArtifactFormatLegacy
// path is unchanged and still round-trips correctly.
func TestDualPath_LegacyUnchanged(t *testing.T) {
	ctx := context.Background()
	client, storePath := newChunkedClient(t)
	srcDir := newChunkedSrcDir(t)
	destDir := t.TempDir()

	pkg, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir:   srcDir,
		DisplayName: "Legacy Test",
		Tag:         "leg:v1",
	}, sori.PackageOptions{}) // zero value = ArtifactFormatLegacy
	if err != nil {
		t.Fatalf("PackageVolumeWithOptions (legacy): %v", err)
	}
	if pkg.ManifestDigest == "" {
		t.Fatal("legacy ManifestDigest must not be empty")
	}

	vi, err := sori.FetchVolSeq(ctx, destDir, storePath, "leg:v1")
	if err != nil {
		t.Fatalf("FetchVolSeq (legacy): %v", err)
	}
	if vi.VolumeRef != pkg.ManifestDigest {
		t.Errorf("VolumeRef=%q want %q", vi.VolumeRef, pkg.ManifestDigest)
	}
}

// TestDualPath_FetchVolSeq_EquivalentToDirectChunkedFetch verifies that
// FetchVolSeq and chunked.Fetch give identical results for a chunked artifact.
func TestDualPath_FetchVolSeq_EquivalentToDirectChunkedFetch(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()
	srcDir := newChunkedSrcDir(t)
	destSeq := t.TempDir()
	destDirect := t.TempDir()

	if _, err := chunked.Publish(ctx, storePath, srcDir, "cmp:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	}); err != nil {
		t.Fatalf("chunked.Publish: %v", err)
	}

	// FetchVolSeq dispatches to chunked.Fetch internally.
	if _, err := sori.FetchVolSeq(ctx, destSeq, storePath, "cmp:v1"); err != nil {
		t.Fatalf("FetchVolSeq (chunked dispatch): %v", err)
	}
	// Direct chunked.Fetch for comparison.
	if err := chunked.Fetch(ctx, storePath, destDirect, "cmp:v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("chunked.Fetch: %v", err)
	}

	assertDirContentsEqual(t, srcDir, destSeq)
	assertDirContentsEqual(t, srcDir, destDirect)
}

// TestDualPath_RequireEmpty_ChunkedCAS verifies that FetchVolume with
// RequireEmptyDestination dispatches to the chunked fetch path and reconstructs
// files byte-identically.
func TestDualPath_RequireEmpty_ChunkedCAS(t *testing.T) {
	ctx := context.Background()
	client, storePath := newChunkedClient(t)
	srcDir := newChunkedSrcDir(t)
	destDir := filepath.Join(t.TempDir(), "dest")

	if _, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir:   srcDir,
		DisplayName: "RequireEmpty Chunked Test",
		Tag:         "re:v1",
	}, sori.PackageOptions{Format: sori.ArtifactFormatChunkedCAS}); err != nil {
		t.Fatalf("PackageVolumeWithOptions: %v", err)
	}

	vi, err := client.FetchVolume(ctx, destDir, storePath, "re:v1", sori.FetchOptions{
		RequireEmptyDestination: true,
		Concurrency:             1,
	})
	if err != nil {
		t.Fatalf("FetchVolume (RequireEmptyDestination, chunked): %v", err)
	}
	if vi.VolumeRef == "" {
		t.Fatal("VolumeRef must not be empty")
	}
	assertDirContentsEqual(t, srcDir, destDir)
}

// TestDualPath_AtomicOverwrite_ChunkedCAS verifies that FetchVolume with
// AtomicOverwrite dispatches to the chunked fetch path, and that a second fetch
// with a different version replaces destRoot atomically.
func TestDualPath_AtomicOverwrite_ChunkedCAS(t *testing.T) {
	ctx := context.Background()
	client, storePath := newChunkedClient(t)
	srcV1 := newChunkedSrcDir(t)
	destDir := filepath.Join(t.TempDir(), "dest")

	// Build v2 source with a different file to detect overwrite.
	srcV2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcV2, "updated.txt"), []byte("v2 content"), 0o644); err != nil {
		t.Fatalf("write v2 file: %v", err)
	}

	if _, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir: srcV1, DisplayName: "AtomicOverwrite Test v1", Tag: "aow:v1",
	}, sori.PackageOptions{Format: sori.ArtifactFormatChunkedCAS}); err != nil {
		t.Fatalf("PackageVolumeWithOptions v1: %v", err)
	}
	if _, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir: srcV2, DisplayName: "AtomicOverwrite Test v2", Tag: "aow:v2",
	}, sori.PackageOptions{Format: sori.ArtifactFormatChunkedCAS}); err != nil {
		t.Fatalf("PackageVolumeWithOptions v2: %v", err)
	}

	// First fetch: destDir does not exist yet.
	if _, err := client.FetchVolume(ctx, destDir, storePath, "aow:v1", sori.FetchOptions{AtomicOverwrite: true}); err != nil {
		t.Fatalf("FetchVolume v1 (AtomicOverwrite, chunked): %v", err)
	}
	assertDirContentsEqual(t, srcV1, destDir)

	// Second fetch: overwrite destDir with v2.
	vi, err := client.FetchVolume(ctx, destDir, storePath, "aow:v2", sori.FetchOptions{AtomicOverwrite: true})
	if err != nil {
		t.Fatalf("FetchVolume v2 (AtomicOverwrite, chunked): %v", err)
	}
	if vi.VolumeRef == "" {
		t.Fatal("VolumeRef must not be empty after overwrite")
	}
	assertDirContentsEqual(t, srcV2, destDir)
}

// TestDualPath_RequireConfigBlob_Chunked verifies that RequireConfigBlob
// returns ErrValidation when no blob is provided on the chunked path.
func TestDualPath_RequireConfigBlob_Chunked(t *testing.T) {
	ctx := context.Background()
	client, _ := newChunkedClient(t)
	_, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
		SourceDir:   newChunkedSrcDir(t),
		DisplayName: "Test",
		Tag:         "req:v1",
	}, sori.PackageOptions{
		Format:            sori.ArtifactFormatChunkedCAS,
		RequireConfigBlob: true,
	})
	if !errors.Is(err, sori.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}
