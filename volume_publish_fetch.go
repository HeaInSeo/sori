package sori

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/HeaInSeo/sori/archiveutil"
	"github.com/HeaInSeo/sori/chunked"
	"github.com/HeaInSeo/sori/registryutil"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
)

type countWriter struct{ n int64 }

func (cw *countWriter) Write(p []byte) (int, error) {
	cw.n += int64(len(p))
	return len(p), nil
}

const (
	annotationPartitionPath     = "org.example.partitionPath"
	annotationVolumeDisplayName = "org.example.volumeDisplayName"
)

const (
	annotationLayerKind = "org.example.layerKind"
	layerKindRootFiles  = "root-files"
	layerKindPartition  = "partition"
)

// validatedLayer holds per-layer metadata after validateManifestLayers succeeds.
type validatedLayer struct {
	desc        ocispec.Descriptor
	partPath    string
	isRootFiles bool
}

// detectManifestMediaType resolves tag in src and returns the manifest descriptor
// and Config.MediaType.  Used for dual-path detection (D-13) before entering either
// the legacy or chunked CAS fetch path.
func detectManifestMediaType(ctx context.Context, src oras.ReadOnlyTarget, tag string) (ocispec.Descriptor, string, error) {
	desc, err := src.Resolve(ctx, tag)
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	rc, err := src.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	defer rc.Close()
	var m ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	return desc, m.Config.MediaType, nil
}

// validateManifestLayers validates the layer annotations of a manifest.
//
// Rules enforced:
//   - Every layer must have a non-empty, relative, local annotationPartitionPath.
//   - layerKind must be "", "partition", or "root-files"; anything else → ErrIntegrity.
//   - All partitionPaths must share the same first path component (rootBase).
//   - root-files layer: partitionPath must exactly equal rootBase.
//   - partition layer: partitionPath must equal rootBase or begin with rootBase+"/".
//   - No duplicate partitionPaths.
//   - No prefix-overlap between partition layers.
func validateManifestLayers(caller string, manifest ocispec.Manifest) ([]validatedLayer, error) {
	seen := make(map[string]struct{}, len(manifest.Layers))
	var partitionPaths []string
	var rootBase string
	var layers []validatedLayer

	for i, layer := range manifest.Layers {
		partPath := layer.Annotations[annotationPartitionPath]
		layerKind := layer.Annotations[annotationLayerKind]

		if partPath == "" {
			return nil, integrityError(caller, fmt.Sprintf("missing partitionPath annotation for layer %s", layer.Digest), nil)
		}
		if filepath.IsAbs(partPath) {
			return nil, integrityError(caller, "partition path must be relative: "+partPath, nil)
		}
		if !filepath.IsLocal(partPath) {
			return nil, integrityError(caller, "partition path is not local: "+partPath, nil)
		}

		switch layerKind {
		case "", layerKindPartition, layerKindRootFiles:
		default:
			return nil, integrityError(caller, "unknown layerKind: "+layerKind, nil)
		}

		layerRoot := strings.SplitN(filepath.ToSlash(partPath), "/", 2)[0]
		if i == 0 {
			rootBase = layerRoot
		} else if layerRoot != rootBase {
			return nil, integrityError(caller,
				fmt.Sprintf("layer %s rootBase %q differs from expected %q", layer.Digest, layerRoot, rootBase), nil)
		}

		if layerKind == layerKindRootFiles && partPath != rootBase {
			return nil, integrityError(caller,
				fmt.Sprintf("root-files layer partitionPath must equal rootBase %q, got %q", rootBase, partPath), nil)
		}
		if (layerKind == "" || layerKind == layerKindPartition) &&
			partPath != rootBase && !strings.HasPrefix(filepath.ToSlash(partPath), rootBase+"/") {
			return nil, integrityError(caller,
				fmt.Sprintf("partition path %q is not under rootBase %q", partPath, rootBase), nil)
		}

		if _, dup := seen[partPath]; dup {
			return nil, conflictError(caller, fmt.Sprintf("duplicate partition path %q", partPath), nil)
		}
		seen[partPath] = struct{}{}

		if layerKind == "" || layerKind == layerKindPartition {
			for _, existing := range partitionPaths {
				if partitionPathsOverlap(existing, partPath) {
					return nil, integrityError(caller,
						fmt.Sprintf("partition paths overlap: %q and %q", existing, partPath), nil)
				}
			}
			partitionPaths = append(partitionPaths, partPath)
		}

		layers = append(layers, validatedLayer{
			desc:        layer,
			partPath:    partPath,
			isRootFiles: layerKind == layerKindRootFiles,
		})
	}
	return layers, nil
}

// rootFileSkipNames lists files that must not appear in the root-files layer
// because they are handled via other mechanisms (config descriptor, fetch-side
// reconstruction).
var rootFileSkipNames = map[string]struct{}{
	ConfigBlobJson:  {},
	VolumeIndexJson: {},
}

// buildLayerResult is returned by buildAndPushTempLayer.
type buildLayerResult struct {
	desc   ocispec.Descriptor
	pushed bool
	empty  bool // writeFn returned hasContent=false; desc is zero
}

// buildAndPushTempLayer builds one OCI layer: it creates a temp file in
// storePath, pipes writes through a MultiWriter(file, sha256, countWriter),
// calls writeFn, computes the descriptor, and calls pushFn.  The temp file is
// closed and removed before returning regardless of success or failure, so
// callers do not need to defer cleanup themselves.
//
// writeFn should return (false, nil) to signal "no content — skip push".
func buildAndPushTempLayer(
	caller string,
	storePath string,
	annotations map[string]string,
	writeFn func(io.Writer) (hasContent bool, err error),
	pushFn func(ocispec.Descriptor, io.Reader) (*bool, error),
) (buildLayerResult, error) {
	tmp, err := os.CreateTemp(storePath, ".sori-layer-*")
	if err != nil {
		return buildLayerResult{}, transportError(caller, "create temp layer file", err)
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()

	h := digest.Canonical.Hash()
	cw := &countWriter{}
	hasContent, err := writeFn(io.MultiWriter(tmp, h, cw))
	if err != nil {
		return buildLayerResult{}, err
	}
	if !hasContent {
		return buildLayerResult{empty: true}, nil
	}

	desc := ocispec.Descriptor{
		MediaType:   ocispec.MediaTypeImageLayerGzip,
		Digest:      digest.NewDigest(digest.Canonical, h),
		Size:        cw.n,
		Annotations: annotations,
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return buildLayerResult{}, transportError(caller, "seek temp layer file", err)
	}
	pushedPtr, err := pushFn(desc, tmp)
	if err != nil {
		return buildLayerResult{}, err
	}
	return buildLayerResult{
		desc:   desc,
		pushed: pushedPtr != nil && *pushedPtr,
	}, nil
}

// Deprecated: prefer Client.PackageVolume, Client.PackageVolumeWithOptions, or
// PackageVolumeToStore so new code stays on the preferred client-based core
// path.
func (vi *VolumeIndex) PublishVolume(ctx context.Context, volPath, volName string, configBlob []byte) (*VolumeIndex, error) {
	return vi.publishVolumeToStore(ctx, NewClient().localStorePath, volPath, volName, configBlob, time.Now)
}

func (vi *VolumeIndex) publishVolumeToStore(ctx context.Context, storePath, volPath, volName string, configBlob []byte, now func() time.Time) (*VolumeIndex, error) {
	store, err := oci.New(storePath)
	if err != nil {
		return nil, transportError("VolumeIndex.publishVolumeToStore", "init OCI store", err)
	}

	anyPushed := false
	pushIfNeeded := func(desc ocispec.Descriptor, r io.Reader) (*bool, error) {
		exists, err := store.Exists(ctx, desc)
		if err != nil {
			return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("check exists %s", desc.Digest), err)
		}
		if exists {
			Log.Infof("blob %s already exists, skipping", desc.Digest)
			skipped := false
			return &skipped, nil
		}
		if err := store.Push(ctx, desc, r); err != nil {
			return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("push blob %s", desc.Digest), err)
		}
		pushed := true
		return &pushed, nil
	}

	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configBlob),
		Size:      int64(len(configBlob)),
	}
	pushedPtr, err := pushIfNeeded(configDesc, bytes.NewReader(configBlob))
	if err != nil {
		return nil, err
	}
	if pushedPtr != nil && *pushedPtr {
		anyPushed = true
	}

	rootBase := filepath.Base(volPath)
	layers := make([]ocispec.Descriptor, 0, len(vi.Partitions)+1)

	const publishCaller = "VolumeIndex.publishVolumeToStore"

	if len(vi.Partitions) == 0 {
		// Flat volume (no partitions): treat all root-level regular files as a
		// root-files layer so that publish and fetch both produce Partitions:[].
		res, err := buildAndPushTempLayer(publishCaller, storePath,
			map[string]string{
				annotationPartitionPath: rootBase,
				annotationLayerKind:     layerKindRootFiles,
			},
			func(w io.Writer) (bool, error) {
				hasFiles, err := archiveutil.TarGzDirFilesTo(w, volPath, rootBase, rootFileSkipNames)
				if err != nil {
					if errors.Is(err, archiveutil.ErrValidation) {
						return false, validationError(publishCaller, fmt.Sprintf("tar.gz flat volume %q", volPath), err)
					}
					return false, transportError(publishCaller, fmt.Sprintf("tar.gz flat volume %q", volPath), err)
				}
				return hasFiles, nil
			},
			pushIfNeeded,
		)
		if err != nil {
			return nil, err
		}
		if !res.empty {
			if res.pushed {
				anyPushed = true
			}
			layers = append(layers, res.desc)
		}
	} else {
		// Push root-level files (e.g., README.md) as a separate layer so they
		// are not silently lost when only partition subdirectories are tarred.
		res, err := buildAndPushTempLayer(publishCaller, storePath,
			map[string]string{
				annotationPartitionPath: rootBase,
				annotationLayerKind:     layerKindRootFiles,
			},
			func(w io.Writer) (bool, error) {
				hasFiles, err := archiveutil.TarGzDirFilesTo(w, volPath, rootBase, rootFileSkipNames)
				if err != nil {
					if errors.Is(err, archiveutil.ErrValidation) {
						return false, validationError(publishCaller, "tar.gz root files", err)
					}
					return false, transportError(publishCaller, "tar.gz root files", err)
				}
				return hasFiles, nil
			},
			pushIfNeeded,
		)
		if err != nil {
			return nil, err
		}
		if !res.empty {
			if res.pushed {
				anyPushed = true
			}
			layers = append(layers, res.desc)
		}

		for i := range vi.Partitions {
			part := &vi.Partitions[i]
			if filepath.IsAbs(part.Path) {
				return nil, validationError("VolumeIndex.publishVolumeToStore",
					fmt.Sprintf("partition path must be relative: %q", part.Path), nil)
			}
			if !strings.HasPrefix(part.Path, rootBase+"/") {
				return nil, validationError("VolumeIndex.publishVolumeToStore",
					fmt.Sprintf("partition path %q does not start with rootBase %q", part.Path, rootBase), nil)
			}
			rel := strings.TrimPrefix(part.Path, rootBase+"/")
			if rel == "" || strings.HasPrefix(filepath.Clean(rel), "..") {
				return nil, validationError("VolumeIndex.publishVolumeToStore",
					fmt.Sprintf("partition path %q escapes volume root", part.Path), nil)
			}
			fsPath := filepath.Join(volPath, rel)
			partPath := part.Path
			res, err := buildAndPushTempLayer(publishCaller, storePath,
				map[string]string{
					annotationPartitionPath: partPath,
					annotationLayerKind:     layerKindPartition,
				},
				func(w io.Writer) (bool, error) {
					if err := archiveutil.TarGzDirTo(w, fsPath, partPath); err != nil {
						if errors.Is(err, archiveutil.ErrValidation) {
							return false, validationError(publishCaller, fmt.Sprintf("tar.gz %q", fsPath), err)
						}
						return false, transportError(publishCaller, fmt.Sprintf("tar.gz %q", fsPath), err)
					}
					return true, nil
				},
				pushIfNeeded,
			)
			if err != nil {
				return nil, err
			}
			if res.pushed {
				anyPushed = true
			}
			part.ManifestRef = res.desc.Digest.String()
			layers = append(layers, res.desc)
		}
	}

	if !anyPushed {
		resolveErr := testHookResolveExistingErr
		var existingDesc ocispec.Descriptor
		if resolveErr == nil {
			existingDesc, resolveErr = store.Resolve(ctx, volName)
		}
		switch {
		case resolveErr == nil:
			Log.Infof("No changes detected (config+layers), skipping manifest update for %q", volName)
			vi.VolumeRef = existingDesc.Digest.String()
			return vi, nil
		case errors.Is(resolveErr, errdef.ErrNotFound):
			// tag does not exist yet — proceed to pack and publish below
		default:
			return nil, transportError(publishCaller, fmt.Sprintf("resolve existing volume %q", volName), resolveErr)
		}
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1,
		ocispec.MediaTypeImageManifest,
		oras.PackManifestOptions{
			ConfigDescriptor: &configDesc,
			Layers:           layers,
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationCreated:   now().UTC().Format(time.RFC3339),
				annotationVolumeDisplayName: vi.DisplayName,
			},
		},
	)
	if err != nil {
		return nil, transportError("VolumeIndex.publishVolumeToStore", "pack manifest", err)
	}
	if err := store.Tag(ctx, manifestDesc, volName); err != nil {
		return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("tag manifest %q", volName), err)
	}
	vi.VolumeRef = manifestDesc.Digest.String()

	Log.Infof("Volume artifact %s tagged as %s", volName, manifestDesc.Digest)
	return vi, nil
}

// Deprecated: prefer Client.PushPackagedVolume or PushPackagedVolume so new
// code stays on the preferred core push path.
func PushLocalToRemote(ctx context.Context, localRepoPath, tag, remoteRepo, user, pass string, plainHTTP bool) (*PushResult, error) {
	repo, err := registryutil.NewRepository(remoteRepo, registryutil.RemoteConfig{
		PlainHTTP: plainHTTP,
		Username:  user,
		Password:  pass,
	})
	if err != nil {
		return nil, err
	}
	return pushLocalTagToRepository(ctx, localRepoPath, tag, repo)
}

func pushLocalTagToRepository(ctx context.Context, localRepoPath, tag string, repo *remote.Repository) (*PushResult, error) {
	srcStore, err := oci.New(localRepoPath)
	if err != nil {
		return nil, transportError("pushLocalTagToRepository", "init local OCI store", err)
	}
	pushedDesc, err := oras.Copy(ctx, srcStore, tag, repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		if registryutil.IsAuthError(err) {
			return nil, authError("pushLocalTagToRepository", "push to remote registry", err)
		}
		return nil, transportError("pushLocalTagToRepository", "push to remote registry", err)
	}

	ref := fmt.Sprintf("%s:%s", repo.Reference.String(), tag)
	Log.Infof("Pushed to remote: %s -> %s (%s)", tag, ref, pushedDesc.Digest)
	return &PushResult{
		Reference:      ref,
		Repository:     repo.Reference.String(),
		Tag:            tag,
		ManifestDigest: pushedDesc.Digest.String(),
	}, nil
}

// FetchVolSeq fetches a packaged dataset from a local OCI store sequentially.
// Dual-path (D-13): detects chunked CAS vs legacy format from the manifest
// Config.MediaType and dispatches accordingly.
func FetchVolSeq(ctx context.Context, destRoot, repo, tag string) (*VolumeIndex, error) {
	src, err := oci.New(repo)
	if err != nil {
		return nil, transportError("FetchVolSeq", "open OCI store", err)
	}
	manifestDesc, mediaType, err := detectManifestMediaType(ctx, src, tag)
	if err != nil {
		if registryutil.IsAuthError(err) {
			return nil, authError("FetchVolSeq", fmt.Sprintf("resolve tag %q", tag), err)
		}
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, notFoundError("FetchVolSeq", fmt.Sprintf("resolve tag %q", tag), err)
		}
		return nil, transportError("FetchVolSeq", fmt.Sprintf("resolve tag %q", tag), err)
	}
	if mediaType == chunked.MediaTypeConfig {
		if err := chunked.Fetch(ctx, repo, destRoot, tag, chunked.FetchOptions{}); err != nil {
			return nil, err
		}
		return &VolumeIndex{VolumeRef: manifestDesc.Digest.String()}, nil
	}
	return fetchVolSeqFrom(ctx, destRoot, src, tag)
}

// fetchVolSeqFrom fetches sequentially from any ReadOnlyTarget.
func fetchVolSeqFrom(ctx context.Context, destRoot string, src oras.ReadOnlyTarget, tag string) (*VolumeIndex, error) {
	manifestDesc, err := src.Resolve(ctx, tag)
	if err != nil {
		if registryutil.IsAuthError(err) {
			return nil, authError("fetchVolSeqFrom", fmt.Sprintf("resolve tag %q", tag), err)
		}
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, notFoundError("fetchVolSeqFrom", fmt.Sprintf("resolve tag %q", tag), err)
		}
		return nil, transportError("fetchVolSeqFrom", fmt.Sprintf("resolve tag %q", tag), err)
	}

	rc, err := src.Fetch(ctx, manifestDesc)
	if err != nil {
		if registryutil.IsAuthError(err) {
			return nil, authError("fetchVolSeqFrom", "fetch manifest", err)
		}
		return nil, transportError("fetchVolSeqFrom", "fetch manifest", err)
	}
	defer func() {
		if cErr := rc.Close(); cErr != nil {
			Log.Warnf("fetchVolSeqFrom: close manifest reader: %v", cErr)
		}
	}()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return nil, integrityError("fetchVolSeqFrom", "decode manifest", err)
	}

	validLayers, err := validateManifestLayers("fetchVolSeqFrom", manifest)
	if err != nil {
		return nil, err
	}

	vi := &VolumeIndex{
		VolumeRef:   manifestDesc.Digest.String(),
		DisplayName: manifest.Annotations[annotationVolumeDisplayName],
		Partitions:  nil,
	}
	if ts := manifest.Annotations[ocispec.AnnotationCreated]; ts != "" {
		vi.CreatedAt = ts
	}

	for _, vl := range validLayers {
		layerRC, err := src.Fetch(ctx, vl.desc)
		if err != nil {
			if registryutil.IsAuthError(err) {
				return nil, authError("fetchVolSeqFrom", fmt.Sprintf("fetch layer %s", vl.desc.Digest), err)
			}
			return nil, transportError("fetchVolSeqFrom", fmt.Sprintf("fetch layer %s", vl.desc.Digest), err)
		}
		if err := os.MkdirAll(destRoot, 0o755); err != nil {
			_ = layerRC.Close()
			return nil, transportError("fetchVolSeqFrom", fmt.Sprintf("create destination root %s", destRoot), err)
		}

		var extractErr error
		if vl.isRootFiles {
			extractErr = archiveutil.UntarGzDirRootFilesOnly(layerRC, destRoot, vl.partPath)
		} else {
			extractErr = archiveutil.UntarGzDirUnderPrefix(layerRC, destRoot, vl.partPath)
		}
		if extractErr != nil {
			_ = layerRC.Close()
			return nil, integrityError("fetchVolSeqFrom", fmt.Sprintf("extract layer %s", vl.desc.Digest), extractErr)
		}

		if err := layerRC.Close(); err != nil {
			return nil, transportError("fetchVolSeqFrom", fmt.Sprintf("close layer reader %s", vl.desc.Digest), err)
		}
		if !vl.isRootFiles {
			vi.Partitions = append(vi.Partitions, Partition{Name: vl.partPath, Path: vl.partPath, ManifestRef: vl.desc.Digest.String()})
		}
	}

	if err := restoreConfigBlob(ctx, src, manifest, destRoot); err != nil {
		return nil, err
	}

	if err := writeVolumeIndex(destRoot, vi); err != nil {
		return nil, err
	}
	return vi, nil
}

// FetchVolParallel fetches a packaged dataset from a local OCI store with
// parallel layer extraction.
// Dual-path (D-13): detects chunked CAS vs legacy format from the manifest
// Config.MediaType; chunked artifacts use the chunked fetcher's own concurrency.
func FetchVolParallel(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error) {
	src, err := oci.New(repo)
	if err != nil {
		return nil, transportError("FetchVolParallel", "open OCI store", err)
	}
	manifestDesc, mediaType, err := detectManifestMediaType(ctx, src, tag)
	if err != nil {
		if registryutil.IsAuthError(err) {
			return nil, authError("FetchVolParallel", fmt.Sprintf("resolve tag %q", tag), err)
		}
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, notFoundError("FetchVolParallel", fmt.Sprintf("resolve tag %q", tag), err)
		}
		return nil, transportError("FetchVolParallel", fmt.Sprintf("resolve tag %q", tag), err)
	}
	if mediaType == chunked.MediaTypeConfig {
		if err := chunked.Fetch(ctx, repo, destRoot, tag, chunked.FetchOptions{}); err != nil {
			return nil, err
		}
		return &VolumeIndex{VolumeRef: manifestDesc.Digest.String()}, nil
	}
	return fetchVolParallelFrom(ctx, destRoot, src, tag, concurrency)
}

// fetchVolParallelFrom fetches with parallel layer extraction from any ReadOnlyTarget.
func fetchVolParallelFrom(ctx context.Context, destRoot string, src oras.ReadOnlyTarget, tag string, concurrency int) (*VolumeIndex, error) {
	manifestDesc, err := src.Resolve(ctx, tag)
	if err != nil {
		if registryutil.IsAuthError(err) {
			return nil, authError("fetchVolParallelFrom", fmt.Sprintf("resolve tag %q", tag), err)
		}
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, notFoundError("fetchVolParallelFrom", fmt.Sprintf("resolve tag %q", tag), err)
		}
		return nil, transportError("fetchVolParallelFrom", fmt.Sprintf("resolve tag %q", tag), err)
	}

	rc, err := src.Fetch(ctx, manifestDesc)
	if err != nil {
		if registryutil.IsAuthError(err) {
			return nil, authError("fetchVolParallelFrom", "fetch manifest", err)
		}
		return nil, transportError("fetchVolParallelFrom", "fetch manifest", err)
	}
	defer func() {
		if cErr := rc.Close(); cErr != nil {
			Log.Warnf("fetchVolParallelFrom: close manifest reader: %v", cErr)
		}
	}()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return nil, integrityError("fetchVolParallelFrom", "decode manifest", err)
	}

	validLayers, err := validateManifestLayers("fetchVolParallelFrom", manifest)
	if err != nil {
		return nil, err
	}

	n := len(validLayers)
	vi := &VolumeIndex{
		VolumeRef:   manifestDesc.Digest.String(),
		DisplayName: manifest.Annotations[annotationVolumeDisplayName],
		Partitions:  nil,
	}
	if ts := manifest.Annotations[ocispec.AnnotationCreated]; ts != "" {
		vi.CreatedAt = ts
	}

	type layerMeta struct {
		idx int
		vl  validatedLayer
	}
	metas := make([]layerMeta, n)
	for i, vl := range validLayers {
		metas[i] = layerMeta{i, vl}
	}

	if concurrency <= 0 || concurrency > n {
		cpu := runtime.NumCPU()
		if cpu < 1 {
			cpu = 1
		}
		if cpu > n {
			cpu = n
		}
		concurrency = cpu
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type jobResult struct {
		idx         int
		p           Partition
		err         error
		isRootFiles bool
	}

	jobs := make(chan layerMeta)
	results := make(chan jobResult, concurrency)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for meta := range jobs {
			select {
			case <-ctx.Done():
				return
			default:
			}

			layerRC, err := src.Fetch(ctx, meta.vl.desc)
			if err != nil {
				var layerErr error
				if registryutil.IsAuthError(err) {
					layerErr = authError("fetchVolParallelFrom", fmt.Sprintf("fetch layer %s", meta.vl.desc.Digest), err)
				} else {
					layerErr = transportError("fetchVolParallelFrom", fmt.Sprintf("fetch layer %s", meta.vl.desc.Digest), err)
				}
				results <- jobResult{idx: meta.idx, err: layerErr}
				cancel()
				continue
			}
			if err := os.MkdirAll(destRoot, 0o755); err != nil {
				_ = layerRC.Close()
				results <- jobResult{idx: meta.idx, err: transportError("fetchVolParallelFrom", fmt.Sprintf("mkdir %s", destRoot), err)}
				cancel()
				continue
			}
			var extractErr error
			if meta.vl.isRootFiles {
				extractErr = archiveutil.UntarGzDirRootFilesOnly(layerRC, destRoot, meta.vl.partPath)
			} else {
				extractErr = archiveutil.UntarGzDirUnderPrefix(layerRC, destRoot, meta.vl.partPath)
			}
			if extractErr != nil {
				_ = layerRC.Close()
				results <- jobResult{idx: meta.idx, err: integrityError("fetchVolParallelFrom", fmt.Sprintf("extract layer %s", meta.vl.desc.Digest), extractErr)}
				cancel()
				continue
			}
			if err := layerRC.Close(); err != nil {
				results <- jobResult{idx: meta.idx, err: transportError("fetchVolParallelFrom", fmt.Sprintf("close reader %s", meta.vl.desc.Digest), err)}
				cancel()
				continue
			}

			results <- jobResult{
				idx:         meta.idx,
				p:           Partition{Name: meta.vl.partPath, Path: meta.vl.partPath, ManifestRef: meta.vl.desc.Digest.String()},
				isRootFiles: meta.vl.isRootFiles,
			}
		}
	}

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go worker()
	}

	go func() {
		defer close(jobs)
		for _, m := range metas {
			select {
			case <-ctx.Done():
				return
			case jobs <- m:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	collected := make(map[int]jobResult, n)
	var firstErr error
	for r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			cancel()
		}
		if r.err == nil {
			collected[r.idx] = r
		}
	}

	if firstErr != nil {
		return nil, firstErr
	}

	for i := 0; i < n; i++ {
		r, ok := collected[i]
		if ok && !r.isRootFiles {
			vi.Partitions = append(vi.Partitions, r.p)
		}
	}

	if err := restoreConfigBlob(ctx, src, manifest, destRoot); err != nil {
		return nil, err
	}

	if err := writeVolumeIndex(destRoot, vi); err != nil {
		return nil, err
	}
	return vi, nil
}

// testHookResolveExistingErr, if non-nil, replaces the store.Resolve call in
// the skip-if-unchanged path of publishVolumeToStore.  Used only by tests.
var testHookResolveExistingErr error

// testHookPhase2RenameErr, if non-nil, replaces the Phase 2 os.Rename call in
// fetchVolWithAtomicOverwrite.  Used only by tests.
var testHookPhase2RenameErr error

// testHookPhase3RenameErr, if non-nil, replaces the Phase 3 os.Rename call in
// fetchVolWithAtomicOverwrite.  Used only by tests.
var testHookPhase3RenameErr error

// testHookBackupCleanupErr, if non-nil, replaces the cleanup os.RemoveAll call
// in fetchVolWithAtomicOverwrite.  Used only by tests.
var testHookBackupCleanupErr error

// validateStagingDir checks that a freshly-extracted staging directory is
// internally consistent before it is committed as the new destRoot.
func validateStagingDir(caller, stagingDir string, vi *VolumeIndex) error {
	for _, p := range vi.Partitions {
		partDir := filepath.Join(stagingDir, p.Path)
		info, statErr := os.Stat(partDir)
		if statErr != nil || !info.IsDir() {
			return integrityError(caller, "partition directory missing in staging: "+p.Path, nil)
		}
	}
	stagingEntries, readErr := os.ReadDir(stagingDir)
	if readErr == nil {
		var rootBase string
		for _, e := range stagingEntries {
			if e.IsDir() {
				rootBase = e.Name()
				break
			}
		}
		if rootBase != "" {
			configPath := filepath.Join(stagingDir, rootBase, ConfigBlobJson)
			if data, readFileErr := os.ReadFile(configPath); readFileErr == nil {
				if !json.Valid(data) {
					return integrityError(caller, "configblob.json is not valid JSON", nil)
				}
			}
		}
	}
	return nil
}

// uniqueBackupPath returns a path under parent that is guaranteed not to exist.
// It creates a temporary file to obtain an OS-assigned unique name, removes the
// file immediately, and returns the path — ready to receive an os.Rename.
func uniqueBackupPath(parent, prefix string) (string, error) {
	f, err := os.CreateTemp(parent, prefix)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

// fetchVolWithStaging extracts layers to a temporary staging directory and
// atomically renames it to destRoot only on full success.
// Dual-path (D-13): detects chunked CAS vs legacy from Config.MediaType.
//
// Precondition: destRoot must not exist (call ensureDestinationAbsent first).
func fetchVolWithStaging(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error) {
	src, err := oci.New(repo)
	if err != nil {
		return nil, transportError("fetchVolWithStaging", "open OCI store", err)
	}
	manifestDesc, mediaType, err := detectManifestMediaType(ctx, src, tag)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, notFoundError("fetchVolWithStaging", fmt.Sprintf("resolve tag %q", tag), err)
		}
		return nil, transportError("fetchVolWithStaging", fmt.Sprintf("resolve tag %q", tag), err)
	}
	if mediaType == chunked.MediaTypeConfig {
		return fetchChunkedWithStaging(ctx, destRoot, repo, tag, manifestDesc.Digest.String())
	}
	return fetchVolWithStagingFrom(ctx, destRoot, src, tag, concurrency)
}

// fetchChunkedWithStaging extracts a chunked CAS artifact to a staging sibling
// of destRoot and renames it to destRoot on success.
func fetchChunkedWithStaging(ctx context.Context, destRoot, storePath, tag, manifestDigest string) (*VolumeIndex, error) {
	parent := filepath.Dir(destRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, transportError("fetchChunkedWithStaging", "create parent directory", err)
	}
	base := filepath.Base(destRoot)
	stagingDir, err := os.MkdirTemp(parent, ".staging-"+base+"-*")
	if err != nil {
		return nil, transportError("fetchChunkedWithStaging", "create staging directory", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(stagingDir)
		}
	}()
	if err := chunked.Fetch(ctx, storePath, stagingDir, tag, chunked.FetchOptions{}); err != nil {
		return nil, err
	}
	if err := os.Rename(stagingDir, destRoot); err != nil {
		return nil, transportError("fetchChunkedWithStaging", "commit staging to destination", err)
	}
	cleanup = false
	return &VolumeIndex{VolumeRef: manifestDigest}, nil
}

// fetchVolWithStagingFrom is the ReadOnlyTarget-based staging extraction path,
// shared between local OCI store and remote fetch callers.
//
// Precondition: destRoot must not exist (call ensureDestinationAbsent first).
func fetchVolWithStagingFrom(ctx context.Context, destRoot string, src oras.ReadOnlyTarget, tag string, concurrency int) (*VolumeIndex, error) {
	parent := filepath.Dir(destRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, transportError("fetchVolWithStagingFrom", "create parent directory", err)
	}
	base := filepath.Base(destRoot)
	stagingDir, err := os.MkdirTemp(parent, ".staging-"+base+"-*")
	if err != nil {
		return nil, transportError("fetchVolWithStagingFrom", "create staging directory", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(stagingDir)
		}
	}()

	var vi *VolumeIndex
	if concurrency <= 1 {
		vi, err = fetchVolSeqFrom(ctx, stagingDir, src, tag)
	} else {
		vi, err = fetchVolParallelFrom(ctx, stagingDir, src, tag, concurrency)
	}
	if err != nil {
		return nil, err
	}

	if err := validateStagingDir("fetchVolWithStagingFrom", stagingDir, vi); err != nil {
		return nil, err
	}

	if err := os.Rename(stagingDir, destRoot); err != nil {
		return nil, transportError("fetchVolWithStagingFrom", "commit staging to destination", err)
	}
	cleanup = false
	return vi, nil
}

// fetchVolWithAtomicOverwrite implements the 3-phase overwrite path for a
// local OCI store. See fetchVolWithAtomicOverwriteFrom for the full algorithm.
// Dual-path (D-13): detects chunked CAS vs legacy from Config.MediaType.
func fetchVolWithAtomicOverwrite(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error) {
	src, err := oci.New(repo)
	if err != nil {
		return nil, transportError("fetchVolWithAtomicOverwrite", "open OCI store", err)
	}
	manifestDesc, mediaType, err := detectManifestMediaType(ctx, src, tag)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, notFoundError("fetchVolWithAtomicOverwrite", fmt.Sprintf("resolve tag %q", tag), err)
		}
		return nil, transportError("fetchVolWithAtomicOverwrite", fmt.Sprintf("resolve tag %q", tag), err)
	}
	if mediaType == chunked.MediaTypeConfig {
		return fetchChunkedWithAtomicOverwrite(ctx, destRoot, repo, tag, manifestDesc.Digest.String())
	}
	return fetchVolWithAtomicOverwriteFrom(ctx, destRoot, src, tag, concurrency)
}

// fetchChunkedWithAtomicOverwrite implements the 3-phase overwrite path for a
// chunked CAS artifact stored in a local OCI store.
//
//	Phase 1 — extract to a staging sibling of destRoot via chunked.Fetch
//	Phase 2 — rename existing destRoot to a backup sibling (if present)
//	Phase 3 — rename staging to destRoot (atomic commit)
//	Cleanup — remove backup (best-effort; warning logged on failure)
func fetchChunkedWithAtomicOverwrite(ctx context.Context, destRoot, storePath, tag, manifestDigest string) (*VolumeIndex, error) {
	parent := filepath.Dir(destRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, transportError("fetchChunkedWithAtomicOverwrite", "create parent directory", err)
	}
	base := filepath.Base(destRoot)
	stagingDir, err := os.MkdirTemp(parent, ".staging-"+base+"-*")
	if err != nil {
		return nil, transportError("fetchChunkedWithAtomicOverwrite", "create staging directory", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			os.RemoveAll(stagingDir)
		}
	}()

	// Phase 1: extract to staging.
	if err := chunked.Fetch(ctx, storePath, stagingDir, tag, chunked.FetchOptions{}); err != nil {
		return nil, err
	}

	// Phase 2: back up existing destRoot if present.
	var backupPath string
	if _, statErr := os.Stat(destRoot); statErr == nil {
		bp, err := uniqueBackupPath(parent, ".backup-"+base+"-")
		if err != nil {
			return nil, transportError("fetchChunkedWithAtomicOverwrite", "reserve backup path", err)
		}
		if testHookPhase2RenameErr != nil {
			return nil, testHookPhase2RenameErr
		}
		if err := os.Rename(destRoot, bp); err != nil {
			return nil, transportError("fetchChunkedWithAtomicOverwrite", "rename destRoot to backup", err)
		}
		backupPath = bp
	}

	// Phase 3: atomic commit — rename staging to destRoot.
	phase3Err := testHookPhase3RenameErr
	if phase3Err == nil {
		phase3Err = os.Rename(stagingDir, destRoot)
	}
	if phase3Err != nil {
		if backupPath != "" {
			if rbErr := os.Rename(backupPath, destRoot); rbErr != nil {
				return nil, transportError("fetchChunkedWithAtomicOverwrite",
					fmt.Sprintf("phase 3 failed and rollback also failed; staging=%s backup=%s", stagingDir, backupPath),
					errors.Join(phase3Err, rbErr))
			}
		}
		return nil, transportError("fetchChunkedWithAtomicOverwrite", "commit staging to destination", phase3Err)
	}
	cleanupStaging = false

	// Cleanup: remove backup (best-effort).
	if backupPath != "" {
		cleanupErr := testHookBackupCleanupErr
		if cleanupErr == nil {
			cleanupErr = os.RemoveAll(backupPath)
		}
		if cleanupErr != nil {
			Log.Warnf("fetchChunkedWithAtomicOverwrite: failed to remove backup %s: %v", backupPath, cleanupErr)
		}
	}
	return &VolumeIndex{VolumeRef: manifestDigest}, nil
}

// fetchVolWithAtomicOverwriteFrom is the ReadOnlyTarget-based 3-phase overwrite
// path, shared between local OCI store and remote fetch callers.
//
//	Phase 1 — extract to a staging sibling of destRoot
//	Phase 2 — rename existing destRoot to a backup sibling (if destRoot is present)
//	Phase 3 — rename staging to destRoot (atomic commit)
//	Cleanup — remove backup (best-effort; warning logged on failure)
//
// On Phase 3 failure a best-effort rollback renames the backup back to destRoot.
// If that rollback also fails, destRoot may be absent; the error message includes
// the staging and backup paths for manual recovery.
func fetchVolWithAtomicOverwriteFrom(ctx context.Context, destRoot string, src oras.ReadOnlyTarget, tag string, concurrency int) (*VolumeIndex, error) {
	parent := filepath.Dir(destRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, transportError("fetchVolWithAtomicOverwriteFrom", "create parent directory", err)
	}
	base := filepath.Base(destRoot)
	stagingDir, err := os.MkdirTemp(parent, ".staging-"+base+"-*")
	if err != nil {
		return nil, transportError("fetchVolWithAtomicOverwriteFrom", "create staging directory", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			os.RemoveAll(stagingDir)
		}
	}()

	// Phase 1: extract to staging.
	var vi *VolumeIndex
	if concurrency <= 1 {
		vi, err = fetchVolSeqFrom(ctx, stagingDir, src, tag)
	} else {
		vi, err = fetchVolParallelFrom(ctx, stagingDir, src, tag, concurrency)
	}
	if err != nil {
		return nil, err
	}
	if err := validateStagingDir("fetchVolWithAtomicOverwriteFrom", stagingDir, vi); err != nil {
		return nil, err
	}

	// Phase 2: back up existing destRoot if present.
	var backupPath string
	if _, statErr := os.Stat(destRoot); statErr == nil {
		bp, err := uniqueBackupPath(parent, ".backup-"+base+"-")
		if err != nil {
			return nil, transportError("fetchVolWithAtomicOverwriteFrom", "reserve backup path", err)
		}
		if testHookPhase2RenameErr != nil {
			return nil, testHookPhase2RenameErr
		}
		if err := os.Rename(destRoot, bp); err != nil {
			return nil, transportError("fetchVolWithAtomicOverwriteFrom", "rename destRoot to backup", err)
		}
		backupPath = bp
	}

	// Phase 3: atomic commit — rename staging to destRoot.
	phase3Err := testHookPhase3RenameErr
	if phase3Err == nil {
		phase3Err = os.Rename(stagingDir, destRoot)
	}
	if phase3Err != nil {
		if backupPath != "" {
			if rbErr := os.Rename(backupPath, destRoot); rbErr != nil {
				return nil, transportError("fetchVolWithAtomicOverwriteFrom",
					fmt.Sprintf("phase 3 failed and rollback also failed; staging=%s backup=%s", stagingDir, backupPath),
					errors.Join(phase3Err, rbErr))
			}
		}
		return nil, transportError("fetchVolWithAtomicOverwriteFrom", "commit staging to destination", phase3Err)
	}
	cleanupStaging = false

	// Cleanup: remove backup (best-effort).
	if backupPath != "" {
		cleanupErr := testHookBackupCleanupErr
		if cleanupErr == nil {
			cleanupErr = os.RemoveAll(backupPath)
		}
		if cleanupErr != nil {
			Log.Warnf("fetchVolWithAtomicOverwriteFrom: failed to remove backup %s: %v", backupPath, cleanupErr)
		}
	}
	return vi, nil
}

// restoreConfigBlob fetches the OCI config blob from the manifest and writes it
// as configblob.json under destRoot/<rootBase>/. This restores the original
// configblob.json that was used as the config descriptor during publish.
func restoreConfigBlob(ctx context.Context, fetcher content.Fetcher, manifest ocispec.Manifest, destRoot string) error {
	if manifest.Config.MediaType != ocispec.MediaTypeImageConfig {
		return nil
	}
	// Derive rootBase from the first layer's partitionPath annotation.
	var rootBase string
	for _, layer := range manifest.Layers {
		if p := layer.Annotations[annotationPartitionPath]; p != "" {
			rootBase = strings.SplitN(p, "/", 2)[0]
			break
		}
	}
	if rootBase == "" {
		return nil
	}

	configRC, err := fetcher.Fetch(ctx, manifest.Config)
	if err != nil {
		return transportError("restoreConfigBlob", "fetch config blob", err)
	}
	defer func() {
		if cErr := configRC.Close(); cErr != nil {
			Log.Warnf("restoreConfigBlob: close config reader: %v", cErr)
		}
	}()

	data, err := io.ReadAll(configRC)
	if err != nil {
		return transportError("restoreConfigBlob", "read config blob", err)
	}
	if err := validateJSONBytes(data); err != nil {
		return integrityError("restoreConfigBlob", "config blob contains invalid JSON", err)
	}

	configPath := filepath.Join(destRoot, rootBase, ConfigBlobJson)
	if err := writeFileAtomic(configPath, data, 0o644); err != nil {
		return transportError("restoreConfigBlob", "write configblob.json", err)
	}
	return nil
}

// partitionPathsOverlap reports whether two partition paths overlap,
// meaning one is a filesystem ancestor of the other.
func partitionPathsOverlap(a, b string) bool {
	a = filepath.ToSlash(a)
	b = filepath.ToSlash(b)
	return strings.HasPrefix(b+"/", a+"/") || strings.HasPrefix(a+"/", b+"/")
}

func pushDataSpecManifest(ctx context.Context, target content.Pusher, subjectDesc ocispec.Descriptor, spec *DataSpec) (*ReferrerPushResult, error) {
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return nil, transportError("pushDataSpecManifest", "marshal data spec", err)
	}

	configDesc, err := oras.PushBytes(ctx, target, MediaTypeDataSpec, specBytes)
	if err != nil {
		return nil, transportError("pushDataSpecManifest", "push data spec blob", err)
	}

	manifestDesc, err := oras.PackManifest(ctx, target, oras.PackManifestVersion1_1,
		ocispec.MediaTypeImageManifest,
		oras.PackManifestOptions{
			Subject:          &subjectDesc,
			ConfigDescriptor: &configDesc,
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationCreated: time.Now().UTC().Format(time.RFC3339),
			},
		},
	)
	if err != nil {
		return nil, transportError("pushDataSpecManifest", "pack data spec manifest", err)
	}

	return &ReferrerPushResult{
		SubjectDigest:  subjectDesc.Digest.String(),
		ManifestDigest: manifestDesc.Digest.String(),
		ConfigDigest:   configDesc.Digest.String(),
		ArtifactType:   MediaTypeDataSpec,
	}, nil
}
