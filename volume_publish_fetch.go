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
	"github.com/HeaInSeo/sori/registryutil"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
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

	if len(vi.Partitions) == 0 {
		// Flat volume (no partitions): treat all root-level regular files as a
		// root-files layer so that publish and fetch both produce Partitions:[].
		flatTmp, err := os.CreateTemp(storePath, ".sori-layer-*")
		if err != nil {
			return nil, transportError("VolumeIndex.publishVolumeToStore", "create temp file for flat volume layer", err)
		}
		defer os.Remove(flatTmp.Name())
		defer flatTmp.Close()
		flatH := digest.Canonical.Hash()
		flatCW := &countWriter{}
		flatMW := io.MultiWriter(flatTmp, flatH, flatCW)
		flatHasFiles, err := archiveutil.TarGzDirFilesTo(flatMW, volPath, rootBase, rootFileSkipNames)
		if err != nil {
			if errors.Is(err, archiveutil.ErrValidation) {
				return nil, validationError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("tar.gz flat volume %q", volPath), err)
			}
			return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("tar.gz flat volume %q", volPath), err)
		}
		if flatHasFiles {
			flatDigest := digest.NewDigest(digest.Canonical, flatH)
			desc := ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageLayerGzip,
				Digest:    flatDigest,
				Size:      flatCW.n,
				Annotations: map[string]string{
					annotationPartitionPath: rootBase,
					annotationLayerKind:     layerKindRootFiles,
				},
			}
			if _, err := flatTmp.Seek(0, 0); err != nil {
				return nil, transportError("VolumeIndex.publishVolumeToStore", "seek flat volume temp file", err)
			}
			pushedPtr, err := pushIfNeeded(desc, flatTmp)
			if err != nil {
				return nil, transportError("VolumeIndex.publishVolumeToStore", "push flat volume layer", err)
			}
			if pushedPtr != nil && *pushedPtr {
				anyPushed = true
			}
			layers = append(layers, desc)
		}
	} else {
		// Push root-level files (e.g., README.md) as a separate layer so they
		// are not silently lost when only partition subdirectories are tarred.
		rootTmp, err := os.CreateTemp(storePath, ".sori-layer-*")
		if err != nil {
			return nil, transportError("VolumeIndex.publishVolumeToStore", "create temp file for root files layer", err)
		}
		defer os.Remove(rootTmp.Name())
		defer rootTmp.Close()
		rootH := digest.Canonical.Hash()
		rootCW := &countWriter{}
		rootMW := io.MultiWriter(rootTmp, rootH, rootCW)
		rootHasFiles, err := archiveutil.TarGzDirFilesTo(rootMW, volPath, rootBase, rootFileSkipNames)
		if err != nil {
			if errors.Is(err, archiveutil.ErrValidation) {
				return nil, validationError("VolumeIndex.publishVolumeToStore", "tar.gz root files", err)
			}
			return nil, transportError("VolumeIndex.publishVolumeToStore", "tar.gz root files", err)
		}
		if rootHasFiles {
			rootDigest := digest.NewDigest(digest.Canonical, rootH)
			desc := ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageLayerGzip,
				Digest:    rootDigest,
				Size:      rootCW.n,
				Annotations: map[string]string{
					annotationPartitionPath: rootBase,
					annotationLayerKind:     layerKindRootFiles,
				},
			}
			if _, err := rootTmp.Seek(0, 0); err != nil {
				return nil, transportError("VolumeIndex.publishVolumeToStore", "seek root files temp file", err)
			}
			pushedPtr, err := pushIfNeeded(desc, rootTmp)
			if err != nil {
				return nil, transportError("VolumeIndex.publishVolumeToStore", "push root files layer", err)
			}
			if pushedPtr != nil && *pushedPtr {
				anyPushed = true
			}
			layers = append(layers, desc)
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
			partTmp, err := os.CreateTemp(storePath, ".sori-layer-*")
			if err != nil {
				return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("create temp file for partition layer %q", part.Name), err)
			}
			defer os.Remove(partTmp.Name())
			defer partTmp.Close()
			partH := digest.Canonical.Hash()
			partCW := &countWriter{}
			partMW := io.MultiWriter(partTmp, partH, partCW)
			if err := archiveutil.TarGzDirTo(partMW, fsPath, part.Path); err != nil {
				if errors.Is(err, archiveutil.ErrValidation) {
					return nil, validationError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("tar.gz %q", fsPath), err)
				}
				return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("tar.gz %q", fsPath), err)
			}
			partDigest := digest.NewDigest(digest.Canonical, partH)
			desc := ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageLayerGzip,
				Digest:    partDigest,
				Size:      partCW.n,
				Annotations: map[string]string{
					annotationPartitionPath: part.Path,
					annotationLayerKind:     layerKindPartition,
				},
			}
			if _, err := partTmp.Seek(0, 0); err != nil {
				return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("seek temp file for partition layer %q", part.Name), err)
			}
			pushedPtr, err := pushIfNeeded(desc, partTmp)
			if err != nil {
				return nil, transportError("VolumeIndex.publishVolumeToStore", fmt.Sprintf("push layer %s", part.Name), err)
			}
			if pushedPtr != nil && *pushedPtr {
				anyPushed = true
			}
			part.ManifestRef = desc.Digest.String()
			layers = append(layers, desc)
		}
	}

	if !anyPushed {
		existingDesc, err := store.Resolve(ctx, volName)
		if err == nil {
			Log.Infof("No changes detected (config+layers), skipping manifest update for %q", volName)
			vi.VolumeRef = existingDesc.Digest.String()
			return vi, nil
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

func FetchVolSeq(ctx context.Context, destRoot, repo, tag string) (*VolumeIndex, error) {
	store, err := oci.New(repo)
	if err != nil {
		return nil, transportError("FetchVolSeq", "open OCI store", err)
	}

	ref := fmt.Sprintf("%s:%s", repo, tag)
	manifestDesc, err := store.Resolve(ctx, tag)
	if err != nil {
		return nil, notFoundError("FetchVolSeq", fmt.Sprintf("resolve reference %q", ref), err)
	}

	rc, err := store.Fetch(ctx, manifestDesc)
	if err != nil {
		return nil, transportError("FetchVolSeq", "fetch manifest", err)
	}
	defer rc.Close()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return nil, integrityError("FetchVolSeq", "decode manifest", err)
	}

	validLayers, err := validateManifestLayers("FetchVolSeq", manifest)
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
		layerRC, err := store.Fetch(ctx, vl.desc)
		if err != nil {
			return nil, transportError("FetchVolSeq", fmt.Sprintf("fetch layer %s", vl.desc.Digest), err)
		}
		if err := os.MkdirAll(destRoot, 0o755); err != nil {
			_ = layerRC.Close()
			return nil, transportError("FetchVolSeq", fmt.Sprintf("create destination root %s", destRoot), err)
		}

		var extractErr error
		if vl.isRootFiles {
			extractErr = archiveutil.UntarGzDirRootFilesOnly(layerRC, destRoot, vl.partPath)
		} else {
			extractErr = archiveutil.UntarGzDirUnderPrefix(layerRC, destRoot, vl.partPath)
		}
		if extractErr != nil {
			_ = layerRC.Close()
			return nil, integrityError("FetchVolSeq", fmt.Sprintf("extract layer %s", vl.desc.Digest), extractErr)
		}

		if err := layerRC.Close(); err != nil {
			return nil, transportError("FetchVolSeq", fmt.Sprintf("close layer reader %s", vl.desc.Digest), err)
		}
		if !vl.isRootFiles {
			vi.Partitions = append(vi.Partitions, Partition{Name: vl.partPath, Path: vl.partPath, ManifestRef: vl.desc.Digest.String()})
		}
	}

	if err := restoreConfigBlob(ctx, store, manifest, destRoot); err != nil {
		return nil, err
	}

	if err := writeVolumeIndex(destRoot, vi); err != nil {
		return nil, err
	}
	return vi, nil
}

func FetchVolParallel(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error) {
	store, err := oci.New(repo)
	if err != nil {
		return nil, transportError("FetchVolParallel", "open OCI store", err)
	}

	manifestDesc, err := store.Resolve(ctx, tag)
	if err != nil {
		return nil, notFoundError("FetchVolParallel", fmt.Sprintf("resolve reference %s:%s", repo, tag), err)
	}

	rc, err := store.Fetch(ctx, manifestDesc)
	if err != nil {
		return nil, transportError("FetchVolParallel", "fetch manifest", err)
	}
	defer rc.Close()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return nil, integrityError("FetchVolParallel", "decode manifest", err)
	}

	validLayers, err := validateManifestLayers("FetchVolParallel", manifest)
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

			layerRC, err := store.Fetch(ctx, meta.vl.desc)
			if err != nil {
				results <- jobResult{idx: meta.idx, err: transportError("FetchVolParallel", fmt.Sprintf("fetch layer %s", meta.vl.desc.Digest), err)}
				cancel()
				continue
			}
			if err := os.MkdirAll(destRoot, 0o755); err != nil {
				_ = layerRC.Close()
				results <- jobResult{idx: meta.idx, err: transportError("FetchVolParallel", fmt.Sprintf("mkdir %s", destRoot), err)}
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
				results <- jobResult{idx: meta.idx, err: integrityError("FetchVolParallel", fmt.Sprintf("extract layer %s", meta.vl.desc.Digest), extractErr)}
				cancel()
				continue
			}
			if err := layerRC.Close(); err != nil {
				results <- jobResult{idx: meta.idx, err: transportError("FetchVolParallel", fmt.Sprintf("close reader %s", meta.vl.desc.Digest), err)}
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

	if err := restoreConfigBlob(ctx, store, manifest, destRoot); err != nil {
		return nil, err
	}

	if err := writeVolumeIndex(destRoot, vi); err != nil {
		return nil, err
	}
	return vi, nil
}

// fetchVolWithStaging extracts layers to a temporary staging directory and
// atomically renames it to destRoot only on full success.
//
// This guarantees destRoot is either untouched (on failure) or fully populated
// (on success), preventing the partial-extraction state that direct extraction
// leaves behind when a layer download or unpack fails midway.
//
// Precondition: destRoot must not exist (call ensureDestinationAbsent first).
func fetchVolWithStaging(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error) {
	parent := filepath.Dir(destRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, transportError("fetchVolWithStaging", "create parent directory", err)
	}
	base := filepath.Base(destRoot)
	stagingDir, err := os.MkdirTemp(parent, ".staging-"+base+"-*")
	if err != nil {
		return nil, transportError("fetchVolWithStaging", "create staging directory", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(stagingDir)
		}
	}()

	var vi *VolumeIndex
	if concurrency <= 1 {
		vi, err = FetchVolSeq(ctx, stagingDir, repo, tag)
	} else {
		vi, err = FetchVolParallel(ctx, stagingDir, repo, tag, concurrency)
	}
	if err != nil {
		return nil, err
	}

	// Validate staging contents before the atomic commit.
	// Rule 1: each partition directory must exist in staging.
	for _, p := range vi.Partitions {
		partDir := filepath.Join(stagingDir, p.Path)
		info, statErr := os.Stat(partDir)
		if statErr != nil || !info.IsDir() {
			return nil, integrityError("fetchVolWithStaging", "partition directory missing in staging: "+p.Path, nil)
		}
	}

	// Rule 2: if configblob.json is present, it must be valid JSON.
	// Derive rootBase from the first directory entry under stagingDir.
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
					return nil, integrityError("fetchVolWithStaging", "configblob.json is not valid JSON", nil)
				}
			}
		}
	}

	if err := os.Rename(stagingDir, destRoot); err != nil {
		return nil, transportError("fetchVolWithStaging", "commit staging to destination", err)
	}
	cleanup = false
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
	defer configRC.Close()

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
