package nodevault

import (
	"time"

	"github.com/HeaInSeo/sori"
)

func DataSpecFromArtifactMetadata(meta *sori.ArtifactMetadata) *DataSpec {
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
			ArtifactType:   "application/vnd.oci.image.manifest.v1+json",
			Repository:     meta.Location.Repository,
			Reference:      meta.Location.Reference,
			ManifestDigest: meta.Location.ManifestDigest,
			ConfigDigest:   meta.Location.ConfigDigest,
			TotalSize:      meta.Contents.TotalSize,
			Partitions:     append([]sori.Partition(nil), meta.Contents.Partitions...),
		},
		Display: DataDisplay{
			Name:        meta.Display.Name,
			Description: meta.Display.Description,
			Annotations: cloneStringMap(meta.Annotations),
		},
		Provenance: DataProvenance{
			PackagedAt: meta.Contents.CreatedAt,
			SourceDir:  meta.Source.SourceDir,
			LocalTag:   meta.Location.LocalTag,
		},
	}
}

func RegisteredDataDefinitionFromArtifactMetadata(meta *sori.ArtifactMetadata, req DataRegisterRequest) *RegisteredDataDefinition {
	if meta == nil {
		return nil
	}
	display := DisplaySpec{
		Label:       firstNonEmpty(req.Display.Label, meta.Display.Name),
		Description: firstNonEmpty(req.Display.Description, meta.Display.Description),
		Category:    firstNonEmpty(req.Display.Category, meta.Display.Category),
		Tags:        cloneStringSlice(firstNonEmptyTags(req.Display.Tags, meta.Display.Tags)),
	}
	checksum := firstNonEmpty(req.Checksum, meta.Location.ManifestDigest)
	storageURI := firstNonEmpty(req.StorageURI, meta.Location.Reference, meta.Location.LocalTag)
	return &RegisteredDataDefinition{
		DataName:        meta.Identity.Name,
		Version:         meta.Identity.Version,
		Description:     meta.Display.Description,
		Format:          meta.Contents.Format,
		SourceURI:       meta.Source.SourceURI,
		Checksum:        checksum,
		StorageURI:      storageURI,
		StableRef:       meta.Identity.StableRef,
		Display:         display,
		RegisteredAt:    time.Now().Unix(),
		LifecyclePhase:  "Active",
		IntegrityHealth: "Healthy",
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptyTags(values ...[]string) []string {
	for _, v := range values {
		if len(v) > 0 {
			return v
		}
	}
	return nil
}
