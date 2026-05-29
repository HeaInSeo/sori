package sori

// volume_integrity_test.go verifies the pre-P3 integrity improvements:
//   - unknown layerKind is rejected with ErrIntegrity
//   - tar entries outside the annotated partitionPath are rejected
//   - symlinks inside a source volume are rejected at package time
//   - no sensitive literals remain in source files

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/HeaInSeo/sori/archiveutil"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// buildLayerWithEntries creates a gzip-compressed tar whose entries are
// determined by the caller via the populate callback.
func buildLayerWithEntries(t *testing.T, partPath, layerKind string, populate func(*tar.Writer)) (ocispec.Descriptor, []byte) {
	t.Helper()
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	populate(tw)
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	data := buf.Bytes()
	annotations := map[string]string{annotationPartitionPath: partPath}
	if layerKind != "" {
		annotations[annotationLayerKind] = layerKind
	}
	desc := ocispec.Descriptor{
		MediaType:   ocispec.MediaTypeImageLayerGzip,
		Digest:      digest.FromBytes(data),
		Size:        int64(len(data)),
		Annotations: annotations,
	}
	return desc, data
}

// buildSingleLayerStore pushes one layer + config to a temp OCI store and tags it.
func buildSingleLayerStore(t *testing.T, layerDesc ocispec.Descriptor, layerData []byte, tag string) string {
	t.Helper()
	ctx := context.Background()
	storePath := t.TempDir()
	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("oci.New: %v", err)
	}
	configBlob := []byte("{}")
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configBlob),
		Size:      int64(len(configBlob)),
	}
	if err := store.Push(ctx, configDesc, bytes.NewReader(configBlob)); err != nil {
		t.Fatalf("push config: %v", err)
	}
	if err := store.Push(ctx, layerDesc, bytes.NewReader(layerData)); err != nil {
		t.Fatalf("push layer: %v", err)
	}
	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1,
		ocispec.MediaTypeImageManifest,
		oras.PackManifestOptions{
			ConfigDescriptor: &configDesc,
			Layers:           []ocispec.Descriptor{layerDesc},
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationCreated:   time.Now().UTC().Format(time.RFC3339),
				annotationVolumeDisplayName: "integrity-test",
			},
		},
	)
	if err != nil {
		t.Fatalf("PackManifest: %v", err)
	}
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		t.Fatalf("Tag: %v", err)
	}
	return storePath
}

// ── unknown layerKind ─────────────────────────────────────────────────────────

func TestFetchVolSeq_UnknownLayerKind_ErrIntegrity(t *testing.T) {
	ctx := context.Background()
	desc, data := buildLayerWithEntries(t, "vol/part", "custom-unknown", func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "vol/part/", Typeflag: tar.TypeDir, Mode: 0o755})
	})
	storePath := buildSingleLayerStore(t, desc, data, "unknown-kind.v1")

	_, err := FetchVolSeq(ctx, filepath.Join(t.TempDir(), "dest"), storePath, "unknown-kind.v1")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for unknown layerKind, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "unknown layerKind") {
		t.Fatalf("error message should mention 'unknown layerKind', got: %v", err)
	}
}

func TestFetchVolParallel_UnknownLayerKind_ErrIntegrity(t *testing.T) {
	ctx := context.Background()
	desc, data := buildLayerWithEntries(t, "vol/part", "exotic-type", func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "vol/part/", Typeflag: tar.TypeDir, Mode: 0o755})
	})
	storePath := buildSingleLayerStore(t, desc, data, "unknown-par.v1")

	_, err := FetchVolParallel(ctx, filepath.Join(t.TempDir(), "dest"), storePath, "unknown-par.v1", 2)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for unknown layerKind (parallel), got %T: %v", err, err)
	}
}

// ── tar entry outside partitionPath ──────────────────────────────────────────

func TestFetchVolSeq_TarEntryOutsidePartitionPath_ErrIntegrity(t *testing.T) {
	ctx := context.Background()
	// partitionPath is "vol/docs" but the tar contains an entry under "vol/other"
	desc, data := buildLayerWithEntries(t, "vol/docs", layerKindPartition, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "vol/docs/", Typeflag: tar.TypeDir, Mode: 0o755})
		content := []byte("content")
		_ = tw.WriteHeader(&tar.Header{
			Name: "vol/other/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
		})
		_, _ = tw.Write(content)
	})
	storePath := buildSingleLayerStore(t, desc, data, "outside-prefix.v1")

	dest := filepath.Join(t.TempDir(), "dest")
	_, err := FetchVolSeq(ctx, dest, storePath, "outside-prefix.v1")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for entry outside prefix, got %T: %v", err, err)
	}
}

func TestFetchVolSeq_RootFilesLayerWithSubdirEntry_ErrIntegrity(t *testing.T) {
	ctx := context.Background()
	// root-files layer: partitionPath is "vol" but tar contains "vol/subdir/file.txt" (too deep)
	desc, data := buildLayerWithEntries(t, "vol", layerKindRootFiles, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "vol/", Typeflag: tar.TypeDir, Mode: 0o755})
		content := []byte("content")
		_ = tw.WriteHeader(&tar.Header{
			Name: "vol/subdir/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
		})
		_, _ = tw.Write(content)
	})
	storePath := buildSingleLayerStore(t, desc, data, "rootfiles-deep.v1")

	dest := filepath.Join(t.TempDir(), "dest")
	_, err := FetchVolSeq(ctx, dest, storePath, "rootfiles-deep.v1")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for root-files entry going deeper than one level, got %T: %v", err, err)
	}
}

func TestFetchVolSeq_RootFilesLayerWithWrongPartitionPath_ErrIntegrity(t *testing.T) {
	ctx := context.Background()
	// root-files layer must have partitionPath == rootBase; "vol/docs" is invalid
	desc, data := buildLayerWithEntries(t, "vol/docs", layerKindRootFiles, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "vol/docs/", Typeflag: tar.TypeDir, Mode: 0o755})
	})
	storePath := buildSingleLayerStore(t, desc, data, "rootfiles-wrong-path.v1")

	_, err := FetchVolSeq(ctx, filepath.Join(t.TempDir(), "dest"), storePath, "rootfiles-wrong-path.v1")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for root-files with subpath, got %T: %v", err, err)
	}
}

// ── symlink rejection at package time ─────────────────────────────────────────

func TestTarGzDir_SymlinkRejected(t *testing.T) {
	src := t.TempDir()
	// Create a real file so the walk has content, then add a symlink.
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	linkPath := filepath.Join(src, "link.txt")
	if err := os.Symlink(filepath.Join(src, "file.txt"), linkPath); err != nil {
		t.Skipf("cannot create symlink (likely Windows): %v", err)
	}

	_, err := archiveutil.TarGzDir(src, "vol")
	if !errors.Is(err, archiveutil.ErrValidation) {
		t.Fatalf("expected ErrValidation for symlink in TarGzDir, got %T: %v", err, err)
	}
}

func TestTarGzDirFiles_SymlinkRejected(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	linkPath := filepath.Join(src, "link.txt")
	if err := os.Symlink(filepath.Join(src, "file.txt"), linkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := archiveutil.TarGzDirFiles(src, "vol", nil)
	if !errors.Is(err, archiveutil.ErrValidation) {
		t.Fatalf("expected ErrValidation for symlink in TarGzDirFiles, got %T: %v", err, err)
	}
}

// TestPackageVolume_SourceWithSymlink verifies that packaging a volume directory
// that contains a symlink returns ErrValidation (not a panic or silent success).
func TestPackageVolume_SourceWithSymlink(t *testing.T) {
	src := t.TempDir()
	// Add a real partition subdirectory with data.
	partDir := filepath.Join(src, "part")
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(partDir, "data.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	// Add a symlink at the volume root level (goes into root-files layer).
	if err := os.Symlink(filepath.Join(src, "part"), filepath.Join(src, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	ctx := context.Background()
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.PublishVolumeFromDir(ctx, src, "symlink-test", "sym.v1")
	if err == nil {
		t.Fatal("expected error when source volume contains a symlink, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %T: %v", err, err)
	}
}

// ── archiveutil prefix functions (unit tests) ─────────────────────────────────

func TestUntarGzDirUnderPrefix_RejectsOutsideEntry(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	content := []byte("bad")
	_ = tw.WriteHeader(&tar.Header{
		Name: "other/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gw.Close()

	err := archiveutil.UntarGzDirUnderPrefix(bytes.NewReader(buf.Bytes()), t.TempDir(), "vol/docs")
	if !errors.Is(err, archiveutil.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for entry outside prefix, got %T: %v", err, err)
	}
}

func TestUntarGzDirUnderPrefix_AcceptsValidEntry(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "vol/docs/", Typeflag: tar.TypeDir, Mode: 0o755})
	content := []byte("ok")
	_ = tw.WriteHeader(&tar.Header{
		Name: "vol/docs/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gw.Close()

	dest := t.TempDir()
	if err := archiveutil.UntarGzDirUnderPrefix(bytes.NewReader(buf.Bytes()), dest, "vol/docs"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "vol", "docs", "file.txt")); err != nil {
		t.Fatalf("expected file to be extracted: %v", err)
	}
}

func TestUntarGzDirRootFilesOnly_RejectsDeepEntry(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "vol/", Typeflag: tar.TypeDir, Mode: 0o755})
	content := []byte("nested")
	_ = tw.WriteHeader(&tar.Header{
		Name: "vol/sub/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gw.Close()

	err := archiveutil.UntarGzDirRootFilesOnly(bytes.NewReader(buf.Bytes()), t.TempDir(), "vol")
	if !errors.Is(err, archiveutil.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for deep entry in root-files, got %T: %v", err, err)
	}
}

func TestUntarGzDirRootFilesOnly_AcceptsTopLevelFile(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "vol/", Typeflag: tar.TypeDir, Mode: 0o755})
	content := []byte("readme")
	_ = tw.WriteHeader(&tar.Header{
		Name: "vol/README.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gw.Close()

	dest := t.TempDir()
	if err := archiveutil.UntarGzDirRootFilesOnly(bytes.NewReader(buf.Bytes()), dest, "vol"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "vol", "README.md")); err != nil {
		t.Fatalf("expected README.md to be extracted: %v", err)
	}
}

// ── sensitive literal check ───────────────────────────────────────────────────

func TestNoHardcodedRegistryPassword(t *testing.T) {
	root := "."
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "reports", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		// Only scan text files.
		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch ext {
		case ".go", ".md", ".json", ".yml", ".yaml", ".sh", ".txt":
		default:
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		// Avoid putting the literal in this source file; construct it at runtime.
		sensitive := "Harbor" + "12345"
		if strings.Contains(string(data), sensitive) {
			t.Errorf("sensitive literal %s found in %s", sensitive, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}
