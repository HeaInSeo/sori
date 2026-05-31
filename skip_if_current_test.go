package sori

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HeaInSeo/sori/chunked"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// publishAndServeChunked packages srcDir as a chunked CAS artifact into
// storePath and starts a mock OCI server.  Returns the server host and the
// manifest digest string.
func publishAndServeChunked(t *testing.T, storePath, srcDir, tag string) (host, manifestDigest string) {
	t.Helper()
	desc, err := chunked.Publish(context.Background(), storePath, srcDir, tag, chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err != nil {
		t.Fatalf("chunked.Publish: %v", err)
	}
	ts := newMockOCIServer(t, storePath, "repo", tag)
	return ts.Listener.Addr().String(), desc.Digest.String()
}

// TestSkipIfCurrent_ChunkedCAS: first fetch downloads; second fetch with
// SkipIfCurrent=true returns Skipped=true without re-downloading.
func TestSkipIfCurrent_ChunkedCAS(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()
	srcDir := newChunkedRemoteSrcDir(t)
	destRoot := filepath.Join(t.TempDir(), "dest")

	host, wantRef := publishAndServeChunked(t, storePath, srcDir, "v1")
	target := RemoteTarget{Registry: host, Repository: "repo", PlainHTTP: true}

	vi1, err := NewClient(WithLocalStorePath(t.TempDir())).FetchVolumeFromRemote(
		ctx, destRoot, target, "v1", FetchOptions{AtomicOverwrite: true})
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if vi1.Skipped {
		t.Error("first fetch must not be skipped")
	}
	if vi1.VolumeRef != wantRef {
		t.Errorf("VolumeRef=%q want %q", vi1.VolumeRef, wantRef)
	}

	vi2, err := NewClient(WithLocalStorePath(t.TempDir())).FetchVolumeFromRemote(
		ctx, destRoot, target, "v1", FetchOptions{AtomicOverwrite: true, SkipIfCurrent: true})
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if !vi2.Skipped {
		t.Error("second fetch must be skipped when digest matches")
	}
	if vi2.VolumeRef != wantRef {
		t.Errorf("skipped VolumeRef=%q want %q", vi2.VolumeRef, wantRef)
	}
	assertRemoteDirContentsEqual(t, srcDir, destRoot)
}

// TestSkipIfCurrent_StaleVersion: when a new version is available
// (different manifest digest) the fetch proceeds and destRoot is updated.
func TestSkipIfCurrent_StaleVersion(t *testing.T) {
	ctx := context.Background()
	storeV1 := t.TempDir()
	storeV2 := t.TempDir()
	srcV1 := newChunkedRemoteSrcDir(t)
	destRoot := filepath.Join(t.TempDir(), "dest")

	hostV1, _ := publishAndServeChunked(t, storeV1, srcV1, "mytag")
	if _, err := NewClient(WithLocalStorePath(t.TempDir())).FetchVolumeFromRemote(
		ctx, destRoot, RemoteTarget{Registry: hostV1, Repository: "repo", PlainHTTP: true},
		"mytag", FetchOptions{AtomicOverwrite: true}); err != nil {
		t.Fatalf("fetch v1: %v", err)
	}

	srcV2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcV2, "newfile.txt"), []byte("v2 content"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	hostV2, wantRefV2 := publishAndServeChunked(t, storeV2, srcV2, "mytag")

	vi, err := NewClient(WithLocalStorePath(t.TempDir())).FetchVolumeFromRemote(
		ctx, destRoot, RemoteTarget{Registry: hostV2, Repository: "repo", PlainHTTP: true},
		"mytag", FetchOptions{AtomicOverwrite: true, SkipIfCurrent: true})
	if err != nil {
		t.Fatalf("fetch v2: %v", err)
	}
	if vi.Skipped {
		t.Error("fetch must not be skipped when remote digest differs from local")
	}
	if vi.VolumeRef != wantRefV2 {
		t.Errorf("VolumeRef=%q want %q", vi.VolumeRef, wantRefV2)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "newfile.txt")); err != nil {
		t.Errorf("v2 file must exist after stale-update fetch: %v", err)
	}
}

// TestSkipIfCurrent_NoLocalIndex: destRoot has no volume-index.json →
// fetch proceeds normally.
func TestSkipIfCurrent_NoLocalIndex(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()
	srcDir := newChunkedRemoteSrcDir(t)
	destRoot := filepath.Join(t.TempDir(), "dest")

	host, wantRef := publishAndServeChunked(t, storePath, srcDir, "v1")
	target := RemoteTarget{Registry: host, Repository: "repo", PlainHTTP: true}

	vi, err := NewClient(WithLocalStorePath(t.TempDir())).FetchVolumeFromRemote(
		ctx, destRoot, target, "v1", FetchOptions{AtomicOverwrite: true, SkipIfCurrent: true})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if vi.Skipped {
		t.Error("must not skip when no local index exists")
	}
	if vi.VolumeRef != wantRef {
		t.Errorf("VolumeRef=%q want %q", vi.VolumeRef, wantRef)
	}
	assertRemoteDirContentsEqual(t, srcDir, destRoot)
}

// TestSkipIfCurrent_EmptyVolumeRef: local volume-index.json has VolumeRef=""
// → treated as no match, full fetch proceeds.
func TestSkipIfCurrent_EmptyVolumeRef(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()
	srcDir := newChunkedRemoteSrcDir(t)
	destRoot := filepath.Join(t.TempDir(), "dest")

	host, wantRef := publishAndServeChunked(t, storePath, srcDir, "v1")
	target := RemoteTarget{Registry: host, Repository: "repo", PlainHTTP: true}

	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeIndex(destRoot, &VolumeIndex{VolumeRef: "", DisplayName: "stale"}); err != nil {
		t.Fatalf("write stale index: %v", err)
	}

	vi, err := NewClient(WithLocalStorePath(t.TempDir())).FetchVolumeFromRemote(
		ctx, destRoot, target, "v1", FetchOptions{AtomicOverwrite: true, SkipIfCurrent: true})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if vi.Skipped {
		t.Error("empty VolumeRef must not trigger skip")
	}
	if vi.VolumeRef != wantRef {
		t.Errorf("VolumeRef=%q want %q", vi.VolumeRef, wantRef)
	}
}

// TestSkipIfCurrent_RequireEmpty_Conflict: RequireEmptyDestination and
// SkipIfCurrent together must return ErrValidation.
func TestSkipIfCurrent_RequireEmpty_Conflict(t *testing.T) {
	ctx := context.Background()
	_, err := NewClient(WithLocalStorePath(t.TempDir())).FetchVolumeFromRemote(
		ctx, t.TempDir(),
		RemoteTarget{Registry: "localhost:5000", Repository: "r", PlainHTTP: true},
		"v1",
		FetchOptions{RequireEmptyDestination: true, SkipIfCurrent: true})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

// TestSkipIfCurrent_Legacy: SkipIfCurrent works on the legacy tar.gz path.
func TestSkipIfCurrent_Legacy(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "leg.v1")
	ts := newMockOCIServer(t, storePath, "repo", "leg.v1")
	host := ts.Listener.Addr().String()
	target := RemoteTarget{Registry: host, Repository: "repo", PlainHTTP: true}
	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(t.TempDir()))

	// First fetch — must download.
	vi1, err := client.FetchVolumeFromRemote(ctx, destRoot, target, "leg.v1",
		FetchOptions{AtomicOverwrite: true})
	if err != nil {
		t.Fatalf("first fetch (legacy): %v", err)
	}
	if vi1.Skipped {
		t.Error("first legacy fetch must not be skipped")
	}

	// Second fetch — same digest → skip.
	vi2, err := client.FetchVolumeFromRemote(ctx, destRoot, target, "leg.v1",
		FetchOptions{AtomicOverwrite: true, SkipIfCurrent: true})
	if err != nil {
		t.Fatalf("second fetch (legacy): %v", err)
	}
	if !vi2.Skipped {
		t.Error("second legacy fetch must be skipped")
	}
	if vi2.VolumeRef != vi1.VolumeRef {
		t.Errorf("VolumeRef mismatch: %q vs %q", vi2.VolumeRef, vi1.VolumeRef)
	}
}

// TestEnsureVolumeFromRemote: convenience wrapper downloads on first call and
// skips on second call.
func TestEnsureVolumeFromRemote(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()
	srcDir := newChunkedRemoteSrcDir(t)
	destRoot := filepath.Join(t.TempDir(), "dest")

	host, wantRef := publishAndServeChunked(t, storePath, srcDir, "latest")
	target := RemoteTarget{Registry: host, Repository: "repo", PlainHTTP: true}
	client := NewClient(WithLocalStorePath(t.TempDir()))

	vi1, err := client.EnsureVolumeFromRemote(ctx, destRoot, target, "latest")
	if err != nil {
		t.Fatalf("EnsureVolumeFromRemote (first): %v", err)
	}
	if vi1.Skipped {
		t.Error("first call must not skip")
	}
	if vi1.VolumeRef != wantRef {
		t.Errorf("VolumeRef=%q want %q", vi1.VolumeRef, wantRef)
	}
	assertRemoteDirContentsEqual(t, srcDir, destRoot)

	vi2, err := client.EnsureVolumeFromRemote(ctx, destRoot, target, "latest")
	if err != nil {
		t.Fatalf("EnsureVolumeFromRemote (second): %v", err)
	}
	if !vi2.Skipped {
		t.Error("second call must skip")
	}
	if vi2.VolumeRef != wantRef {
		t.Errorf("skipped VolumeRef=%q want %q", vi2.VolumeRef, wantRef)
	}

	// Skipped must not be persisted to volume-index.json.
	localVI, err := readLocalVolumeIndex(destRoot)
	if err != nil {
		t.Fatalf("readLocalVolumeIndex: %v", err)
	}
	if localVI.Skipped {
		t.Error("Skipped must not be persisted to volume-index.json")
	}
	assertRemoteDirContentsEqual(t, srcDir, destRoot)
}

// TestSkipIfCurrent_SkippedNotInJSON: VolumeIndex.Skipped must not appear in
// the marshalled JSON (json:"-" tag).
func TestSkipIfCurrent_SkippedNotInJSON(t *testing.T) {
	vi := VolumeIndex{VolumeRef: "sha256:abc", DisplayName: "test", Skipped: true}
	b, err := json.Marshal(vi)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if bytes.Contains(b, []byte("skipped")) {
		t.Errorf("marshalled VolumeIndex must not contain 'skipped' key, got: %s", b)
	}
}
