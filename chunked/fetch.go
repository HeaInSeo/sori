package chunked

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2/content/oci"
)

// fetchConcurrency is the maximum number of concurrent file reconstruction
// goroutines during Fetch.
const fetchConcurrency = 4

// FetchOptions controls a chunked CAS fetch operation.
type FetchOptions struct {
	// Progress receives per-chunk progress events.  Pass nil to suppress.
	Progress ProgressFunc
}

// Fetcher holds resolved state for a single chunked CAS fetch operation.
type Fetcher struct {
	opts  FetchOptions
	store *oci.Store
}

func (f *Fetcher) emit(cp ChunkProgress) {
	if f.opts.Progress != nil {
		f.opts.Progress(cp)
	}
}

// Fetch reconstructs the files of a chunked CAS artifact from storePath into
// destRoot, resolving the artifact by volName tag.
func Fetch(ctx context.Context, storePath, destRoot, volName string, opts FetchOptions) error {
	store, err := oci.New(storePath)
	if err != nil {
		return fmt.Errorf("chunked.Fetch: init OCI store: %w", err)
	}

	f := &Fetcher{opts: opts, store: store}
	return f.fetch(ctx, destRoot, volName)
}

// fetch is the internal entry point after store initialisation.
func (f *Fetcher) fetch(ctx context.Context, destRoot, volName string) error {
	const caller = "chunked.Fetch"

	// Step 1: resolve tag → manifest descriptor → manifest bytes.
	manifestDesc, err := f.store.Resolve(ctx, volName)
	if err != nil {
		return fmt.Errorf("%s: resolve tag %q: %w", caller, volName, err)
	}

	manifestRC, err := f.store.Fetch(ctx, manifestDesc)
	if err != nil {
		return fmt.Errorf("%s: fetch manifest: %w", caller, err)
	}
	defer manifestRC.Close()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(manifestRC).Decode(&manifest); err != nil {
		return fmt.Errorf("%s: decode manifest: %w", caller, err)
	}

	// Step 2: dual-path detection (D-13).
	switch manifest.Config.MediaType {
	case MediaTypeLegacyConfig:
		return fmt.Errorf("%s: legacy format not supported by chunked fetcher: %w",
			caller, ErrValidation)
	case MediaTypeConfig:
		// chunked CAS path — continue.
	default:
		return fmt.Errorf("%s: unknown config mediaType %q: %w",
			caller, manifest.Config.MediaType, ErrValidation)
	}

	// Step 3: locate chunk-index layer by mediaType (not position).
	var chunkIndexDesc *ocispec.Descriptor
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType == MediaTypeChunkIndex {
			chunkIndexDesc = &manifest.Layers[i]
			break
		}
	}
	if chunkIndexDesc == nil {
		return fmt.Errorf("%s: no chunk-index layer found in manifest: %w", caller, ErrValidation)
	}

	// Step 4: fetch and parse chunk-index.json.
	idxRC, err := f.store.Fetch(ctx, *chunkIndexDesc)
	if err != nil {
		return fmt.Errorf("%s: fetch chunk-index: %w", caller, err)
	}
	defer idxRC.Close()

	var idx ChunkIndex
	if err := json.NewDecoder(idxRC).Decode(&idx); err != nil {
		return fmt.Errorf("%s: decode chunk-index: %w", caller, err)
	}
	if idx.SchemaVersion != SchemaVersionChunkIndex {
		return fmt.Errorf("%s: chunk-index schemaVersion %q, want %q: %w",
			caller, idx.SchemaVersion, SchemaVersionChunkIndex, ErrValidation)
	}
	if err := ValidatePaths(&idx); err != nil {
		return fmt.Errorf("%s: %w", caller, err)
	}

	// Step 5: create destRoot if needed.
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return fmt.Errorf("%s: create destRoot: %w", caller, err)
	}

	// Step 6: reconstruct files — worker pool with fetchConcurrency=4.
	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, fetchConcurrency)

	for _, idxFile := range idx.Files {
		idxFile := idxFile // capture loop variable
		sem <- struct{}{}
		g.Go(func() error {
			defer func() { <-sem }()
			return f.reconstructFile(gctx, destRoot, idxFile)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Step 7: write dataset-metadata (optional).
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeDatasetMeta {
			data, err := f.fetchBlobBytes(ctx, layer)
			if err != nil {
				return fmt.Errorf("%s: fetch dataset-metadata: %w", caller, err)
			}
			soriDir := filepath.Join(destRoot, ".sori")
			if err := os.MkdirAll(soriDir, 0o755); err != nil {
				return fmt.Errorf("%s: create .sori dir: %w", caller, err)
			}
			metaPath := filepath.Join(soriDir, "dataset-metadata.json")
			if err := os.WriteFile(metaPath, data, 0o644); err != nil {
				return fmt.Errorf("%s: write dataset-metadata: %w", caller, err)
			}
			break
		}
	}

	// Step 8: write configblob (optional).
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeConfigBlob {
			data, err := f.fetchBlobBytes(ctx, layer)
			if err != nil {
				return fmt.Errorf("%s: fetch configblob: %w", caller, err)
			}
			cfgPath := filepath.Join(destRoot, "configblob.json")
			if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
				return fmt.Errorf("%s: write configblob: %w", caller, err)
			}
			break
		}
	}

	// Step 9: emit ArtifactDone.
	f.emit(ChunkProgress{Event: "ArtifactDone"})
	return nil
}

// reconstructFile writes a single file described by idxFile into destRoot.
func (f *Fetcher) reconstructFile(ctx context.Context, destRoot string, idxFile ChunkIndexFile) error {
	const caller = "chunked.Fetch"

	destPath := filepath.Join(destRoot, filepath.FromSlash(idxFile.Path))

	// Step 1: create parent directories.
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("%s: create parent dirs for %s: %w", caller, idxFile.Path, err)
	}

	// Step 2: pre-allocate file.
	file, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(idxFile.Mode))
	if err != nil {
		return fmt.Errorf("%s: create file %s: %w", caller, idxFile.Path, err)
	}
	if idxFile.Size > 0 {
		if err := file.Truncate(idxFile.Size); err != nil {
			file.Close()
			return fmt.Errorf("%s: truncate file %s: %w", caller, idxFile.Path, err)
		}
	}

	// Step 3: fetch and write each chunk.
	for i, chunk := range idxFile.Chunks {
		chunkDesc := ocispec.Descriptor{
			MediaType: MediaTypeChunk,
			Digest:    digest.Digest(chunk.Digest),
			Size:      chunk.Size,
		}

		rc, err := f.store.Fetch(ctx, chunkDesc)
		if err != nil {
			file.Close()
			return fmt.Errorf("%s: fetch chunk %d of %s: %w", caller, i, idxFile.Path, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			file.Close()
			return fmt.Errorf("%s: read chunk %d of %s: %w", caller, i, idxFile.Path, err)
		}

		// Verify chunk integrity.
		got := digest.FromBytes(data)
		if got != digest.Digest(chunk.Digest) {
			file.Close()
			return fmt.Errorf("%s: chunk %d of %s: digest mismatch got %s want %s: %w",
				caller, i, idxFile.Path, got, chunk.Digest, ErrIntegrity)
		}

		// Write at correct offset.
		if _, err := file.WriteAt(data, chunk.Offset); err != nil {
			file.Close()
			return fmt.Errorf("%s: write chunk %d of %s at offset %d: %w",
				caller, i, idxFile.Path, chunk.Offset, err)
		}

		f.emit(ChunkProgress{
			Event:      "ChunkFetched",
			File:       idxFile.Path,
			ChunkIndex: i,
			Bytes:      chunk.Size,
			Digest:     chunk.Digest,
		})
	}

	// Close before re-reading for whole-file verification.
	if err := file.Close(); err != nil {
		return fmt.Errorf("%s: close file %s: %w", caller, idxFile.Path, err)
	}

	// Step 5: verify whole-file sha256.
	rf, err := os.Open(destPath)
	if err != nil {
		return fmt.Errorf("%s: open file for verification %s: %w", caller, idxFile.Path, err)
	}
	h := digest.Canonical.Hash()
	if _, err := io.Copy(h, rf); err != nil {
		rf.Close()
		return fmt.Errorf("%s: hash file %s: %w", caller, idxFile.Path, err)
	}
	rf.Close()

	gotFileDigest := digest.NewDigest(digest.Canonical, h)
	if gotFileDigest.String() != idxFile.Digest {
		return fmt.Errorf("%s: file %s: digest mismatch got %s want %s: %w",
			caller, idxFile.Path, gotFileDigest, idxFile.Digest, ErrIntegrity)
	}

	// Step 6: emit FileDone.
	f.emit(ChunkProgress{
		Event: "FileDone",
		File:  idxFile.Path,
		Bytes: idxFile.Size,
	})
	return nil
}

// fetchBlobBytes fetches a blob by descriptor and returns all bytes.
func (f *Fetcher) fetchBlobBytes(ctx context.Context, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := f.store.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
