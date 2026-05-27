package nodevault

import (
	"testing"

	"github.com/HeaInSeo/sori"
)

func TestDataSpecFromArtifactMetadata(t *testing.T) {
	meta := &sori.ArtifactMetadata{
		SchemaVersion: sori.ArtifactMetadataSchemaVersion,
		Kind:          "dataset",
		Identity: sori.ArtifactIdentity{
			Name:      "hg38-reference",
			Version:   "2024-01",
			StableRef: "hg38-reference@2024-01",
		},
		Display: sori.ArtifactDisplay{
			Name:        "HumanRef",
			Description: "reference genome",
		},
		Source: sori.ArtifactSource{
			SourceDir: "./test-vol",
		},
		Location: sori.ArtifactLocation{
			LocalTag:       "hg38.v1",
			Repository:     "harbor.example/data/hg38",
			Reference:      "harbor.example/data/hg38:2024-01",
			ManifestDigest: "sha256:remote",
			ConfigDigest:   "sha256:config",
		},
		Contents: sori.ArtifactContents{
			TotalSize: 1234,
			CreatedAt: "2026-04-17T00:00:00Z",
			Partitions: []sori.Partition{
				{Name: "docs", Path: "test-vol/docs", ManifestRef: "sha256:layer"},
			},
		},
		Annotations: map[string]string{"organism": "human"},
	}

	spec := DataSpecFromArtifactMetadata(meta)
	if spec.Identity.StableRef != meta.Identity.StableRef {
		t.Fatalf("stable ref mismatch: got %q", spec.Identity.StableRef)
	}
	if spec.Data.Repository != meta.Location.Repository {
		t.Fatalf("repository mismatch: got %q", spec.Data.Repository)
	}
	if spec.Display.Annotations["organism"] != "human" {
		t.Fatalf("annotation mismatch: %+v", spec.Display.Annotations)
	}
}

func TestRegisteredDataDefinitionFromArtifactMetadata(t *testing.T) {
	meta := &sori.ArtifactMetadata{
		SchemaVersion: sori.ArtifactMetadataSchemaVersion,
		Kind:          "dataset",
		Identity: sori.ArtifactIdentity{
			Name:      "hg38-reference",
			Version:   "2024-01",
			StableRef: "hg38-reference@2024-01",
		},
		Display: sori.ArtifactDisplay{
			Name:        "HumanRef",
			Description: "reference genome",
			Category:    "Reference",
			Tags:        []string{"human"},
		},
		Source: sori.ArtifactSource{
			SourceURI: "s3://bucket/hg38.fa.gz",
		},
		Location: sori.ArtifactLocation{
			LocalTag:       "hg38.v1",
			Reference:      "harbor.example/data/hg38:2024-01",
			ManifestDigest: "sha256:remote",
		},
		Contents: sori.ArtifactContents{
			Format: "FASTA",
		},
	}

	def := RegisteredDataDefinitionFromArtifactMetadata(meta, DataRegisterRequest{})
	if def.DataName != meta.Identity.Name {
		t.Fatalf("data name mismatch: got %q", def.DataName)
	}
	if def.StorageURI != meta.Location.Reference {
		t.Fatalf("storage uri mismatch: got %q", def.StorageURI)
	}
	if def.Display.Category != meta.Display.Category {
		t.Fatalf("category mismatch: got %q", def.Display.Category)
	}
}
