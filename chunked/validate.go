package chunked

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
)

// Layer budget constants (D-11).
const (
	// MaxChunkedLayers is the hard upper bound on the total number of OCI layers
	// in a chunked CAS manifest.  ECR documents a 1000-layer limit; 900 provides
	// headroom.  The effective chunk layer budget is computed dynamically by
	// MetadataLayerCount.
	MaxChunkedLayers = 900

	// DefaultChunkSize is the nominal chunk size used when none is specified (D-1).
	DefaultChunkSize int64 = 1 << 30 // 1 GiB

	// MinChunkSize is the smallest accepted chunk size (D-1).
	MinChunkSize int64 = 256 << 20 // 256 MiB

	// MaxChunkSize is the largest accepted chunk size (D-1).
	MaxChunkSize int64 = 2 << 30 // 2 GiB
)

// Schema version and artifact format strings (D-3, D-4, §10-3).
const (
	SchemaVersionChunkIndex = "sori.chunked-cas.v1"
	SchemaVersionConfig     = "sori.chunked-cas.config.v1"
	ArtifactFormatV1        = "sori.chunked-cas.v1"
	SchemaVersionMetadata   = "sori.dataset.metadata.v1"
)

// OCI media type strings for chunked CAS blobs (D-3).
const (
	MediaTypeConfig      = "application/vnd.sori.chunked-cas.config.v1+json"
	MediaTypeChunkIndex  = "application/vnd.sori.chunk-index.v1+json"
	MediaTypeDatasetMeta = "application/vnd.sori.dataset.metadata.v1+json"
	MediaTypeConfigBlob  = "application/vnd.sori.configblob.v1+json"
	MediaTypeChunk       = "application/vnd.sori.chunk.v1"

	// MediaTypeLegacyConfig is the config mediaType used by legacy tar.gz artifacts.
	// Its presence in manifest.Config.MediaType triggers the legacy fetch path (D-13).
	MediaTypeLegacyConfig = "application/vnd.oci.image.config.v1+json"
)

// ErrPathValidation is returned when a path in chunk-index.json fails D-10 rules.
var ErrPathValidation = errors.New("invalid chunk-index path")

// ErrValidation is returned when a schema version, media type, or structural
// validation check fails during fetch.
var ErrValidation = errors.New("validation error")

// ErrIntegrity is returned when a fetched chunk or file digest does not match
// the value recorded in chunk-index.json.
var ErrIntegrity = errors.New("chunk integrity failure")

// ValidatePath checks whether p is a safe relative path per D-10.
// Returns a wrapped ErrPathValidation on failure.
func ValidatePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty path", ErrPathValidation)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%w: absolute path not allowed: %s", ErrPathValidation, p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("%w: path traversal not allowed: %s", ErrPathValidation, p)
		}
		if part == "" {
			return fmt.Errorf("%w: empty path segment in: %s", ErrPathValidation, p)
		}
	}
	return nil
}

// ValidatePaths checks all paths in a ChunkIndex for D-10 compliance and
// duplicate detection.  Returns the first violation found.
func ValidatePaths(idx *ChunkIndex) error {
	seen := make(map[string]struct{}, len(idx.Files))
	for _, f := range idx.Files {
		if err := ValidatePath(f.Path); err != nil {
			return err
		}
		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("%w: duplicate path: %s", ErrPathValidation, f.Path)
		}
		seen[f.Path] = struct{}{}
	}
	return nil
}

// ValidateIndex checks the structural reconstruction contract of a chunk index.
// It verifies paths plus file/chunk sizes, contiguous offsets, and digest syntax.
func ValidateIndex(idx *ChunkIndex) error {
	if idx == nil {
		return fmt.Errorf("%w: chunk-index is required", ErrValidation)
	}
	if err := ValidateChunkSize(idx.ChunkSize); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
	if err := ValidatePaths(idx); err != nil {
		return err
	}
	for _, f := range idx.Files {
		if err := validateIndexFile(f); err != nil {
			return err
		}
	}
	return nil
}

func validateIndexFile(f ChunkIndexFile) error {
	if f.Size < 0 {
		return fmt.Errorf("%w: negative file size for %s", ErrValidation, f.Path)
	}
	if _, err := digest.Parse(f.Digest); err != nil {
		return fmt.Errorf("%w: invalid file digest for %s: %w", ErrValidation, f.Path, err)
	}
	if len(f.Chunks) == 0 {
		return fmt.Errorf("%w: file has no chunks: %s", ErrValidation, f.Path)
	}
	total, err := validateIndexChunks(f)
	if err != nil {
		return err
	}
	if total != f.Size {
		return fmt.Errorf("%w: chunk sizes for %s sum to %d, want file size %d",
			ErrValidation, f.Path, total, f.Size)
	}
	return nil
}

func validateIndexChunks(f ChunkIndexFile) (int64, error) {
	var offset int64
	for i, c := range f.Chunks {
		if c.Offset != offset {
			return 0, fmt.Errorf("%w: non-contiguous chunk %d for %s: got offset %d want %d",
				ErrValidation, i, f.Path, c.Offset, offset)
		}
		if c.Size < 0 {
			return 0, fmt.Errorf("%w: negative chunk size for %s chunk %d", ErrValidation, f.Path, i)
		}
		if _, err := digest.Parse(c.Digest); err != nil {
			return 0, fmt.Errorf("%w: invalid chunk digest for %s chunk %d: %w", ErrValidation, f.Path, i, err)
		}
		offset += c.Size
	}
	return offset, nil
}

// MetadataLayerCount returns the number of non-chunk OCI layers for a given
// combination of optional blobs (D-11).
//
//	hasDatasetMetadata: opts.DatasetMetadata != nil
//	hasConfigBlob:      opts.ConfigBlob != nil
func MetadataLayerCount(hasDatasetMetadata, hasConfigBlob bool) int {
	n := 1 // chunk-index.json is always present
	if hasDatasetMetadata {
		n++
	}
	if hasConfigBlob {
		n++
	}
	return n
}

// EstimatedChunkCount computes the total number of chunks that will be produced
// for a list of file sizes at the given chunkSize (D-11, §7-10).
//
// File boundaries are never shared between chunks: each file contributes at
// least one chunk regardless of size.
func EstimatedChunkCount(fileSizes []int64, chunkSize int64) int64 {
	var total int64
	for _, size := range fileSizes {
		if size == 0 {
			total++ // empty file still produces one (zero-length) chunk entry
			continue
		}
		n := size / chunkSize
		if size%chunkSize != 0 {
			n++
		}
		total += n
	}
	return total
}

// ValidateChunkSize returns an error if sz is outside the allowed range (D-1).
func ValidateChunkSize(sz int64) error {
	if sz < MinChunkSize || sz > MaxChunkSize {
		return fmt.Errorf("chunkSize %d is outside allowed range [%d, %d]",
			sz, MinChunkSize, MaxChunkSize)
	}
	return nil
}
