package sori

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/HeaInSeo/sori/chunked"
	"github.com/HeaInSeo/sori/registryutil"
	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

type (
	// Partition describes one partition entry inside the packaged dataset.
	//
	// Partition is part of the preferred core path because it is used by the
	// core packaging, push, and fetch results.
	Partition struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		ManifestRef string `json:"manifest_ref"`
		CreatedAt   string `json:"created_at"`
		Compression string `json:"compression"`
	}
	// VolumeIndex describes the partition layout of a packaged dataset.
	//
	// VolumeIndex is part of the core candidate surface and is used by the
	// preferred client path as well as compatibility helpers.
	VolumeIndex struct {
		VolumeRef   string      `json:"volume_ref"`
		DisplayName string      `json:"display_name"`
		CreatedAt   string      `json:"created_at"`
		Partitions  []Partition `json:"partitions"`
		// Skipped is true when FetchVolumeFromRemote detected that destRoot
		// already holds the requested manifest digest and skipped the download.
		// This field is runtime-only and is never written to volume-index.json.
		Skipped bool `json:"-"`
	}
	ConfigBlob  map[string]interface{}
	VolumeEntry struct {
		Index      VolumeIndex `json:"index"`
		ConfigBlob ConfigBlob  `json:"configBlob"`
	}
	VolumeCollection struct {
		Version int           `json:"version"`
		Volumes []VolumeEntry `json:"volumes"`
	}
	CollectionManager struct {
		mu    sync.RWMutex
		root  string
		coll  *VolumeCollection
		byRef map[string]int
	}
	// PackageRequest is the input to the preferred core packaging path.
	//
	// This request type is intended to remain part of the long-lived core
	// surface used by Client.PackageVolume and PackageVolumeToStore.
	PackageRequest struct {
		SourceDir   string            `json:"source_dir"`
		DisplayName string            `json:"display_name"`
		Tag         string            `json:"tag"`
		Dataset     string            `json:"dataset,omitempty"`
		Version     string            `json:"version,omitempty"`
		StableRef   string            `json:"stable_ref,omitempty"`
		Description string            `json:"description,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
		ConfigBlob  []byte            `json:"-"`
	}
	// PackageResult is the output of the preferred core packaging path.
	//
	// This result type is part of the stable core candidate surface used by the
	// client, metadata builder, and remote push flow.
	PackageResult struct {
		StableRef      string      `json:"stable_ref"`
		LocalTag       string      `json:"local_tag"`
		ManifestDigest string      `json:"manifest_digest"`
		ConfigDigest   string      `json:"config_digest"`
		TotalSize      int64       `json:"total_size"`
		CreatedAt      string      `json:"created_at"`
		Partitions     []Partition `json:"partitions"`
		VolumeIndex    VolumeIndex `json:"volume_index"`
	}
	// RemoteTarget describes how the preferred core push path should reach a
	// remote OCI registry.
	//
	// This type is part of the intended long-lived core surface used by
	// PushPackagedVolume and Client.PushPackagedVolume.
	RemoteTarget struct {
		Registry            string              `json:"registry"`
		Repository          string              `json:"repository"`
		PlainHTTP           bool                `json:"plain_http"`
		InsecureTLS         bool                `json:"insecure_tls,omitempty"`
		Username            string              `json:"username,omitempty"`
		Password            string              `json:"password,omitempty"`
		Token               string              `json:"token,omitempty"`
		CAFile              string              `json:"ca_file,omitempty"`
		HTTPClient          *http.Client        `json:"-"`
		Transport           http.RoundTripper   `json:"-"`
		AuthProvider        auth.CredentialFunc `json:"-"`
		ReferrersCapability *bool               `json:"-"`
	}
	// PushResult reports the remote artifact identity returned by the preferred
	// core push path.
	//
	// This result type is part of the stable core candidate surface.
	PushResult struct {
		Reference      string `json:"reference"`
		Repository     string `json:"repository"`
		Tag            string `json:"tag"`
		ManifestDigest string `json:"manifest_digest"`
	}
)

const (
	ConfigBlobJson  = "configblob.json"
	CollectionJson  = "volume-collection.json"
	VolumeIndexJson = "volume-index.json"
)

// Deprecated: prefer Config.NewClient followed by Client.PackageVolume or
// Client.PackageVolumeWithOptions so new code stays on the preferred core path.
func PackageVolume(ctx context.Context, req PackageRequest) (*PackageResult, error) {
	return NewClient().PackageVolume(ctx, req)
}

// PackageVolumeToStore packages a dataset into the given local OCI store using
// the preferred core packaging contract.
func PackageVolumeToStore(ctx context.Context, localStorePath string, req PackageRequest) (*PackageResult, error) {
	return packageVolumeToStoreWithOptions(ctx, localStorePath, req, PackageOptions{ConfigBlob: req.ConfigBlob}, time.Now)
}

func packageVolumeToStoreWithOptions(ctx context.Context, localStorePath string, req PackageRequest, opts PackageOptions, now func() time.Time) (*PackageResult, error) {
	if strings.TrimSpace(localStorePath) == "" {
		return nil, validationError("PackageVolumeToStore", "local store path is required", nil)
	}
	if strings.TrimSpace(req.SourceDir) == "" {
		return nil, validationError("PackageVolumeToStore", "source_dir is required", nil)
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		return nil, validationError("PackageVolumeToStore", "display_name is required", nil)
	}
	if strings.TrimSpace(req.Tag) == "" {
		return nil, validationError("PackageVolumeToStore", "tag is required", nil)
	}

	// Chunked CAS path: does not call ValidateVolumeDir (avoids mutating srcDir).
	if opts.Format == ArtifactFormatChunkedCAS {
		var configBlob []byte
		if len(opts.ConfigBlob) > 0 {
			if err := validateJSONBytes(opts.ConfigBlob); err != nil {
				return nil, validationError("PackageVolumeToStore", "invalid config blob", err)
			}
			configBlob = opts.ConfigBlob
		} else if len(req.ConfigBlob) > 0 {
			if err := validateJSONBytes(req.ConfigBlob); err != nil {
				return nil, validationError("PackageVolumeToStore", "invalid config blob", err)
			}
			configBlob = append([]byte(nil), req.ConfigBlob...)
		} else if opts.RequireConfigBlob {
			return nil, validationError("PackageVolumeToStore", "config blob is required by options", nil)
		}
		if len(opts.DatasetMetadata) > 0 {
			if err := ValidateDatasetMetadataJSON(opts.DatasetMetadata); err != nil {
				return nil, err
			}
		}
		return packageVolumeChunked(ctx, localStorePath, req, opts, configBlob, now)
	}

	// Legacy tar.gz path.
	var configBlob []byte
	if len(opts.ConfigBlob) > 0 {
		req.ConfigBlob = opts.ConfigBlob
	}
	if len(req.ConfigBlob) == 0 {
		if opts.RequireConfigBlob {
			return nil, validationError("PackageVolumeToStore", "config blob is required by options", nil)
		}
		var err error
		configBlob, err = ValidateVolumeDir(req.SourceDir)
		if err != nil {
			return nil, err
		}
	} else {
		if err := validateJSONBytes(req.ConfigBlob); err != nil {
			return nil, validationError("PackageVolumeToStore", "invalid config blob", err)
		}
		configBlob = append([]byte(nil), req.ConfigBlob...)
	}

	vi, err := GenerateVolumeIndex(req.SourceDir, req.DisplayName)
	if err != nil {
		return nil, transportError("PackageVolumeToStore", "generate volume index", err)
	}

	published, err := vi.publishVolumeToStore(ctx, localStorePath, req.SourceDir, req.Tag, configBlob, now)
	if err != nil {
		return nil, err
	}

	totalSize, err := dirRegularFileSize(req.SourceDir)
	if err != nil {
		return nil, transportError("PackageVolumeToStore", "compute total size", err)
	}

	return &PackageResult{
		StableRef:      deriveStableRef(req),
		LocalTag:       req.Tag,
		ManifestDigest: published.VolumeRef,
		ConfigDigest:   digest.FromBytes(configBlob).String(),
		TotalSize:      totalSize,
		CreatedAt:      published.CreatedAt,
		Partitions:     append([]Partition(nil), published.Partitions...),
		VolumeIndex:    *published,
	}, nil
}

// packageVolumeChunked handles the ArtifactFormatChunkedCAS dispatch path.
// Unlike the legacy path it does NOT call ValidateVolumeDir, so srcDir is never
// modified as a side-effect of packaging.
func packageVolumeChunked(
	ctx context.Context, storePath string, req PackageRequest,
	opts PackageOptions, configBlob []byte, now func() time.Time,
) (*PackageResult, error) {
	publishOpts := chunked.PublishOptions{
		ConfigBlob:      configBlob,
		DatasetMetadata: opts.DatasetMetadata,
		Progress:        opts.Progress,
	}
	manifestDesc, err := chunked.Publish(ctx, storePath, req.SourceDir, req.Tag, publishOpts)
	if err != nil {
		return nil, err
	}

	totalSize, err := dirRegularFileSize(req.SourceDir)
	if err != nil {
		return nil, transportError("PackageVolumeToStore", "compute total size", err)
	}

	createdAt := now().UTC().Format(time.RFC3339)
	vi := VolumeIndex{
		VolumeRef:   manifestDesc.Digest.String(),
		DisplayName: req.DisplayName,
		CreatedAt:   createdAt,
	}
	return &PackageResult{
		StableRef:      deriveStableRef(req),
		LocalTag:       req.Tag,
		ManifestDigest: manifestDesc.Digest.String(),
		ConfigDigest:   digest.FromBytes(configBlob).String(),
		TotalSize:      totalSize,
		CreatedAt:      createdAt,
		VolumeIndex:    vi,
	}, nil
}

// PushPackagedVolume copies a packaged artifact from the local OCI store to a
// remote registry using the preferred core push contract.
func PushPackagedVolume(ctx context.Context, localStorePath string, pkg *PackageResult, target RemoteTarget) (*PushResult, error) {
	if pkg == nil {
		return nil, validationError("PushPackagedVolume", "package result is required", nil)
	}
	if strings.TrimSpace(pkg.LocalTag) == "" {
		return nil, validationError("PushPackagedVolume", "package result local tag is empty", nil)
	}
	if strings.TrimSpace(target.Registry) == "" {
		return nil, validationError("PushPackagedVolume", "remote target registry is required", nil)
	}
	if strings.TrimSpace(target.Repository) == "" {
		return nil, validationError("PushPackagedVolume", "remote target repository is required", nil)
	}

	remoteRepo := strings.TrimRight(target.Registry, "/") + "/" + strings.TrimLeft(target.Repository, "/")
	repo, err := newRemoteRepository(remoteRepo, target)
	if err != nil {
		return nil, err
	}
	return pushLocalTagToRepository(ctx, localStorePath, pkg.LocalTag, repo)
}

func deriveStableRef(req PackageRequest) string {
	if ref := strings.TrimSpace(req.StableRef); ref != "" {
		return ref
	}
	dataset := strings.TrimSpace(req.Dataset)
	version := strings.TrimSpace(req.Version)
	switch {
	case dataset != "" && version != "":
		return dataset + ":" + version
	case dataset != "":
		return dataset
	default:
		return strings.TrimSpace(req.Tag)
	}
}

func cloneAnnotations(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func newRemoteRepository(remoteRepo string, target RemoteTarget) (*remote.Repository, error) {
	return registryutil.NewRepository(remoteRepo, registryutil.RemoteConfig{
		PlainHTTP:           target.PlainHTTP,
		InsecureTLS:         target.InsecureTLS,
		Username:            target.Username,
		Password:            target.Password,
		Token:               target.Token,
		CAFile:              target.CAFile,
		HTTPClient:          target.HTTPClient,
		Transport:           target.Transport,
		AuthProvider:        target.AuthProvider,
		ReferrersCapability: target.ReferrersCapability,
	})
}
