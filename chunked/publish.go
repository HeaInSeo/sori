package chunked

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

// PublishOptions controls a chunked CAS publish operation.
type PublishOptions struct {
	// ChunkSize is the nominal size of each chunk in bytes.
	// Must be in [MinChunkSize, MaxChunkSize].  Defaults to DefaultChunkSize.
	ChunkSize int64

	// DatasetMetadata is the serialised dataset-metadata.json blob.
	// If non-nil, pushed as a dedicated OCI layer (§10-2).
	DatasetMetadata []byte

	// ConfigBlob is the caller-supplied configblob.json content.
	// If non-nil, pushed as a dedicated OCI layer (D-7).
	ConfigBlob []byte

	// Progress receives per-chunk progress events.  Pass nil to suppress.
	Progress ProgressFunc
}

// Publisher holds resolved state for a single chunked CAS publish operation.
type Publisher struct {
	opts  PublishOptions
	store *oci.Store
}

// Publish packages srcDir as a chunked CAS artifact in storePath and tags it
// as volName.  Returns the manifest descriptor of the pushed artifact.
func Publish(
	ctx context.Context,
	storePath, srcDir, volName string,
	opts PublishOptions,
) (ocispec.Descriptor, error) {
	if opts.ChunkSize == 0 {
		opts.ChunkSize = DefaultChunkSize
	}
	if err := ValidateChunkSize(opts.ChunkSize); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("chunked.Publish: %w", err)
	}

	store, err := oci.New(storePath)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("chunked.Publish: init OCI store: %w", err)
	}

	p := &Publisher{opts: opts, store: store}
	return p.publish(ctx, srcDir, volName)
}

func (p *Publisher) emit(cp ChunkProgress) {
	if p.opts.Progress != nil {
		p.opts.Progress(cp)
	}
}

// publish is the internal entry point after opts validation.
func (p *Publisher) publish(ctx context.Context, srcDir, volName string) (ocispec.Descriptor, error) {
	const caller = "chunked.Publish"

	// Step 1 + preflight: walk, validate, estimate chunk count.
	files, err := walkAndValidate(srcDir)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: walk: %w", caller, err)
	}

	sizes := make([]int64, len(files))
	for i, f := range files {
		sizes[i] = f.size
	}
	mlc := MetadataLayerCount(
		p.opts.DatasetMetadata != nil,
		p.opts.ConfigBlob != nil,
	)
	estimated := EstimatedChunkCount(sizes, p.opts.ChunkSize)
	if estimated > int64(MaxChunkedLayers-mlc) {
		return ocispec.Descriptor{}, fmt.Errorf(
			"%s: dataset would produce %d chunk layers, exceeding MaxChunkedLayers budget %d: %w",
			caller, estimated, MaxChunkedLayers-mlc, ErrPathValidation,
		)
	}

	// Step 3: hash + push each chunk.
	idx := ChunkIndex{
		SchemaVersion: SchemaVersionChunkIndex,
		ChunkSize:     p.opts.ChunkSize,
		Files:         make([]ChunkIndexFile, 0, len(files)),
	}

	chunkLayers := make([]ocispec.Descriptor, 0, int(estimated))

	for _, sf := range files {
		idxFile, fileChunkDescs, err := p.processFile(ctx, caller, sf)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		idx.Files = append(idx.Files, idxFile)
		chunkLayers = append(chunkLayers, fileChunkDescs...)
	}

	// Step 4: push chunk-index.json.
	chunkIndexDesc, err := p.pushJSON(ctx, caller, MediaTypeChunkIndex, idx)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	// Step 5: push dataset-metadata (optional).
	layers := make([]ocispec.Descriptor, 0, mlc+len(chunkLayers))
	layers = append(layers, chunkIndexDesc)

	if p.opts.DatasetMetadata != nil {
		metaDesc, err := p.pushBytes(ctx, caller, MediaTypeDatasetMeta, p.opts.DatasetMetadata)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		layers = append(layers, metaDesc)
	}

	// Step 6: push original configblob (optional).
	if p.opts.ConfigBlob != nil {
		cfgBlobDesc, err := p.pushBytes(ctx, caller, MediaTypeConfigBlob, p.opts.ConfigBlob)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		layers = append(layers, cfgBlobDesc)
	}

	// Append chunk layers after all metadata layers.
	layers = append(layers, chunkLayers...)

	// Step 7: push OCI config descriptor blob.
	ociCfg := OCIConfigDescriptor{
		SchemaVersion:  SchemaVersionConfig,
		ArtifactFormat: ArtifactFormatV1,
	}
	ociCfgBytes, err := json.Marshal(ociCfg)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: marshal OCI config: %w", caller, err)
	}
	ociCfgDesc, err := p.pushBytes(ctx, caller, MediaTypeConfig, ociCfgBytes)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	// Step 8+9: build and push OCI manifest.
	manifestDesc, err := oras.PackManifest(ctx, p.store, oras.PackManifestVersion1_1,
		ocispec.MediaTypeImageManifest,
		oras.PackManifestOptions{
			ConfigDescriptor: &ociCfgDesc,
			Layers:           layers,
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationCreated: time.Now().UTC().Format(time.RFC3339),
			},
		},
	)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: pack manifest: %w", caller, err)
	}
	if err := p.store.Tag(ctx, manifestDesc, volName); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: tag manifest %q: %w", caller, volName, err)
	}
	return manifestDesc, nil
}

// sourceFile holds resolved metadata for one file discovered during walk.
type sourceFile struct {
	absPath string
	relPath string
	size    int64
	mode    fs.FileMode
}

// walkAndValidate walks srcDir and returns sorted regular files, rejecting
// symlinks and special files per D-10.
func walkAndValidate(srcDir string) ([]sourceFile, error) {
	var files []sourceFile
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		// Reject non-regular files (symlinks, devices, pipes, sockets).
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%w: non-regular file rejected: %s", ErrPathValidation, path)
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err := ValidatePath(rel); err != nil {
			return err
		}
		files = append(files, sourceFile{
			absPath: path,
			relPath: rel,
			size:    fi.Size(),
			mode:    fi.Mode(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].relPath < files[j].relPath
	})
	return files, nil
}

// processFile hashes and pushes all chunks for one source file using the
// two-pass io.SectionReader approach (§4):
//   - Pass 1: hash the chunk bytes (small read buffer, no heap allocation)
//   - Dedup check: skip if blob already exists
//   - Pass 2: push via SectionReader seeking back to offset 0
func (p *Publisher) processFile(
	ctx context.Context,
	caller string,
	sf sourceFile,
) (ChunkIndexFile, []ocispec.Descriptor, error) {
	f, err := os.Open(sf.absPath)
	if err != nil {
		return ChunkIndexFile{}, nil, fmt.Errorf("%s: open %s: %w", caller, sf.absPath, err)
	}
	defer func() {
		if cErr := f.Close(); cErr != nil {
			// best-effort; file is read-only
			_ = cErr
		}
	}()

	// Compute whole-file digest for the ChunkIndexFile.digest field.
	fileHash := digest.Canonical.Hash()
	if _, err := io.Copy(fileHash, f); err != nil {
		return ChunkIndexFile{}, nil, fmt.Errorf("%s: hash file %s: %w", caller, sf.absPath, err)
	}
	fileDigest := digest.NewDigest(digest.Canonical, fileHash).String()

	idxFile := ChunkIndexFile{
		Path:   sf.relPath,
		Mode:   uint32(sf.mode.Perm()),
		Size:   sf.size,
		Digest: fileDigest,
	}

	var chunkDescs []ocispec.Descriptor
	chunkIdx := 0
	for offset := int64(0); offset < sf.size || (sf.size == 0 && chunkIdx == 0); offset += p.opts.ChunkSize {
		chunkSize := p.opts.ChunkSize
		if remaining := sf.size - offset; remaining < chunkSize {
			chunkSize = remaining
		}

		desc, entry, err := p.pushChunk(ctx, caller, f, sf.absPath, offset, chunkSize, chunkIdx)
		if err != nil {
			return ChunkIndexFile{}, nil, err
		}
		idxFile.Chunks = append(idxFile.Chunks, entry)
		chunkDescs = append(chunkDescs, desc)
		chunkIdx++

		if sf.size == 0 {
			break // empty file: one zero-length chunk entry
		}
	}

	return idxFile, chunkDescs, nil
}

// pushChunk implements the two-pass SectionReader approach for a single chunk.
func (p *Publisher) pushChunk(
	ctx context.Context,
	caller string,
	f *os.File,
	filePath string,
	offset, chunkSize int64,
	chunkIdx int,
) (ocispec.Descriptor, ChunkEntry, error) {
	sr := io.NewSectionReader(f, offset, chunkSize)

	// Pass 1: hash.
	h := digest.Canonical.Hash()
	if _, err := io.Copy(h, sr); err != nil {
		return ocispec.Descriptor{}, ChunkEntry{}, fmt.Errorf(
			"%s: hash chunk %d of %s: %w", caller, chunkIdx, filePath, err)
	}
	chunkDigest := digest.NewDigest(digest.Canonical, h)

	desc := ocispec.Descriptor{
		MediaType: MediaTypeChunk,
		Digest:    chunkDigest,
		Size:      chunkSize,
	}

	entry := ChunkEntry{
		Offset: offset,
		Size:   chunkSize,
		Digest: chunkDigest.String(),
	}

	// Dedup check: skip push if blob already exists (D-5).
	exists, err := p.store.Exists(ctx, desc)
	if err != nil {
		return ocispec.Descriptor{}, ChunkEntry{}, fmt.Errorf(
			"%s: exists check chunk %d of %s: %w", caller, chunkIdx, filePath, err)
	}
	if exists {
		p.emit(ChunkProgress{
			Event:      "ChunkSkipped",
			File:       filePath,
			ChunkIndex: chunkIdx,
			Digest:     chunkDigest.String(),
			Bytes:      chunkSize,
		})
		return desc, entry, nil
	}

	// Pass 2: push via SectionReader (seeks back to offset 0 within the section).
	sr2 := io.NewSectionReader(f, offset, chunkSize)
	start := time.Now()
	if err := p.store.Push(ctx, desc, sr2); err != nil {
		return ocispec.Descriptor{}, ChunkEntry{}, fmt.Errorf(
			"%s: push chunk %d of %s: %w", caller, chunkIdx, filePath, err)
	}
	p.emit(ChunkProgress{
		Event:      "ChunkUploaded",
		File:       filePath,
		ChunkIndex: chunkIdx,
		Digest:     chunkDigest.String(),
		Bytes:      chunkSize,
		DurationMs: time.Since(start).Milliseconds(),
	})

	return desc, entry, nil
}

// pushJSON marshals v and pushes it as a blob with the given mediaType.
func (p *Publisher) pushJSON(ctx context.Context, caller, mediaType string, v any) (ocispec.Descriptor, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: marshal %s: %w", caller, mediaType, err)
	}
	return p.pushBytes(ctx, caller, mediaType, data)
}

// pushBytes pushes raw bytes as a blob and returns its descriptor.
func (p *Publisher) pushBytes(ctx context.Context, caller, mediaType string, data []byte) (ocispec.Descriptor, error) {
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	exists, err := p.store.Exists(ctx, desc)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: exists check %s: %w", caller, mediaType, err)
	}
	if exists {
		return desc, nil
	}
	if err := p.store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: push %s: %w", caller, mediaType, err)
	}
	return desc, nil
}
