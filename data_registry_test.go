package sori

import (
	"errors"
	"testing"
)

func TestBuildRegisteredDataDefinition(t *testing.T) {
	req := DataRegisterRequest{
		DataName:    "hg38-reference",
		Version:     "2024-01",
		Description: "reference genome",
		Format:      "FASTA",
		SourceURI:   "s3://bucket/hg38.fa.gz",
		Display: DisplaySpec{
			Category: "Reference",
			Tags:     []string{"human", "genome"},
		},
	}
	pkg := &PackageResult{
		LocalTag:       "hg38.v1",
		ManifestDigest: "sha256:local-manifest",
	}
	push := &PushResult{
		Reference:      "harbor.example/data/hg38:2024-01",
		ManifestDigest: "sha256:remote-manifest",
	}

	def, err := BuildRegisteredDataDefinition(req, pkg, push)
	if err != nil {
		t.Fatalf("BuildRegisteredDataDefinition: %v", err)
	}

	if def.StableRef != "hg38-reference@2024-01" {
		t.Fatalf("stable ref mismatch: got %q", def.StableRef)
	}
	if def.Checksum != push.ManifestDigest {
		t.Fatalf("checksum mismatch: got %q want %q", def.Checksum, push.ManifestDigest)
	}
	if def.StorageURI != push.Reference {
		t.Fatalf("storage uri mismatch: got %q want %q", def.StorageURI, push.Reference)
	}
	if def.Display.Label != req.DataName {
		t.Fatalf("display label mismatch: got %q", def.Display.Label)
	}
	if def.CASHash == "" {
		t.Fatal("expected cas hash to be populated")
	}
}

func TestBuildRegisteredDataDefinition_ValidationError(t *testing.T) {
	_, err := BuildRegisteredDataDefinition(DataRegisterRequest{}, nil, nil)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}
