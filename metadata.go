package sori

import (
	"encoding/json"
	"strings"

	"github.com/HeaInSeo/sori/chunked"
)

const ArtifactMetadataSchemaVersion = "sori.artifact.v1"

const (
	// DatasetMetadataSchemaVersion is the schemaVersion used by
	// dataset-metadata.json blobs attached to chunked CAS artifacts.
	DatasetMetadataSchemaVersion = chunked.SchemaVersionMetadata

	// MediaTypeDatasetMetadata is the OCI layer media type for
	// dataset-metadata.json.
	MediaTypeDatasetMetadata = chunked.MediaTypeDatasetMeta
)

type (
	// DatasetMetadata is the public dataset-metadata.json model used for
	// catalog and human/operator-facing dataset descriptions.
	DatasetMetadata = chunked.DatasetMetadata

	// Organism describes the biological organism for a genomics dataset.
	Organism = chunked.Organism

	// DatasetReference describes the reference build for a genomics dataset.
	DatasetReference = chunked.DatasetReference

	// CompatibleInput is a structured compatibility record used by pipeline
	// editors for precise inputType+format+organism matching.
	CompatibleInput = chunked.CompatibleInput
)

// ValidateDatasetMetadata checks the minimum catalog-capable metadata fields.
func ValidateDatasetMetadata(meta *DatasetMetadata) error {
	if meta == nil {
		return validationError("ValidateDatasetMetadata", "metadata is required", nil)
	}
	if strings.TrimSpace(meta.SchemaVersion) != DatasetMetadataSchemaVersion {
		return validationError("ValidateDatasetMetadata", "schemaVersion must be "+DatasetMetadataSchemaVersion, nil)
	}
	if strings.TrimSpace(meta.Kind) == "" {
		return validationError("ValidateDatasetMetadata", "kind is required", nil)
	}
	if strings.TrimSpace(meta.DisplayName) == "" {
		return validationError("ValidateDatasetMetadata", "displayName is required", nil)
	}
	if strings.TrimSpace(meta.Description) == "" {
		return validationError("ValidateDatasetMetadata", "description is required", nil)
	}
	return nil
}

// ValidateDatasetMetadataJSON decodes and validates dataset-metadata.json.
func ValidateDatasetMetadataJSON(data []byte) error {
	var meta DatasetMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return validationError("ValidateDatasetMetadataJSON", "decode json", err)
	}
	return ValidateDatasetMetadata(&meta)
}

// ArtifactMetadata is the generic metadata model for the preferred core path.
//
// This type is intended to remain the long-lived metadata surface even while
// higher-level NodeVault-oriented adapters stay experimental.
type ArtifactMetadata struct {
	SchemaVersion string                 `json:"schema_version"`
	Kind          string                 `json:"kind"`
	Identity      ArtifactIdentity       `json:"identity"`
	Display       ArtifactDisplay        `json:"display"`
	Source        ArtifactSource         `json:"source"`
	Location      ArtifactLocation       `json:"location"`
	Contents      ArtifactContents       `json:"contents"`
	Annotations   map[string]string      `json:"annotations,omitempty"`
	Extras        map[string]interface{} `json:"extras,omitempty"`
}

// ArtifactIdentity identifies the packaged artifact in the generic core model.
type ArtifactIdentity struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	StableRef string `json:"stable_ref"`
}

// ArtifactDisplay holds display-oriented metadata in the generic core model.
type ArtifactDisplay struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// ArtifactSource describes the source location for the generic core model.
type ArtifactSource struct {
	SourceDir string `json:"source_dir,omitempty"`
	SourceURI string `json:"source_uri,omitempty"`
}

// ArtifactLocation describes where the packaged artifact lives in the generic
// core model.
type ArtifactLocation struct {
	LocalTag       string `json:"local_tag,omitempty"`
	Repository     string `json:"repository,omitempty"`
	Reference      string `json:"reference,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
	ConfigDigest   string `json:"config_digest,omitempty"`
}

// ArtifactContents describes packaged contents in the generic core model.
type ArtifactContents struct {
	Format     string      `json:"format,omitempty"`
	TotalSize  int64       `json:"total_size"`
	CreatedAt  string      `json:"created_at,omitempty"`
	Partitions []Partition `json:"partitions,omitempty"`
}

// ArtifactMetadataInput is the input contract for BuildArtifactMetadata in the
// preferred core path.
type ArtifactMetadataInput struct {
	Kind        string
	Name        string
	Version     string
	StableRef   string
	DisplayName string
	Description string
	Category    string
	Tags        []string
	Format      string
	SourceDir   string
	SourceURI   string
	Annotations map[string]string
	Extras      map[string]interface{}
}

// BuildArtifactMetadata derives generic metadata from the preferred core
// packaging and push results.
//
// This function is part of the intended long-lived core surface and is the
// preferred metadata entrypoint for new code.
func BuildArtifactMetadata(input ArtifactMetadataInput, pkg *PackageResult, push *PushResult) (*ArtifactMetadata, error) {
	if strings.TrimSpace(input.Name) == "" {
		return nil, validationError("BuildArtifactMetadata", "name is required", nil)
	}
	if pkg == nil {
		return nil, validationError("BuildArtifactMetadata", "package result is required", nil)
	}

	stableRef := strings.TrimSpace(input.StableRef)
	if stableRef == "" {
		if strings.TrimSpace(input.Version) != "" {
			stableRef = input.Name + "@" + input.Version
		} else {
			stableRef = input.Name
		}
	}

	meta := &ArtifactMetadata{
		SchemaVersion: ArtifactMetadataSchemaVersion,
		Kind:          defaultString(input.Kind, "dataset"),
		Identity: ArtifactIdentity{
			Name:      input.Name,
			Version:   input.Version,
			StableRef: stableRef,
		},
		Display: ArtifactDisplay{
			Name:        defaultString(input.DisplayName, input.Name),
			Description: input.Description,
			Category:    input.Category,
			Tags:        cloneStringSlice(input.Tags),
		},
		Source: ArtifactSource{
			SourceDir: input.SourceDir,
			SourceURI: input.SourceURI,
		},
		Location: ArtifactLocation{
			LocalTag:       pkg.LocalTag,
			ManifestDigest: pkg.ManifestDigest,
			ConfigDigest:   pkg.ConfigDigest,
		},
		Contents: ArtifactContents{
			Format:     input.Format,
			TotalSize:  pkg.TotalSize,
			CreatedAt:  pkg.CreatedAt,
			Partitions: append([]Partition(nil), pkg.Partitions...),
		},
		Annotations: cloneAnnotations(input.Annotations),
		Extras:      cloneInterfaceMap(input.Extras),
	}
	if push != nil {
		meta.Location.Repository = push.Repository
		meta.Location.Reference = push.Reference
		meta.Location.ManifestDigest = push.ManifestDigest
	}
	return meta, nil
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

func cloneInterfaceMap(src map[string]interface{}) map[string]interface{} {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
