package sori

import (
	"errors"
	"testing"
)

func TestBuildArtifactMetadata(t *testing.T) {
	pkg := &PackageResult{
		LocalTag:       "hg38.v1",
		ManifestDigest: "sha256:local",
		ConfigDigest:   "sha256:config",
		TotalSize:      1234,
		CreatedAt:      "2026-04-17T00:00:00Z",
		Partitions: []Partition{
			{Name: "docs", Path: "test-vol/docs", ManifestRef: "sha256:layer"},
		},
	}
	push := &PushResult{
		Repository:     "harbor.example/data/hg38",
		Reference:      "harbor.example/data/hg38:2026.04",
		ManifestDigest: "sha256:remote",
	}

	meta, err := BuildArtifactMetadata(ArtifactMetadataInput{
		Kind:        "dataset",
		Name:        "hg38",
		Version:     "2026.04",
		StableRef:   "hg38@2026.04",
		DisplayName: "HumanRef GRCh38",
		Description: "reference genome",
		Category:    "Reference",
		Tags:        []string{"human", "genome"},
		Format:      "FASTA",
		SourceDir:   "./test-vol",
		SourceURI:   "s3://bucket/hg38.fa.gz",
		Annotations: map[string]string{"organism": "human"},
	}, pkg, push)
	if err != nil {
		t.Fatalf("BuildArtifactMetadata: %v", err)
	}

	if meta.SchemaVersion != ArtifactMetadataSchemaVersion {
		t.Fatalf("schema version mismatch: got %q", meta.SchemaVersion)
	}
	if meta.Identity.StableRef != "hg38@2026.04" {
		t.Fatalf("stable ref mismatch: got %q", meta.Identity.StableRef)
	}
	if meta.Location.ManifestDigest != push.ManifestDigest {
		t.Fatalf("manifest digest mismatch: got %q", meta.Location.ManifestDigest)
	}
	if meta.Contents.Format != "FASTA" {
		t.Fatalf("format mismatch: got %q", meta.Contents.Format)
	}
	if meta.Display.Category != "Reference" {
		t.Fatalf("category mismatch: got %q", meta.Display.Category)
	}
}

func TestArtifactMetadataAdapters(t *testing.T) {
	meta := &ArtifactMetadata{
		SchemaVersion: ArtifactMetadataSchemaVersion,
		Kind:          "dataset",
		Identity: ArtifactIdentity{
			Name:      "hg38-reference",
			Version:   "2024-01",
			StableRef: "hg38-reference@2024-01",
		},
		Display: ArtifactDisplay{
			Name:        "HumanRef",
			Description: "reference genome",
			Category:    "Reference",
			Tags:        []string{"human"},
		},
		Source: ArtifactSource{
			SourceDir: "./test-vol",
			SourceURI: "s3://bucket/hg38.fa.gz",
		},
		Location: ArtifactLocation{
			LocalTag:       "hg38.v1",
			Repository:     "harbor.example/data/hg38",
			Reference:      "harbor.example/data/hg38:2024-01",
			ManifestDigest: "sha256:remote",
			ConfigDigest:   "sha256:config",
		},
		Contents: ArtifactContents{
			Format:    "FASTA",
			TotalSize: 1234,
			CreatedAt: "2026-04-17T00:00:00Z",
			Partitions: []Partition{
				{Name: "docs", Path: "test-vol/docs", ManifestRef: "sha256:layer"},
			},
		},
		Annotations: map[string]string{"organism": "human"},
	}

	spec := ArtifactMetadataToDataSpec(meta)
	if spec.Identity.StableRef != meta.Identity.StableRef {
		t.Fatalf("dataspec stable ref mismatch: got %q", spec.Identity.StableRef)
	}
	if spec.Data.Repository != meta.Location.Repository {
		t.Fatalf("dataspec repository mismatch: got %q", spec.Data.Repository)
	}

	registered := ArtifactMetadataToRegisteredDataDefinition(meta, DataRegisterRequest{})
	if registered.DataName != meta.Identity.Name {
		t.Fatalf("registered data name mismatch: got %q", registered.DataName)
	}
	if registered.Format != meta.Contents.Format {
		t.Fatalf("registered format mismatch: got %q", registered.Format)
	}
	if registered.StorageURI != meta.Location.Reference {
		t.Fatalf("registered storage uri mismatch: got %q", registered.StorageURI)
	}
}

func TestValidateDatasetMetadata(t *testing.T) {
	meta := &DatasetMetadata{
		SchemaVersion: DatasetMetadataSchemaVersion,
		Kind:          "reference_genome",
		DisplayName:   "GRCh38 FASTA",
		Description:   "Human GRCh38 reference FASTA.",
	}
	if err := ValidateDatasetMetadata(meta); err != nil {
		t.Fatalf("ValidateDatasetMetadata: %v", err)
	}
}

func TestValidateDatasetMetadataJSON_InvalidSchema(t *testing.T) {
	err := ValidateDatasetMetadataJSON([]byte(`{
		"schemaVersion": "wrong",
		"kind": "reference_genome",
		"displayName": "GRCh38 FASTA",
		"description": "Human GRCh38 reference FASTA."
	}`))
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}
