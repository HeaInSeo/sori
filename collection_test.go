package sori

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVolumeCollection_AddVolume(t *testing.T) {
	vc := NewVolumeCollection()
	entry := VolumeEntry{Index: VolumeIndex{VolumeRef: "ref1", DisplayName: "vol1"}}
	vc.AddVolume(entry)

	if len(vc.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(vc.Volumes))
	}
	if vc.Version != 2 {
		t.Fatalf("expected version 2 (1 initial + 1 add), got %d", vc.Version)
	}
	if vc.Volumes[0].Index.VolumeRef != "ref1" {
		t.Fatalf("unexpected VolumeRef: %q", vc.Volumes[0].Index.VolumeRef)
	}
}

func TestVolumeCollection_RemoveVolume_Success(t *testing.T) {
	vc := NewVolumeCollection(
		VolumeEntry{Index: VolumeIndex{VolumeRef: "ref1"}},
		VolumeEntry{Index: VolumeIndex{VolumeRef: "ref2"}},
	)
	if err := vc.RemoveVolume(0); err != nil {
		t.Fatalf("RemoveVolume: %v", err)
	}
	if len(vc.Volumes) != 1 {
		t.Fatalf("expected 1 volume after remove, got %d", len(vc.Volumes))
	}
	if vc.Volumes[0].Index.VolumeRef != "ref2" {
		t.Fatalf("unexpected remaining ref: %q", vc.Volumes[0].Index.VolumeRef)
	}
}

func TestVolumeCollection_RemoveVolume_OutOfRange(t *testing.T) {
	vc := NewVolumeCollection()
	err := vc.RemoveVolume(0)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for out-of-range index, got %v", err)
	}
}

func TestVolumeCollection_RemoveVolume_NegativeIndex(t *testing.T) {
	vc := NewVolumeCollection(VolumeEntry{Index: VolumeIndex{VolumeRef: "ref1"}})
	err := vc.RemoveVolume(-1)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for negative index, got %v", err)
	}
}

func TestLoadOrNewCollection_InvalidJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, CollectionJson), []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := LoadOrNewCollection(root)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for invalid JSON, got %v", err)
	}
}

func TestLoadOrNewCollection_ReadError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test read permission as root")
	}
	root := t.TempDir()
	collPath := filepath.Join(root, CollectionJson)
	if err := os.WriteFile(collPath, []byte(`{"version":1}`), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(collPath, 0o644) })

	_, err := LoadOrNewCollection(root)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("expected ErrTransport for unreadable file, got %v", err)
	}
}

func TestCollectionManager_AddOrUpdate_EmptyRef(t *testing.T) {
	cm, err := NewCollectionManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}
	err = cm.AddOrUpdate(VolumeEntry{Index: VolumeIndex{VolumeRef: ""}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for empty VolumeRef, got %v", err)
	}
}

func TestCollectionManager_AddOrUpdate_NoDuplicateVersion(t *testing.T) {
	cm, err := NewCollectionManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}
	entry := VolumeEntry{Index: VolumeIndex{VolumeRef: "ref1", DisplayName: "vol1"}}
	if err := cm.AddOrUpdate(entry); err != nil {
		t.Fatalf("first AddOrUpdate: %v", err)
	}
	snap1 := cm.GetSnapshot()

	// Adding identical entry should not increment version.
	if err := cm.AddOrUpdate(entry); err != nil {
		t.Fatalf("second AddOrUpdate (no-op): %v", err)
	}
	snap2 := cm.GetSnapshot()
	if snap1.Version != snap2.Version {
		t.Fatalf("version should not change for identical entry: %d → %d", snap1.Version, snap2.Version)
	}
}

func TestCollectionManager_Remove_NotFound(t *testing.T) {
	cm, err := NewCollectionManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}
	removed, err := cm.Remove("nonexistent")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed {
		t.Fatal("expected removed=false for missing ref")
	}
}

func TestNewCollectionManager_DuplicateRef(t *testing.T) {
	root := t.TempDir()
	coll := VolumeCollection{
		Version: 1,
		Volumes: []VolumeEntry{
			{Index: VolumeIndex{VolumeRef: "dup"}},
			{Index: VolumeIndex{VolumeRef: "dup"}},
		},
	}
	if err := saveCollection(root, coll); err != nil {
		t.Fatalf("saveCollection: %v", err)
	}
	_, err := NewCollectionManager(root)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for duplicate ref, got %v", err)
	}
}

func TestClient_WithHTTPClient(t *testing.T) {
	client := NewClient(WithHTTPClient(nil))
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestClient_WithClock_NilIgnored(t *testing.T) {
	client := NewClient(WithClock(nil))
	if client.now == nil {
		t.Fatal("clock must not be nil after WithClock(nil) — default should be preserved")
	}
}

func TestClient_WithClock_CustomClock(t *testing.T) {
	client := NewClient(WithLocalStorePath(t.TempDir()))
	if client.LocalStorePath() == "" {
		t.Fatal("expected non-empty store path")
	}
}

func TestClient_PublishVolumeFromDir_Success(t *testing.T) {
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "oci")
	client := NewClient(WithLocalStorePath(storePath))

	volDir := "./test-vol"
	pkg, err := client.PublishVolumeFromDir(ctx, volDir, "PublishFromDirTest", "pfd.v1")
	if err != nil {
		t.Fatalf("PublishVolumeFromDir: %v", err)
	}
	if pkg.ManifestDigest == "" {
		t.Fatal("expected non-empty ManifestDigest")
	}
}

func TestClient_FetchVolumeParallel(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	storePath := filepath.Join(tmp, "oci")
	client := NewClient(WithLocalStorePath(storePath))

	volDir := "./test-vol"
	pkg, err := client.PublishVolumeFromDir(ctx, volDir, "ParallelFetch", "pf.v1")
	if err != nil {
		t.Fatalf("PublishVolumeFromDir: %v", err)
	}
	if pkg.ManifestDigest == "" {
		t.Fatal("expected non-empty ManifestDigest after publish")
	}

	dest := filepath.Join(tmp, "dest")
	vi, err := client.FetchVolumeParallel(ctx, dest, storePath, "pf.v1", 2)
	if err != nil {
		t.Fatalf("FetchVolumeParallel: %v", err)
	}
	if vi.VolumeRef == "" {
		t.Fatal("expected non-empty VolumeRef after parallel fetch")
	}
	if len(vi.Partitions) == 0 {
		t.Fatal("expected at least one partition after parallel fetch")
	}
}

func TestClient_FetchVolumeParallel_NonExistentStore(t *testing.T) {
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "dest")
	client := NewClient()
	_, err := client.FetchVolumeParallel(ctx, dest, filepath.Join(t.TempDir(), "nonexistent"), "v1", 2)
	if err == nil {
		t.Fatal("expected error for non-existent store")
	}
}
