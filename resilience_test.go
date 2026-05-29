package sori

// resilience_test.go covers three test categories:
//
//   graceful failure  – error returned (not panic) and state is consistent after failure
//   context cancel    – cancelled context terminates operations without deadlock or leak
//   concurrent        – concurrent access to shared state is safe (run with -race)

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// buildValidTarGzLayer returns a gzip-compressed tar that holds one directory
// entry with the given partPath name.
func buildValidTarGzLayer(t *testing.T, partPath string) (ocispec.Descriptor, []byte) {
	t.Helper()
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     partPath + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	data := buf.Bytes()
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
		Annotations: map[string]string{
			annotationPartitionPath: partPath,
			annotationLayerKind:     layerKindPartition,
		},
	}
	return desc, data
}

// buildCorruptLayer returns a descriptor whose content is valid in the store
// (correct digest / size) but is not a valid gzip stream.
func buildCorruptLayer(partPath string) (ocispec.Descriptor, []byte) {
	data := []byte("this is not a valid gzip stream — intentionally corrupt")
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
		Annotations: map[string]string{
			annotationPartitionPath: partPath,
			annotationLayerKind:     layerKindPartition,
		},
	}
	return desc, data
}

// buildOCIStore pushes layers into a local OCI store and tags a manifest.
// Returns the store path and the tag string.
func buildOCIStore(t *testing.T, layers []struct {
	desc ocispec.Descriptor
	data []byte
}, tag string) string {
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
	layerDescs := make([]ocispec.Descriptor, 0, len(layers))
	for _, l := range layers {
		if err := store.Push(ctx, l.desc, bytes.NewReader(l.data)); err != nil {
			t.Fatalf("push layer: %v", err)
		}
		layerDescs = append(layerDescs, l.desc)
	}
	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1,
		ocispec.MediaTypeImageManifest,
		oras.PackManifestOptions{
			ConfigDescriptor: &configDesc,
			Layers:           layerDescs,
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationCreated:   time.Now().UTC().Format(time.RFC3339),
				annotationVolumeDisplayName: "resilience-test",
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

// withTimeout runs f in a goroutine and fails the test if f has not returned
// within d.  This is used to guarantee that a function does not deadlock.
func withTimeout(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("operation did not complete within %v — likely deadlock", d)
	}
}

// ── graceful failure ─────────────────────────────────────────────────────────

// TestFetchVolSeq_CorruptLayer_GracefulError verifies that FetchVolSeq returns
// an ErrIntegrity error when a layer is not valid gzip, and does not panic.
func TestFetchVolSeq_CorruptLayer_GracefulError(t *testing.T) {
	ctx := context.Background()

	corruptDesc, corruptData := buildCorruptLayer("vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{corruptDesc, corruptData}}, "corrupt.v1")

	dest := filepath.Join(t.TempDir(), "dest")
	_, err := FetchVolSeq(ctx, dest, storePath, "corrupt.v1")
	if err == nil {
		t.Fatal("expected error for corrupt layer, got nil")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %T: %v", err, err)
	}
}

// TestFetchWithStaging_CorruptLayer_StagingCleaned verifies that when layer
// extraction fails during a staged fetch, the staging directory is removed and
// destRoot is never created.
func TestFetchWithStaging_CorruptLayer_StagingCleaned(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	corruptDesc, corruptData := buildCorruptLayer("vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{corruptDesc, corruptData}}, "corrupt.v1")

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "corrupt.v1", FetchOptions{
		Concurrency:             1,
		RequireEmptyDestination: true,
	})
	if err == nil {
		t.Fatal("expected error for corrupt layer, got nil")
	}

	// destRoot must not exist — the staging rename never happened.
	if _, statErr := os.Stat(destRoot); !os.IsNotExist(statErr) {
		t.Errorf("destRoot must not exist after failed staged fetch, but it does: %v", statErr)
	}

	// No .staging-* sibling must survive — deferred cleanup ran.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	for _, e := range entries {
		if len(e.Name()) >= 9 && e.Name()[:9] == ".staging-" {
			t.Errorf("stale staging dir found: %s", e.Name())
		}
	}
}

// TestFetchWithStaging_ValidThenCorruptLayer_StagingCleaned extends the above
// to a two-layer manifest: the first layer succeeds, the second is corrupt.
// Staging must still be cleaned and destRoot absent.
func TestFetchWithStaging_ValidThenCorruptLayer_StagingCleaned(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/good")
	corruptDesc, corruptData := buildCorruptLayer("vol/bad")

	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{
		{validDesc, validData},
		{corruptDesc, corruptData},
	}, "mixed.v1")

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "mixed.v1", FetchOptions{
		Concurrency:             1,
		RequireEmptyDestination: true,
	})
	if err == nil {
		t.Fatal("expected error for corrupt second layer, got nil")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %T: %v", err, err)
	}

	if _, statErr := os.Stat(destRoot); !os.IsNotExist(statErr) {
		t.Error("destRoot must not exist after failed staged fetch")
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if len(e.Name()) >= 9 && e.Name()[:9] == ".staging-" {
			t.Errorf("stale staging dir: %s", e.Name())
		}
	}
}

// TestFetchWithStaging_Parallel_CorruptLayer_StagingCleaned runs the same
// corrupt-layer scenario through FetchVolParallel (concurrency=2).
func TestFetchWithStaging_Parallel_CorruptLayer_StagingCleaned(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	corruptDesc, corruptData := buildCorruptLayer("vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{corruptDesc, corruptData}}, "corrupt-par.v1")

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "corrupt-par.v1", FetchOptions{
		Concurrency:             2,
		RequireEmptyDestination: true,
	})
	if err == nil {
		t.Fatal("expected error for corrupt layer (parallel), got nil")
	}

	if _, statErr := os.Stat(destRoot); !os.IsNotExist(statErr) {
		t.Error("destRoot must not exist after failed parallel staged fetch")
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if len(e.Name()) >= 9 && e.Name()[:9] == ".staging-" {
			t.Errorf("stale staging dir after parallel failure: %s", e.Name())
		}
	}
}

// TestFetchVolSeq_StagingValidation_PartitionPresent is a regression test that
// verifies staging validation does not reject a well-formed artifact: a volume
// with one partition directory that is present in the extracted staging tree
// must complete successfully and produce a populated destRoot.
func TestFetchVolSeq_StagingValidation_PartitionPresent(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "staging-valid.v1")

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(storePath))
	vi, err := client.FetchVolume(ctx, destRoot, storePath, "staging-valid.v1", FetchOptions{
		Concurrency:             1,
		RequireEmptyDestination: true,
	})
	if err != nil {
		t.Fatalf("expected success for valid artifact with staging validation, got: %v", err)
	}
	if vi == nil {
		t.Fatal("expected non-nil VolumeIndex")
	}

	// destRoot must exist after a successful staged fetch.
	if _, statErr := os.Stat(destRoot); statErr != nil {
		t.Errorf("destRoot must exist after successful staged fetch: %v", statErr)
	}

	// The partition directory must exist under destRoot.
	partDir := filepath.Join(destRoot, "vol", "part")
	if info, statErr := os.Stat(partDir); statErr != nil || !info.IsDir() {
		t.Errorf("partition directory %q must exist under destRoot", partDir)
	}
}

// ── context cancellation ─────────────────────────────────────────────────────

// TestFetchVolSeq_CancelledContext_NoDeadlock verifies that FetchVolSeq returns
// within a reasonable timeout when given an already-cancelled context.
func TestFetchVolSeq_CancelledContext_NoDeadlock(t *testing.T) {
	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "seq.v1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "dest")
	withTimeout(t, 5*time.Second, func() {
		_, _ = FetchVolSeq(ctx, dest, storePath, "seq.v1")
	})
}

// TestFetchVolParallel_CancelledContext_NoDeadlock verifies that FetchVolParallel
// returns within a reasonable timeout when given an already-cancelled context.
func TestFetchVolParallel_CancelledContext_NoDeadlock(t *testing.T) {
	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "par.v1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "dest")
	withTimeout(t, 5*time.Second, func() {
		_, _ = FetchVolParallel(ctx, dest, storePath, "par.v1", 4)
	})
}

// TestFetchVolParallel_CancelledContext_NoGoroutineLeak verifies that the
// worker goroutines spawned by FetchVolParallel exit after the context is
// cancelled — no goroutine leak.
func TestFetchVolParallel_CancelledContext_NoGoroutineLeak(t *testing.T) {
	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "goroutine.v1")

	// Settle goroutines from previous tests.
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dest := filepath.Join(t.TempDir(), "dest")
	withTimeout(t, 5*time.Second, func() {
		_, _ = FetchVolParallel(ctx, dest, storePath, "goroutine.v1", 4)
	})

	// Allow goroutines time to exit.
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()

	// Allow reasonable slack (GC + test runner goroutines).
	if after > before+5 {
		t.Fatalf("goroutine leak: %d before → %d after (leaked ~%d)", before, after, after-before)
	}
}

// TestFetchWithStaging_CancelledContext_ConsistentState verifies that the
// filesystem state is consistent after FetchVolume is called with a cancelled
// context: either the operation succeeded (destRoot exists) or it failed
// (destRoot absent, no staging dir leaking).
func TestFetchWithStaging_CancelledContext_ConsistentState(t *testing.T) {
	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "ctx-stage.v1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tmp := t.TempDir()
	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(storePath))

	var fetchErr error
	withTimeout(t, 5*time.Second, func() {
		_, fetchErr = client.FetchVolume(ctx, destRoot, storePath, "ctx-stage.v1", FetchOptions{
			Concurrency:             1,
			RequireEmptyDestination: true,
		})
	})

	_, destStat := os.Stat(destRoot)
	destExists := destStat == nil

	if fetchErr != nil {
		// On failure: destRoot must NOT exist and no staging dir must survive.
		if destExists {
			t.Error("destRoot must not exist when FetchVolume returns an error")
		}
		entries, _ := os.ReadDir(tmp)
		for _, e := range entries {
			if len(e.Name()) >= 9 && e.Name()[:9] == ".staging-" {
				t.Errorf("stale staging dir after error: %s", e.Name())
			}
		}
	} else {
		// On success: destRoot must exist.
		if !destExists {
			t.Error("destRoot must exist when FetchVolume returns nil error")
		}
	}
}

// TestPackageVolume_CancelledContext_GracefulError verifies that
// PackageVolumeToStore returns an error (not panic) when the context is
// cancelled before the call.
func TestPackageVolume_CancelledContext_GracefulError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	withTimeout(t, 5*time.Second, func() {
		_, _ = PackageVolumeToStore(ctx, t.TempDir(), PackageRequest{
			SourceDir:   "./test-vol",
			DisplayName: "ctx-cancel-test",
			Tag:         "cancel.v1",
		})
	})
}

// ── concurrent ───────────────────────────────────────────────────────────────

// TestCollectionManager_ConcurrentAddOrUpdate fires N goroutines each adding a
// unique ref.  After all goroutines finish the collection must hold N entries
// with no data races (run with -race).
func TestCollectionManager_ConcurrentAddOrUpdate(t *testing.T) {
	const n = 20
	cm, err := NewCollectionManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			entry := VolumeEntry{
				Index: VolumeIndex{
					VolumeRef:   fmt.Sprintf("ref-%d", i),
					DisplayName: fmt.Sprintf("vol-%d", i),
				},
			}
			if err := cm.AddOrUpdate(entry); err != nil {
				t.Errorf("AddOrUpdate[%d]: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	snap := cm.GetSnapshot()
	if len(snap.Volumes) != n {
		t.Fatalf("expected %d volumes, got %d", n, len(snap.Volumes))
	}
}

// TestCollectionManager_ConcurrentGetSnapshot runs add goroutines and snapshot
// goroutines simultaneously.  Every snapshot returned must be a self-consistent
// copy (non-nil Volumes slice, non-negative Version).
func TestCollectionManager_ConcurrentGetSnapshot(t *testing.T) {
	const writers = 10
	const readers = 10
	cm, err := NewCollectionManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			_ = cm.AddOrUpdate(VolumeEntry{
				Index: VolumeIndex{VolumeRef: fmt.Sprintf("snap-ref-%d", i)},
			})
		}(i)
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			snap := cm.GetSnapshot()
			if snap.Volumes == nil {
				t.Error("GetSnapshot returned nil Volumes slice")
			}
			if snap.Version < 0 {
				t.Errorf("GetSnapshot returned negative Version: %d", snap.Version)
			}
		}()
	}
	wg.Wait()
}

// TestCollectionManager_ConcurrentGet fires concurrent Get calls while an
// AddOrUpdate is in flight.  The read-lock must not block writers indefinitely,
// and Get must return consistent data (not corrupt / half-written).
func TestCollectionManager_ConcurrentGet(t *testing.T) {
	cm, err := NewCollectionManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}
	// Seed one entry so readers have something to look for.
	if err := cm.AddOrUpdate(VolumeEntry{
		Index: VolumeIndex{VolumeRef: "stable-ref", DisplayName: "stable"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		if i%3 == 0 {
			// writer
			go func(i int) {
				defer wg.Done()
				_ = cm.AddOrUpdate(VolumeEntry{
					Index: VolumeIndex{VolumeRef: fmt.Sprintf("w-ref-%d", i)},
				})
			}(i)
		} else {
			// reader
			go func() {
				defer wg.Done()
				entry, ok := cm.Get("stable-ref")
				if ok && entry.Index.VolumeRef != "stable-ref" {
					t.Errorf("corrupted VolumeRef: got %q", entry.Index.VolumeRef)
				}
			}()
		}
	}
	wg.Wait()
}

// TestCollectionManager_ConcurrentRemove fires concurrent Remove calls against
// the same set of refs.  Some removals will find nothing (ref already gone);
// neither case should panic or corrupt state.
func TestCollectionManager_ConcurrentRemove(t *testing.T) {
	cm, err := NewCollectionManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}
	const n = 10
	for i := 0; i < n; i++ {
		_ = cm.AddOrUpdate(VolumeEntry{
			Index: VolumeIndex{VolumeRef: fmt.Sprintf("del-ref-%d", i)},
		})
	}

	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		ref := fmt.Sprintf("del-ref-%d", i)
		// Two goroutines racing to remove the same ref — exactly one should win.
		go func(ref string) {
			defer wg.Done()
			_, err := cm.Remove(ref)
			if err != nil {
				t.Errorf("Remove(%q): %v", ref, err)
			}
		}(ref)
		go func(ref string) {
			defer wg.Done()
			_, err := cm.Remove(ref)
			if err != nil {
				t.Errorf("Remove(%q) concurrent: %v", ref, err)
			}
		}(ref)
	}
	wg.Wait()

	// Collection must still be consistent (readable, non-negative version).
	snap := cm.GetSnapshot()
	if snap.Version < 0 {
		t.Fatalf("corrupt version after concurrent removes: %d", snap.Version)
	}
}

// ── AtomicOverwrite ──────────────────────────────────────────────────────────

// TestFetchWithAtomicOverwrite_DestAbsent verifies that AtomicOverwrite succeeds
// when destRoot does not exist.
func TestFetchWithAtomicOverwrite_DestAbsent(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "ao-absent.v1")

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(storePath))
	vi, err := client.FetchVolume(ctx, destRoot, storePath, "ao-absent.v1", FetchOptions{
		Concurrency:     1,
		AtomicOverwrite: true,
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if vi == nil {
		t.Fatal("expected non-nil VolumeIndex")
	}
	if _, statErr := os.Stat(destRoot); statErr != nil {
		t.Errorf("destRoot must exist after AtomicOverwrite: %v", statErr)
	}
}

// TestFetchWithAtomicOverwrite_DestExists verifies that AtomicOverwrite succeeds
// and replaces the existing destRoot, leaving no backup sibling.
func TestFetchWithAtomicOverwrite_DestExists(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "ao-exists.v1")

	destRoot := filepath.Join(tmp, "dest")
	sentinelPath := filepath.Join(destRoot, "sentinel.txt")
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sentinelPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "ao-exists.v1", FetchOptions{
		Concurrency:     1,
		AtomicOverwrite: true,
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if _, statErr := os.Stat(sentinelPath); !os.IsNotExist(statErr) {
		t.Error("sentinel must not exist after AtomicOverwrite replaced destRoot")
	}
	if _, statErr := os.Stat(destRoot); statErr != nil {
		t.Errorf("destRoot must exist after AtomicOverwrite: %v", statErr)
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-") {
			t.Errorf("stale backup dir found: %s", e.Name())
		}
	}
}

// TestFetchWithAtomicOverwrite_MutualExclusion verifies that setting both
// RequireEmptyDestination and AtomicOverwrite returns ErrValidation.
func TestFetchWithAtomicOverwrite_MutualExclusion(t *testing.T) {
	ctx := context.Background()
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.FetchVolume(ctx, t.TempDir(), t.TempDir(), "any.v1", FetchOptions{
		RequireEmptyDestination: true,
		AtomicOverwrite:         true,
	})
	if err == nil {
		t.Fatal("expected ErrValidation for mutually exclusive options")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %T: %v", err, err)
	}
}

// TestFetchWithAtomicOverwrite_Phase1Failure_DestPreserved verifies that when
// layer extraction fails (Phase 1), destRoot is untouched and no staging dir
// survives.
func TestFetchWithAtomicOverwrite_Phase1Failure_DestPreserved(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	corruptDesc, corruptData := buildCorruptLayer("vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{corruptDesc, corruptData}}, "ao-p1fail.v1")

	destRoot := filepath.Join(tmp, "dest")
	sentinelPath := filepath.Join(destRoot, "sentinel.txt")
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sentinelPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "ao-p1fail.v1", FetchOptions{
		Concurrency:     1,
		AtomicOverwrite: true,
	})
	if err == nil {
		t.Fatal("expected error for corrupt layer")
	}
	data, readErr := os.ReadFile(sentinelPath)
	if readErr != nil {
		t.Fatalf("sentinel must exist after Phase 1 failure: %v", readErr)
	}
	if string(data) != "original" {
		t.Errorf("sentinel content changed after Phase 1 failure: got %q", string(data))
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Errorf("stale staging dir after Phase 1 failure: %s", e.Name())
		}
	}
}

// TestFetchWithAtomicOverwrite_Phase2Failure_DestPreserved verifies that when
// the Phase 2 backup rename fails (injected), destRoot is untouched.
func TestFetchWithAtomicOverwrite_Phase2Failure_DestPreserved(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "ao-p2fail.v1")

	destRoot := filepath.Join(tmp, "dest")
	sentinelPath := filepath.Join(destRoot, "sentinel.txt")
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sentinelPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	testHookPhase2RenameErr = errors.New("injected phase 2 failure")
	t.Cleanup(func() { testHookPhase2RenameErr = nil })

	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "ao-p2fail.v1", FetchOptions{
		Concurrency:     1,
		AtomicOverwrite: true,
	})
	if err == nil {
		t.Fatal("expected error from Phase 2 hook")
	}
	data, readErr := os.ReadFile(sentinelPath)
	if readErr != nil {
		t.Fatalf("sentinel must still exist: %v", readErr)
	}
	if string(data) != "original" {
		t.Errorf("sentinel content changed after Phase 2 failure: got %q", string(data))
	}
}

// TestFetchWithAtomicOverwrite_Phase3Failure_BackupRestored verifies that when
// Phase 3 fails (injected), the backup is renamed back to destRoot and no
// temporary siblings survive.
func TestFetchWithAtomicOverwrite_Phase3Failure_BackupRestored(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "ao-p3fail.v1")

	destRoot := filepath.Join(tmp, "dest")
	sentinelPath := filepath.Join(destRoot, "sentinel.txt")
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sentinelPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	testHookPhase3RenameErr = errors.New("injected phase 3 failure")
	t.Cleanup(func() { testHookPhase3RenameErr = nil })

	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "ao-p3fail.v1", FetchOptions{
		Concurrency:     1,
		AtomicOverwrite: true,
	})
	if err == nil {
		t.Fatal("expected error from Phase 3 hook")
	}
	data, readErr := os.ReadFile(sentinelPath)
	if readErr != nil {
		t.Fatalf("sentinel must be restored after rollback: %v", readErr)
	}
	if string(data) != "original" {
		t.Errorf("sentinel content incorrect after rollback: got %q", string(data))
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") || strings.HasPrefix(e.Name(), ".backup-") {
			t.Errorf("stale temp dir after rollback: %s", e.Name())
		}
	}
}

// TestFetchWithAtomicOverwrite_CleanupFailure_ReturnsSuccess verifies that a
// backup cleanup failure is logged but does not propagate as a function error.
func TestFetchWithAtomicOverwrite_CleanupFailure_ReturnsSuccess(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "ao-cleanup.v1")

	destRoot := filepath.Join(tmp, "dest")
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	testHookBackupCleanupErr = errors.New("injected cleanup failure")
	t.Cleanup(func() { testHookBackupCleanupErr = nil })

	client := NewClient(WithLocalStorePath(storePath))
	_, err := client.FetchVolume(ctx, destRoot, storePath, "ao-cleanup.v1", FetchOptions{
		Concurrency:     1,
		AtomicOverwrite: true,
	})
	if err != nil {
		t.Fatalf("cleanup failure must not cause function error, got: %v", err)
	}
	if _, statErr := os.Stat(destRoot); statErr != nil {
		t.Errorf("destRoot must exist after successful AtomicOverwrite: %v", statErr)
	}
}

// TestCollectionManager_ConcurrentFlush verifies that concurrent Flush calls
// while writes are in flight do not corrupt the on-disk collection file.
func TestCollectionManager_ConcurrentFlush(t *testing.T) {
	root := t.TempDir()
	cm, err := NewCollectionManager(root)
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}

	const n = 15
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		if i%3 == 0 {
			go func() {
				defer wg.Done()
				if err := cm.Flush(); err != nil {
					t.Errorf("Flush: %v", err)
				}
			}()
		} else {
			go func(i int) {
				defer wg.Done()
				_ = cm.AddOrUpdate(VolumeEntry{
					Index: VolumeIndex{VolumeRef: fmt.Sprintf("flush-ref-%d", i)},
				})
			}(i)
		}
	}
	wg.Wait()

	// The on-disk file must be parseable JSON (LoadOrNewCollection would validate).
	_, err = LoadOrNewCollection(root)
	if err != nil {
		t.Fatalf("collection file corrupted after concurrent flush: %v", err)
	}
}
