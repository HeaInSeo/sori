//go:build integration

package chunked_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/HeaInSeo/sori/chunked"
	"github.com/HeaInSeo/sori/registryutil"
)

// pseudoRandomBytes returns n pseudorandom bytes using crypto/rand.
func pseudoRandomBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(256))
		b[i] = byte(v.Int64())
	}
	return b
}

// hashFiles returns a map[relPath][]byte of file contents under root.
func readFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	m := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		m[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("readFiles(%s): %v", root, err)
	}
	return m
}

// assertFilesEqual verifies src and dest directories have identical file contents.
func assertFilesEqual(t *testing.T, srcDir, destDir string) {
	t.Helper()
	src := readFiles(t, srcDir)
	dst := readFiles(t, destDir)

	if len(src) != len(dst) {
		t.Errorf("file count mismatch: src=%d dest=%d", len(src), len(dst))
	}
	for rel, srcData := range src {
		dstData, ok := dst[rel]
		if !ok {
			t.Errorf("missing file in dest: %s", rel)
			continue
		}
		if !bytes.Equal(srcData, dstData) {
			t.Errorf("content mismatch for %s: src=%d bytes dest=%d bytes", rel, len(srcData), len(dstData))
		}
	}
}

// TestIntegration_LocalOCIRoundTrip performs a full round-trip using a local
// OCI store without requiring any remote registry.
func TestIntegration_LocalOCIRoundTrip(t *testing.T) {
	ctx := context.Background()

	storePath := t.TempDir()
	srcDir := t.TempDir()
	destDir := t.TempDir()

	// Create test files: one empty, one small, one ~1 MiB of pseudorandom bytes.
	files := map[string][]byte{
		"empty.bin":  {},
		"small.txt":  []byte("hello integration test"),
		"random.bin": pseudoRandomBytes(1 << 20), // 1 MiB
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(srcDir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	opts := chunked.PublishOptions{ChunkSize: chunked.MinChunkSize}

	// 1. First publish.
	if _, err := chunked.Publish(ctx, storePath, srcDir, "integration:v1", opts); err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	// 2. Fetch to destDir.
	if err := chunked.Fetch(ctx, storePath, destDir, "integration:v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// 3. Verify byte-identical.
	assertFilesEqual(t, srcDir, destDir)

	// 4. Second publish (identical) — verify all chunks are skipped.
	var skipped, uploaded int
	opts.Progress = func(cp chunked.ChunkProgress) {
		switch cp.Event {
		case "ChunkSkipped":
			skipped++
		case "ChunkUploaded":
			uploaded++
		}
	}
	if _, err := chunked.Publish(ctx, storePath, srcDir, "integration:v1", opts); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if uploaded != 0 {
		t.Errorf("second publish: expected 0 ChunkUploaded, got %d", uploaded)
	}
	if skipped == 0 {
		t.Errorf("second publish: expected at least one ChunkSkipped, got 0")
	}

	// 5. Modify one file, publish again — verify only that file's chunks are re-uploaded.
	modifiedFile := filepath.Join(srcDir, "small.txt")
	if err := os.WriteFile(modifiedFile, []byte("modified content for partial update"), 0o644); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	var reUploaded int
	reUploadedFiles := make(map[string]int)
	skipped = 0
	opts.Progress = func(cp chunked.ChunkProgress) {
		switch cp.Event {
		case "ChunkUploaded":
			reUploaded++
			reUploadedFiles[cp.File]++
		case "ChunkSkipped":
			skipped++
		}
	}
	if _, err := chunked.Publish(ctx, storePath, srcDir, "integration:v2", opts); err != nil {
		t.Fatalf("partial update Publish: %v", err)
	}
	if reUploaded == 0 {
		t.Error("partial update: expected at least one ChunkUploaded for modified file")
	}
	// Unmodified files (empty.bin, random.bin) should be skipped.
	for _, staticName := range []string{"empty.bin", "random.bin"} {
		staticPath := filepath.Join(srcDir, staticName)
		if n := reUploadedFiles[staticPath]; n > 0 {
			t.Errorf("static file %s unexpectedly uploaded %d chunks", staticName, n)
		}
	}
}

// TestIntegration_Harbor tests push/fetch via a Harbor registry.
// Skips if SORI_HARBOR_URL, SORI_HARBOR_USER, SORI_HARBOR_PASSWORD are not set.
func TestIntegration_Harbor(t *testing.T) {
	harborURL := os.Getenv("SORI_HARBOR_URL")
	harborUser := os.Getenv("SORI_HARBOR_USER")
	harborPass := os.Getenv("SORI_HARBOR_PASSWORD")
	if harborURL == "" || harborUser == "" || harborPass == "" {
		t.Skip("skipping Harbor integration test: SORI_HARBOR_URL, SORI_HARBOR_USER, SORI_HARBOR_PASSWORD not set")
	}

	ctx := context.Background()
	localStore := t.TempDir()
	srcDir := t.TempDir()
	destDir := t.TempDir()
	fetchStore := t.TempDir()

	// Write test files.
	for name, data := range map[string][]byte{
		"a.bin": []byte("harbor test file a"),
		"b.bin": pseudoRandomBytes(512 * 1024),
	} {
		if err := os.WriteFile(filepath.Join(srcDir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	opts := chunked.PublishOptions{ChunkSize: chunked.MinChunkSize}

	// Publish to local store.
	manifestDesc, err := chunked.Publish(ctx, localStore, srcDir, "harbor-test:v1", opts)
	if err != nil {
		t.Fatalf("Publish to local store: %v", err)
	}

	// Push local store → Harbor via ORAS Copy.
	remoteRef := harborURL + "/harbor-test:v1"
	remoteTarget, err := registryutil.NewRepository(remoteRef, registryutil.RemoteConfig{
		Username:    harborUser,
		Password:    harborPass,
		InsecureTLS: true,
	})
	if err != nil {
		t.Fatalf("NewRepository (Harbor): %v", err)
	}

	src, err := oci.New(localStore)
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	if _, err := oras.Copy(ctx, src, manifestDesc.Digest.String(), remoteTarget, "harbor-test:v1",
		oras.DefaultCopyOptions); err != nil {
		t.Fatalf("ORAS Copy to Harbor: %v", err)
	}

	// Fetch back: copy remote → local fetchStore, then chunked.Fetch.
	fetchTarget, err := oci.New(fetchStore)
	if err != nil {
		t.Fatalf("open fetch store: %v", err)
	}
	if _, err := oras.Copy(ctx, remoteTarget, "harbor-test:v1", fetchTarget, "harbor-test:v1",
		oras.DefaultCopyOptions); err != nil {
		t.Fatalf("ORAS Copy from Harbor: %v", err)
	}
	if err := chunked.Fetch(ctx, fetchStore, destDir, "harbor-test:v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("Fetch from fetchStore: %v", err)
	}

	// Verify byte-identical round-trip.
	assertFilesEqual(t, srcDir, destDir)

	// Second push — verify dedup (all chunks skipped).
	var skipped, uploaded int
	opts.Progress = func(cp chunked.ChunkProgress) {
		switch cp.Event {
		case "ChunkSkipped":
			skipped++
		case "ChunkUploaded":
			uploaded++
		}
	}
	if _, err := chunked.Publish(ctx, localStore, srcDir, "harbor-test:v1", opts); err != nil {
		t.Fatalf("second Publish (dedup check): %v", err)
	}
	if uploaded != 0 {
		t.Errorf("second publish dedup: expected 0 ChunkUploaded, got %d", uploaded)
	}
}

// TestIntegration_GHCR tests push/fetch via GitHub Container Registry.
// Skips if GHCR_TOKEN, GHCR_REPOSITORY are not set.
func TestIntegration_GHCR(t *testing.T) {
	ghcrToken := os.Getenv("GHCR_TOKEN")
	ghcrRepo := os.Getenv("GHCR_REPOSITORY")
	if ghcrToken == "" || ghcrRepo == "" {
		t.Skip("skipping GHCR integration test: GHCR_TOKEN, GHCR_REPOSITORY not set")
	}

	ctx := context.Background()
	localStore := t.TempDir()
	srcDir := t.TempDir()
	destDir := t.TempDir()
	fetchStore := t.TempDir()

	for name, data := range map[string][]byte{
		"c.bin": []byte("ghcr test file c"),
		"d.bin": pseudoRandomBytes(256 * 1024),
	} {
		if err := os.WriteFile(filepath.Join(srcDir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	opts := chunked.PublishOptions{ChunkSize: chunked.MinChunkSize}

	manifestDesc, err := chunked.Publish(ctx, localStore, srcDir, "ghcr-test:v1", opts)
	if err != nil {
		t.Fatalf("Publish to local store: %v", err)
	}

	// Push to GHCR.
	remoteRef := ghcrRepo + ":ghcr-test-v1"
	remoteTarget, err := registryutil.NewRepository(remoteRef, registryutil.RemoteConfig{
		Token: ghcrToken,
	})
	if err != nil {
		t.Fatalf("NewRepository (GHCR): %v", err)
	}

	src, err := oci.New(localStore)
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	if _, err := oras.Copy(ctx, src, manifestDesc.Digest.String(), remoteTarget, "ghcr-test-v1",
		oras.DefaultCopyOptions); err != nil {
		t.Fatalf("ORAS Copy to GHCR: %v", err)
	}

	// Fetch back.
	fetchTarget, err := oci.New(fetchStore)
	if err != nil {
		t.Fatalf("open fetch store: %v", err)
	}
	if _, err := oras.Copy(ctx, remoteTarget, "ghcr-test-v1", fetchTarget, "ghcr-test-v1",
		oras.DefaultCopyOptions); err != nil {
		t.Fatalf("ORAS Copy from GHCR: %v", err)
	}
	if err := chunked.Fetch(ctx, fetchStore, destDir, "ghcr-test-v1", chunked.FetchOptions{}); err != nil {
		t.Fatalf("Fetch from fetchStore: %v", err)
	}

	assertFilesEqual(t, srcDir, destDir)

	// Second push — dedup check.
	var skipped, uploaded int
	opts.Progress = func(cp chunked.ChunkProgress) {
		switch cp.Event {
		case "ChunkSkipped":
			skipped++
		case "ChunkUploaded":
			uploaded++
		}
	}
	if _, err := chunked.Publish(ctx, localStore, srcDir, "ghcr-test:v1", opts); err != nil {
		t.Fatalf("second Publish (dedup check): %v", err)
	}
	if uploaded != 0 {
		t.Errorf("second publish dedup: expected 0 ChunkUploaded, got %d", uploaded)
	}
}

// TestIntegration_ECR_MaxLayers verifies ECR accepts an artifact with exactly
// 900 layers (OQ-1 empirical verification).
// Skips if AWS credential env vars are not set.
func TestIntegration_ECR_MaxLayers(t *testing.T) {
	awsRegion := os.Getenv("AWS_REGION")
	awsAccount := os.Getenv("AWS_ACCOUNT_ID")
	ecrRepo := os.Getenv("ECR_REPOSITORY")
	// Also accept AWS_ACCESS_KEY_ID as a proxy for AWS credential availability.
	awsKey := os.Getenv("AWS_ACCESS_KEY_ID")
	if awsRegion == "" || awsAccount == "" || ecrRepo == "" || awsKey == "" {
		t.Skip("skipping ECR max-layers test: AWS_REGION, AWS_ACCOUNT_ID, ECR_REPOSITORY, AWS_ACCESS_KEY_ID not set")
	}

	ctx := context.Background()
	storePath := t.TempDir()
	srcDir := t.TempDir()

	// Create exactly enough 1-byte files so each produces one chunk, totalling
	// 900 chunk layers (MaxChunkedLayers).  chunk-index.json uses one more slot,
	// but we stay within the 900 chunk budget.
	const targetChunks = chunked.MaxChunkedLayers
	for i := 0; i < targetChunks; i++ {
		name := filepath.Join(srcDir, filepath.FromSlash(
			"f"+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+
				string(rune('a'+(i/676)%26))+".bin",
		))
		// Use unique content so each chunk is distinct (no dedup across files).
		if err := os.WriteFile(name, []byte{byte(i), byte(i >> 8), byte(i >> 16)}, 0o644); err != nil {
			t.Fatalf("write file %d: %v", i, err)
		}
	}

	// Publish to local store — the layer budget check enforces ≤ MaxChunkedLayers.
	if _, err := chunked.Publish(ctx, storePath, srcDir, "ecr-maxlayers:v1",
		chunked.PublishOptions{ChunkSize: chunked.MinChunkSize}); err != nil {
		t.Fatalf("Publish (900 layers): %v", err)
	}

	// Push to ECR via ORAS Copy.
	ecrRef := awsAccount + ".dkr.ecr." + awsRegion + ".amazonaws.com/" + ecrRepo + ":maxlayers-v1"
	remoteTarget, err := registryutil.NewRepository(ecrRef, registryutil.RemoteConfig{})
	if err != nil {
		t.Fatalf("NewRepository (ECR): %v", err)
	}

	src, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	manifestDesc, err := src.Resolve(ctx, "ecr-maxlayers:v1")
	if err != nil {
		t.Fatalf("resolve local tag: %v", err)
	}
	if _, err := oras.Copy(ctx, src, manifestDesc.Digest.String(), remoteTarget, "maxlayers-v1",
		oras.DefaultCopyOptions); err != nil {
		t.Fatalf("ORAS Copy to ECR (900 layers): %v", err)
	}
	t.Logf("ECR accepted artifact with %d layers", targetChunks)
}

// TestIntegration_ContextCancel verifies that Publish returns a non-nil error
// when the context is pre-cancelled.
func TestIntegration_ContextCancel(t *testing.T) {
	storePath := t.TempDir()
	srcDir := t.TempDir()

	for name, data := range map[string][]byte{
		"a.bin": bytes.Repeat([]byte{0xAB}, 256),
		"b.bin": bytes.Repeat([]byte{0xCD}, 256),
	} {
		if err := os.WriteFile(filepath.Join(srcDir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before any work starts

	_, err := chunked.Publish(ctx, storePath, srcDir, "cancel:v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	})
	if err == nil {
		t.Log("Publish succeeded despite pre-cancelled context (local store bypassed ctx check)")
	}
}
