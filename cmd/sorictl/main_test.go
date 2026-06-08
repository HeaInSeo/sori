package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/HeaInSeo/sori"
)

func TestDefaultMetadata(t *testing.T) {
	meta := defaultMetadata(pushConfig{
		registry:    "registry.example.com",
		repository:  "bio/refdata",
		tag:         "grch38-v1",
		name:        "grch38",
		displayName: "GRCh38",
		description: "Human GRCh38 reference.",
		kind:        "reference_genome",
		sourceURI:   "https://example.com/grch38.fa.gz",
	})
	if meta.SchemaVersion != sori.DatasetMetadataSchemaVersion {
		t.Fatalf("schemaVersion = %q", meta.SchemaVersion)
	}
	if meta.ArtifactRef != "registry.example.com/bio/refdata:grch38-v1" {
		t.Fatalf("artifactRef = %q", meta.ArtifactRef)
	}
	if err := sori.ValidateDatasetMetadata(&meta); err != nil {
		t.Fatalf("ValidateDatasetMetadata: %v", err)
	}
}

func TestParseImageRef(t *testing.T) {
	ref, err := parseImageRef("localhost:5000/bio/refdata/grch38:bwa-v1")
	if err != nil {
		t.Fatalf("parseImageRef: %v", err)
	}
	if ref.registry != "localhost:5000" {
		t.Fatalf("registry = %q", ref.registry)
	}
	if ref.repository != "bio/refdata/grch38" {
		t.Fatalf("repository = %q", ref.repository)
	}
	if ref.tag != "bwa-v1" {
		t.Fatalf("tag = %q", ref.tag)
	}
}

func TestParseImageRef_Invalid(t *testing.T) {
	if _, err := parseImageRef("ghcr.io/owner/repo"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParsePushConfig_Ref(t *testing.T) {
	dir := t.TempDir()
	if err := writeTestFile(dir+"/file.txt", []byte("data")); err != nil {
		t.Fatalf("write source: %v", err)
	}
	cfg, err := parsePushConfig([]string{
		"--ref", "registry.example.com/bio/refdata:grch38-v1",
		"--display-name", "GRCh38",
		"--description", "Human GRCh38 reference.",
		dir,
	})
	if err != nil {
		t.Fatalf("parsePushConfig: %v", err)
	}
	if cfg.registry != "registry.example.com" || cfg.repository != "bio/refdata" || cfg.tag != "grch38-v1" {
		t.Fatalf("parsed ref = %s/%s:%s", cfg.registry, cfg.repository, cfg.tag)
	}
}

func TestParsePushConfig_SourceBeforeFlags(t *testing.T) {
	dir := t.TempDir()
	if err := writeTestFile(dir+"/file.txt", []byte("data")); err != nil {
		t.Fatalf("write source: %v", err)
	}
	cfg, err := parsePushConfig([]string{
		dir,
		"--ref", "registry.example.com/bio/refdata:grch38-v1",
		"--display-name", "GRCh38",
		"--description", "Human GRCh38 reference.",
	})
	if err != nil {
		t.Fatalf("parsePushConfig: %v", err)
	}
	if cfg.sourceDir != dir || cfg.repository != "bio/refdata" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV("bwa, samtools,, minimap2 ")
	want := []string{"bwa", "samtools", "minimap2"}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q", i, got[i])
		}
	}
}

func TestWriteInspectOutputText(t *testing.T) {
	out := inspectOutput{
		Reference:         "registry.example.com/bio/refdata:grch38-v1",
		ManifestDigest:    "sha256:manifest",
		ConfigMediaType:   "application/vnd.sori.chunked-cas.config.v1+json",
		ArtifactFormat:    "chunked-cas",
		LayerCount:        3,
		FileCount:         1,
		ChunkCount:        1,
		ChunkSize:         1 << 30,
		TotalSize:         1234,
		DatasetMetadata:   true,
		MetadataValid:     true,
		MetadataMediaType: sori.MediaTypeDatasetMetadata,
		Metadata: &sori.DatasetMetadata{
			SchemaVersion:   sori.DatasetMetadataSchemaVersion,
			Kind:            "reference_genome",
			DisplayName:     "GRCh38",
			Description:     "Human GRCh38 reference.",
			CompatibleTools: []string{"bwa"},
		},
	}
	var buf bytes.Buffer
	if err := writeInspectText(&buf, out); err != nil {
		t.Fatalf("writeInspectText: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("GRCh38")) {
		t.Fatalf("inspect output missing metadata: %s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("chunked-cas")) {
		t.Fatalf("inspect output missing format summary: %s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("files:")) {
		t.Fatalf("inspect output missing file summary: %s", buf.String())
	}
}

func TestArtifactFormat(t *testing.T) {
	if got := artifactFormat(sori.MediaTypeChunkedConfig); got != "chunked-cas" {
		t.Fatalf("artifactFormat = %q", got)
	}
	if got := artifactFormat("custom"); got != "custom" {
		t.Fatalf("artifactFormat custom = %q", got)
	}
}

func TestConfigureLibraryLogging_JSONDiscardsInfoLogs(t *testing.T) {
	var buf bytes.Buffer
	orig := sori.Log.Out
	t.Cleanup(func() { sori.Log.SetOutput(orig) })

	sori.Log.SetOutput(&buf)
	configureLibraryLogging("json")
	sori.Log.Info("must not be written")
	if buf.Len() != 0 {
		t.Fatalf("expected no log output, got %q", buf.String())
	}
	if sori.Log.Out != io.Discard {
		t.Fatal("expected json mode to discard library logs")
	}
}

func TestConfigureLibraryLogging_TextUsesStderr(t *testing.T) {
	orig := sori.Log.Out
	t.Cleanup(func() { sori.Log.SetOutput(orig) })

	configureLibraryLogging("text")
	if sori.Log.Out != os.Stderr {
		t.Fatal("expected text mode to use stderr")
	}
}

func TestLoadOrBuildMetadata_File(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dataset-metadata.json"
	data := []byte(`{
		"schemaVersion": "sori.dataset.metadata.v1",
		"kind": "reference_genome",
		"displayName": "GRCh38 FASTA",
		"description": "Human GRCh38 reference FASTA."
	}`)
	if err := writeTestFile(path, data); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	got, ok, err := loadOrBuildMetadata(pushConfig{metadataPath: path})
	if err != nil {
		t.Fatalf("loadOrBuildMetadata: %v", err)
	}
	if !ok {
		t.Fatal("expected metadata")
	}
	var meta sori.DatasetMetadata
	if err := json.Unmarshal(got, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.DisplayName != "GRCh38 FASTA" {
		t.Fatalf("displayName = %q", meta.DisplayName)
	}
}

func TestMetadataInitOutput_ValidatesAsDatasetMetadata(t *testing.T) {
	out, err := captureStdout(func() error {
		return runMetadataInit([]string{
			"--ref", "registry.example.com/bio/refdata:grch38-bwa-v1",
			"--name", "grch38-bwa",
			"--display-name", "GRCh38 BWA Index",
			"--description", "Human GRCh38 BWA index.",
			"--organism", "Homo sapiens",
			"--taxonomy-id", "9606",
			"--reference", "GRCh38",
			"--reference-version", "v1",
			"--format", "BWA index",
			"--tool", "bwa,bwa-mem2",
		})
	})
	if err != nil {
		t.Fatalf("runMetadataInit: %v", err)
	}
	if err := sori.ValidateDatasetMetadataJSON(out); err != nil {
		t.Fatalf("ValidateDatasetMetadataJSON: %v\n%s", err, string(out))
	}
	var meta sori.DatasetMetadata
	if err := json.Unmarshal(out, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.ArtifactRef != "registry.example.com/bio/refdata:grch38-bwa-v1" {
		t.Fatalf("artifactRef = %q", meta.ArtifactRef)
	}
	if len(meta.CompatibleTools) != 2 {
		t.Fatalf("compatibleTools = %#v", meta.CompatibleTools)
	}
}

func captureStdout(fn func() error) ([]byte, error) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	os.Stdout = w
	defer func() {
		os.Stdout = orig
	}()

	fnErr := fn()
	closeErr := w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		return nil, readErr
	}
	if fnErr != nil {
		return nil, fnErr
	}
	return out, closeErr
}

func writeTestFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
