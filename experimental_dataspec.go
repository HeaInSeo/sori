package sori

import (
	"context"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ReferrerPushResult reports the manifest uploaded by the experimental
// referrer helpers.
//
// Experimental: this type is NodeVault-oriented and is not yet part of the
// intended long-lived core contract.
type ReferrerPushResult struct {
	SubjectDigest  string `json:"subject_digest"`
	ManifestDigest string `json:"manifest_digest"`
	ConfigDigest   string `json:"config_digest"`
	Repository     string `json:"repository"`
	ArtifactType   string `json:"artifact_type"`
}

// DataSpec is a NodeVault-oriented metadata view derived from ArtifactMetadata.
//
// Experimental: this type is kept in the root package for now, but it is not
// yet part of the frozen core contract and may move or change as the
// NodeVault-facing model settles.
type DataSpec struct {
	Identity   DataIdentity   `json:"identity"`
	Data       DataSection    `json:"data"`
	Display    DataDisplay    `json:"display"`
	Provenance DataProvenance `json:"provenance"`
}

// DataIdentity describes the logical identity section of an experimental
// DataSpec.
//
// Experimental: this shape is NodeVault-oriented and may change before any
// stable promotion.
type DataIdentity struct {
	StableRef string `json:"stableRef"`
	Dataset   string `json:"dataset,omitempty"`
	Version   string `json:"version,omitempty"`
}

// DataSection describes the artifact location and contents section of an
// experimental DataSpec.
//
// Experimental: this shape is not yet part of the frozen core contract.
type DataSection struct {
	ArtifactType   string      `json:"artifactType"`
	Repository     string      `json:"repository,omitempty"`
	Reference      string      `json:"reference,omitempty"`
	ManifestDigest string      `json:"manifestDigest,omitempty"`
	ConfigDigest   string      `json:"configDigest"`
	TotalSize      int64       `json:"totalSize"`
	Partitions     []Partition `json:"partitions"`
}

// DataDisplay describes the presentation-oriented section of an experimental
// DataSpec.
//
// Experimental: this shape is NodeVault-oriented and may change.
type DataDisplay struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// DataProvenance describes the packaging provenance section of an experimental
// DataSpec.
//
// Experimental: this shape is NodeVault-oriented and may change.
type DataProvenance struct {
	PackagedAt string `json:"packagedAt"`
	SourceDir  string `json:"sourceDir"`
	LocalTag   string `json:"localTag"`
}

// BuildDataSpec derives a NodeVault-oriented DataSpec from the generic core
// metadata inputs.
//
// Experimental: prefer BuildArtifactMetadata for the stable core candidate
// path. This helper remains available for callers that still need the current
// DataSpec shape.
func BuildDataSpec(pkg *PackageResult, push *PushResult, req PackageRequest) (*DataSpec, error) {
	if pkg == nil {
		return nil, validationError("BuildDataSpec", "package result is required", nil)
	}
	meta, err := BuildArtifactMetadata(ArtifactMetadataInput{
		Kind:        "dataset",
		Name:        defaultString(req.Dataset, req.Tag),
		Version:     req.Version,
		StableRef:   defaultString(pkg.StableRef, req.StableRef),
		DisplayName: req.DisplayName,
		Description: req.Description,
		SourceDir:   req.SourceDir,
		Annotations: req.Annotations,
	}, pkg, push)
	if err != nil {
		return nil, err
	}
	return ArtifactMetadataToDataSpec(meta), nil
}

// ArtifactMetadataToDataSpec converts generic ArtifactMetadata into the current
// NodeVault-oriented DataSpec view.
//
// Experimental: this adapter is intentionally outside the preferred core path
// and is not yet part of the frozen core contract.
func ArtifactMetadataToDataSpec(meta *ArtifactMetadata) *DataSpec {
	if meta == nil {
		return nil
	}
	return &DataSpec{
		Identity: DataIdentity{
			StableRef: meta.Identity.StableRef,
			Dataset:   meta.Identity.Name,
			Version:   meta.Identity.Version,
		},
		Data: DataSection{
			ArtifactType:   ocispec.MediaTypeImageManifest,
			Repository:     meta.Location.Repository,
			Reference:      meta.Location.Reference,
			ManifestDigest: meta.Location.ManifestDigest,
			ConfigDigest:   meta.Location.ConfigDigest,
			TotalSize:      meta.Contents.TotalSize,
			Partitions:     append([]Partition(nil), meta.Contents.Partitions...),
		},
		Display: DataDisplay{
			Name:        meta.Display.Name,
			Description: meta.Display.Description,
			Annotations: cloneAnnotations(meta.Annotations),
		},
		Provenance: DataProvenance{
			PackagedAt: meta.Contents.CreatedAt,
			SourceDir:  meta.Source.SourceDir,
			LocalTag:   meta.Location.LocalTag,
		},
	}
}

// PushRemoteDataSpecReferrer uploads an experimental DataSpec referrer for the
// pushed artifact identified by push.
//
// Experimental: this helper is NodeVault-oriented and is not yet part of the
// intended long-lived core contract.
func PushRemoteDataSpecReferrer(ctx context.Context, push *PushResult, target RemoteTarget, spec *DataSpec) (*ReferrerPushResult, error) {
	if push == nil {
		return nil, validationError("PushRemoteDataSpecReferrer", "push result is required", nil)
	}
	if spec == nil {
		return nil, validationError("PushRemoteDataSpecReferrer", "data spec is required", nil)
	}
	if strings.TrimSpace(push.Repository) == "" {
		return nil, validationError("PushRemoteDataSpecReferrer", "push result repository is empty", nil)
	}
	if strings.TrimSpace(push.ManifestDigest) == "" {
		return nil, validationError("PushRemoteDataSpecReferrer", "push result manifest digest is empty", nil)
	}

	repo, err := newRemoteRepository(push.Repository, target)
	if err != nil {
		return nil, err
	}
	subjectDesc, err := repo.Resolve(ctx, push.ManifestDigest)
	if err != nil {
		return nil, notFoundError("PushRemoteDataSpecReferrer", "resolve subject manifest", err)
	}

	result, err := pushDataSpecManifest(ctx, repo, subjectDesc, spec)
	if err != nil {
		return nil, err
	}
	result.Repository = push.Repository
	return result, nil
}
