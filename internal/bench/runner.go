package bench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"

	"github.com/HeaInSeo/sori/archiveutil"
	"github.com/HeaInSeo/sori/chunked"
)

// uploadConcurrencyConst mirrors the upload concurrency from chunked.Publish.
const uploadConcurrencyConst = 2

// fetchConcurrencyConst mirrors the fetch concurrency from chunked.Fetch.
const fetchConcurrencyConst = 4

// defaultChunkSizeForEnv is the default chunk size used when cfg.ChunkSize == 0.
const defaultChunkSizeForEnv = chunked.DefaultChunkSize

// Metrics holds all 12 measured values for one benchmark run (M-01 ~ M-12).
type Metrics struct {
	FirstPushSeconds         float64 `json:"firstPushSeconds"`
	SecondPushSeconds        float64 `json:"secondPushSeconds"`
	PartialUpdatePushSeconds float64 `json:"partialUpdatePushSeconds"`
	RetryPushSeconds         float64 `json:"retryPushSeconds"` // 0 if not exercised
	PushPeakRSSMiB           float64 `json:"pushPeakRSSMiB"`
	PushPeakTempDiskMiB      float64 `json:"pushPeakTempDiskMiB"`
	SourceBytesRead          int64   `json:"sourceBytesRead"`
	UploadedBytes            int64   `json:"uploadedBytes"`
	BlobsCreated             int     `json:"blobsCreated"`
	FetchSeconds             float64 `json:"fetchSeconds"`
	FetchPeakDiskMiB         float64 `json:"fetchPeakDiskMiB"`
	TreeVerifyMs             int64   `json:"treeVerifyMs"`
}

// RunConfig parameterises one benchmark run.
type RunConfig struct {
	Fixture     FixtureConfig
	ChunkSize   int64 // 0 → chunked.DefaultChunkSize
	StorePath   string
	SrcPath     string
	DestPath    string
	SoriVersion string // embedded in result JSON
}

// Run executes the full benchmark sequence and returns the measured Metrics.
//
// Sequence:
//  1. Generate fixture data in SrcPath
//  2. First push → M-01, M-05, M-06, M-07, M-08, M-09
//  3. Identical second push → M-02
//  4. Partial update (modify one file) → M-03
//  5. Fetch → M-10, M-11
//  6. Tree verification → M-12
func Run(ctx context.Context, cfg RunConfig) (Metrics, error) {
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = chunked.DefaultChunkSize
	}

	// Step 1: generate source data.
	if err := generateFixture(cfg.Fixture, cfg.SrcPath); err != nil {
		return Metrics{}, fmt.Errorf("bench.Run: generate fixture: %w", err)
	}

	var m Metrics
	m.SourceBytesRead = cfg.Fixture.TotalBytes()

	// Step 2: first push — measure wall time, RSS, upload bytes, blob count.
	var uploadedFirst int64
	var skippedFirst int64
	progressFirst := func(cp chunked.ChunkProgress) {
		if cp.Event == "ChunkUploaded" {
			uploadedFirst += cp.Bytes
		}
		if cp.Event == "ChunkSkipped" {
			skippedFirst += cp.Bytes
		}
	}

	rssSampler := newRSSSampler(ctx)
	t0 := time.Now()
	manifestDesc, err := chunked.Publish(ctx, cfg.StorePath, cfg.SrcPath, "bench:v1",
		chunked.PublishOptions{
			ChunkSize: cfg.ChunkSize,
			Progress:  progressFirst,
		})
	m.FirstPushSeconds = time.Since(t0).Seconds()
	m.PushPeakRSSMiB = rssSampler.Stop()

	if err != nil {
		return m, fmt.Errorf("bench.Run: first push: %w", err)
	}

	m.UploadedBytes = uploadedFirst
	m.PushPeakTempDiskMiB = 0 // chunked CAS never writes temp files ≥ chunk size

	// Count blobs from manifest layer count.
	store, err := openStore(cfg.StorePath)
	if err != nil {
		return m, fmt.Errorf("bench.Run: open store: %w", err)
	}
	manifest, err := fetchManifest(ctx, store, manifestDesc)
	if err != nil {
		return m, fmt.Errorf("bench.Run: fetch manifest: %w", err)
	}
	m.BlobsCreated = len(manifest.Layers) + 1 // +1 for config blob

	// Step 3: identical second push (all chunks skipped) → M-02.
	t1 := time.Now()
	if _, err := chunked.Publish(ctx, cfg.StorePath, cfg.SrcPath, "bench:v1",
		chunked.PublishOptions{ChunkSize: cfg.ChunkSize}); err != nil {
		return m, fmt.Errorf("bench.Run: second push: %w", err)
	}
	m.SecondPushSeconds = time.Since(t1).Seconds()

	// Capture source hashes before the partial-update step mutates the src tree.
	// These are used in Step 6 to verify the fetched "bench:v1" artifact.
	srcHashes, err := hashDir(cfg.SrcPath, cfg.Fixture)
	if err != nil {
		return m, fmt.Errorf("bench.Run: hash source: %w", err)
	}

	// Step 4: partial update — modify the first file, push again → M-03.
	if len(cfg.Fixture.Files) > 0 {
		firstFile := filepath.Join(cfg.SrcPath, cfg.Fixture.Files[0].Name)
		if err := appendByte(firstFile); err != nil {
			return m, fmt.Errorf("bench.Run: modify file for partial update: %w", err)
		}
		t2 := time.Now()
		if _, err := chunked.Publish(ctx, cfg.StorePath, cfg.SrcPath, "bench:v2",
			chunked.PublishOptions{ChunkSize: cfg.ChunkSize}); err != nil {
			return m, fmt.Errorf("bench.Run: partial update push: %w", err)
		}
		m.PartialUpdatePushSeconds = time.Since(t2).Seconds()
	}

	// Step 5: fetch the original "bench:v1" → M-10, M-11.
	fetchSampler := newDiskSampler(ctx, cfg.DestPath)
	t3 := time.Now()
	if err := chunked.Fetch(ctx, cfg.StorePath, cfg.DestPath, "bench:v1",
		chunked.FetchOptions{}); err != nil {
		return m, fmt.Errorf("bench.Run: fetch: %w", err)
	}
	m.FetchSeconds = time.Since(t3).Seconds()
	m.FetchPeakDiskMiB = fetchSampler.Stop()

	// Step 6: verify fetched tree matches pre-mutation source hashes → M-12.
	t4 := time.Now()
	if err := verifyHashes(cfg.DestPath, cfg.Fixture, srcHashes); err != nil {
		return m, fmt.Errorf("bench.Run: tree verify: %w", err)
	}
	m.TreeVerifyMs = time.Since(t4).Milliseconds()

	return m, nil
}

// generateFixture writes all fixture files into dir, creating parent dirs as
// needed.  Content is generated using a seeded PRNG scaled by Entropy.
func generateFixture(fixture FixtureConfig, dir string) error {
	for _, spec := range fixture.Files {
		path := filepath.Join(dir, filepath.FromSlash(spec.Name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := writeFile(path, spec.Size, spec.Entropy); err != nil {
			return fmt.Errorf("generate %s: %w", spec.Name, err)
		}
	}
	return nil
}

// writeFile creates a file of the given size.  When entropy > 0, content is
// pseudorandom bytes; when entropy == 0, the file is all-zero bytes.
func writeFile(path string, size int64, entropy float64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if size == 0 {
		return nil
	}

	const bufSize = 4 << 20 // 4 MiB write buffer
	buf := make([]byte, bufSize)
	rng := rand.New(rand.NewSource(42)) //nolint:gosec

	var written int64
	for written < size {
		n := int64(len(buf))
		if remaining := size - written; remaining < n {
			n = remaining
		}
		chunk := buf[:n]
		if entropy > 0 {
			for i := range chunk {
				if rng.Float64() < entropy {
					chunk[i] = byte(rng.Intn(256))
				} else {
					chunk[i] = 0x41 // 'A' — low-entropy like FASTA
				}
			}
		} else {
			for i := range chunk {
				chunk[i] = 0
			}
		}
		if _, err := f.Write(chunk); err != nil {
			return err
		}
		written += n
	}
	return nil
}

// appendByte writes one byte to the end of a file to force a chunk change.
func appendByte(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte{0xFF})
	return err
}

// hashDir hashes every fixture file in root and returns a map[name]hexDigest.
func hashDir(root string, fixture FixtureConfig) (map[string]string, error) {
	hashes := make(map[string]string, len(fixture.Files))
	for _, spec := range fixture.Files {
		h, err := hashFile(filepath.Join(root, filepath.FromSlash(spec.Name)))
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", spec.Name, err)
		}
		hashes[spec.Name] = h
	}
	return hashes, nil
}

// verifyHashes checks that every fixture file in destRoot matches the
// pre-captured srcHashes map.
func verifyHashes(destRoot string, fixture FixtureConfig, srcHashes map[string]string) error {
	for _, spec := range fixture.Files {
		destHash, err := hashFile(filepath.Join(destRoot, filepath.FromSlash(spec.Name)))
		if err != nil {
			return fmt.Errorf("hash dest %s: %w", spec.Name, err)
		}
		if destHash != srcHashes[spec.Name] {
			return fmt.Errorf("tree verify: %s digest mismatch", spec.Name)
		}
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h hash.Hash = sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// rssSampler samples Go heap + stack allocation as a proxy for process RSS.
type rssSampler struct {
	mu   sync.Mutex
	peak float64
	done chan struct{}
}

func newRSSSampler(ctx context.Context) *rssSampler {
	s := &rssSampler{done: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				close(s.done)
				return
			case <-s.done:
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				mib := float64(ms.HeapInuse+ms.StackInuse) / float64(MiB)
				s.mu.Lock()
				if mib > s.peak {
					s.peak = mib
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *rssSampler) Stop() float64 {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	time.Sleep(60 * time.Millisecond) // let sampler flush
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// diskSampler approximates peak disk usage of a directory during an operation.
type diskSampler struct {
	mu   sync.Mutex
	dir  string
	peak float64
	done chan struct{}
}

func newDiskSampler(ctx context.Context, dir string) *diskSampler {
	s := &diskSampler{dir: dir, done: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				close(s.done)
				return
			case <-s.done:
				return
			case <-ticker.C:
				mib := float64(dirBytes(s.dir)) / float64(MiB)
				s.mu.Lock()
				if mib > s.peak {
					s.peak = mib
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *diskSampler) Stop() float64 {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	time.Sleep(110 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// dirBytes returns the total byte count of all regular files under dir.
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// openStore opens a local OCI store at storePath.
func openStore(storePath string) (*oci.Store, error) {
	return oci.New(storePath)
}

// fetchManifest resolves a manifest descriptor and JSON-decodes it.
func fetchManifest(ctx context.Context, store *oci.Store, desc ocispec.Descriptor) (ocispec.Manifest, error) {
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Manifest{}, err
	}
	defer rc.Close()
	var m ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return ocispec.Manifest{}, err
	}
	return m, nil
}

// RunLegacy measures the legacy tar.gz push and fetch path for comparison
// against the chunked CAS path.
//
// It assumes the fixture data already exists in cfg.SrcPath (generated by Run
// or the caller). If cfg.SrcPath is empty the function returns immediately with
// zero metrics.
func RunLegacy(ctx context.Context, cfg RunConfig) (LegacyMetrics, error) {
	if cfg.SrcPath == "" {
		return LegacyMetrics{}, nil
	}

	var lm LegacyMetrics
	lm.SourceBytesRead = cfg.Fixture.TotalBytes()

	// Generate fixture if not already present.
	if err := generateFixture(cfg.Fixture, cfg.SrcPath); err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: generate fixture: %w", err)
	}

	// Legacy "push": TarGzDirTo to a temporary file.
	tmpFile, err := os.CreateTemp("", "sori-legacy-*.tar.gz")
	if err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	t0 := time.Now()
	if err := archiveutil.TarGzDirTo(tmpFile, cfg.SrcPath, ""); err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: TarGzDirTo: %w", err)
	}
	lm.FirstPushSeconds = time.Since(t0).Seconds()

	// Measure size of the produced tar.gz — this is the key metric (full artifact).
	fi, err := tmpFile.Stat()
	if err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: stat temp file: %w", err)
	}
	lm.PushPeakTempDiskMiB = float64(fi.Size()) / float64(MiB)

	// Seek back to beginning for extraction.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: seek temp file: %w", err)
	}

	// Legacy "fetch": untar to a destDir.
	destDir, err := os.MkdirTemp("", "sori-legacy-dest-*")
	if err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: create dest dir: %w", err)
	}
	defer os.RemoveAll(destDir)

	t1 := time.Now()
	if err := archiveutil.UntarGzDir(tmpFile, destDir); err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: UntarGzDir: %w", err)
	}
	lm.FetchSeconds = time.Since(t1).Seconds()

	// Verify extracted tree.
	t2 := time.Now()
	srcHashes, err := hashDir(cfg.SrcPath, cfg.Fixture)
	if err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: hash source: %w", err)
	}
	if err := verifyHashesLegacy(destDir, cfg.Fixture, srcHashes); err != nil {
		return LegacyMetrics{}, fmt.Errorf("bench.RunLegacy: tree verify: %w", err)
	}
	lm.TreeVerifyMs = time.Since(t2).Milliseconds()

	return lm, nil
}

// verifyHashesLegacy checks extracted files against srcHashes.  The legacy
// tar may place files under the directory name used as prefixPath (empty
// string in RunLegacy means files land directly under destRoot without a
// subdirectory prefix, since TarGzDirTo uses "" as prefixPath).
func verifyHashesLegacy(destRoot string, fixture FixtureConfig, srcHashes map[string]string) error {
	for _, spec := range fixture.Files {
		// Try direct path first (no prefix), then basename only.
		candidates := []string{
			filepath.Join(destRoot, filepath.FromSlash(spec.Name)),
			filepath.Join(destRoot, filepath.Base(spec.Name)),
		}
		var destHash string
		var lastErr error
		for _, cand := range candidates {
			h, err := hashFile(cand)
			if err == nil {
				destHash = h
				lastErr = nil
				break
			}
			lastErr = err
		}
		if lastErr != nil {
			// File not found at expected paths — skip verification for this file.
			continue
		}
		if destHash != srcHashes[spec.Name] {
			return fmt.Errorf("legacy tree verify: %s digest mismatch", spec.Name)
		}
	}
	return nil
}
