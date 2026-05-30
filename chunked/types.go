// Package chunked implements the sori chunked CAS artifact format.
//
// Chunked CAS divides each source file into fixed-size raw byte ranges (chunks)
// and stores each chunk as a separate OCI layer.  This eliminates the full-
// artifact disk spike of the legacy tar.gz format and enables push resume via
// registry-level content deduplication.
//
// Design decisions are documented in p5-rfc.md.
package chunked

// ChunkIndex is the in-memory representation of chunk-index.json (D-4).
// It is the primary storage reconstruction contract between push and fetch.
type ChunkIndex struct {
	SchemaVersion string           `json:"schemaVersion"`
	ChunkSize     int64            `json:"chunkSize"`
	Files         []ChunkIndexFile `json:"files"`
}

// ChunkIndexFile describes a single source file and its chunks.
type ChunkIndexFile struct {
	Path   string       `json:"path"`
	Mode   uint32       `json:"mode"`
	Size   int64        `json:"size"`
	Digest string       `json:"digest"` // sha256 of the full unmodified file
	Chunks []ChunkEntry `json:"chunks"`
}

// ChunkEntry describes one fixed-size byte range within a file.
type ChunkEntry struct {
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // sha256 of the raw chunk bytes
}

// OCIConfigDescriptor is the content of the OCI config descriptor blob
// for chunked CAS artifacts (D-3).
//
// mediaType: application/vnd.sori.chunked-cas.config.v1+json
//
// SchemaVersion identifies the schema of this document.
// ArtifactFormat identifies the artifact layout version.
// Keeping them separate allows the config schema to evolve (e.g. add
// ProducerVersion or Features) without changing the artifact format version.
type OCIConfigDescriptor struct {
	SchemaVersion  string `json:"schemaVersion"`
	ArtifactFormat string `json:"artifactFormat"`
}

// DatasetMetadata is the in-memory representation of dataset-metadata.json (§10-3).
// It is the catalog/UX contract between the data producer and catalog services.
// It is optional at the storage level: fetch succeeds without it.
type DatasetMetadata struct {
	SchemaVersion        string            `json:"schemaVersion"`
	Kind                 string            `json:"kind"`
	DisplayName          string            `json:"displayName"`
	Description          string            `json:"description"`
	Organism             Organism          `json:"organism,omitempty"`
	Reference            DatasetReference  `json:"reference,omitempty"`
	DataTypes            []string          `json:"dataTypes,omitempty"`
	FileFormats          []string          `json:"fileFormats,omitempty"`
	CompatibleTools      []string          `json:"compatibleTools,omitempty"`
	CompatibleNodeTypes  []string          `json:"compatibleNodeTypes,omitempty"`
	CompatibleInputTypes []string          `json:"compatibleInputTypes,omitempty"`
	CompatibleInputs     []CompatibleInput `json:"compatibleInputs,omitempty"`
	SizeBytes            int64             `json:"sizeBytes,omitempty"`
	Source               string            `json:"source,omitempty"`
	License              string            `json:"license,omitempty"`
	Tags                 []string          `json:"tags,omitempty"`
	CreatedAt            string            `json:"createdAt,omitempty"`
	CreatedBy            string            `json:"createdBy,omitempty"`
	ValidationStatus     string            `json:"validationStatus,omitempty"`
	ArtifactRef          string            `json:"artifactRef,omitempty"`
	// manifestDigest is intentionally absent: dataset-metadata.json is pushed
	// as an OCI layer; its digest contributes to the manifest digest, making
	// self-reference structurally impossible.  The catalog/indexer fills this
	// in externally after tag resolve.
}

// Organism describes the biological organism for a genomics dataset.
type Organism struct {
	Name       string `json:"name,omitempty"`
	TaxonomyID string `json:"taxonomyId,omitempty"`
}

// DatasetReference describes the reference build for a genomics dataset.
type DatasetReference struct {
	Name    string   `json:"name,omitempty"`
	Version string   `json:"version,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

// CompatibleInput is a structured compatibility record used by pipeline editors
// for precise inputType+format+organism matching (§10-3, §10-5).
type CompatibleInput struct {
	InputType       string   `json:"inputType"`
	Format          string   `json:"format"`
	CompatibleTools []string `json:"compatibleTools,omitempty"`
	Organism        string   `json:"organism,omitempty"`
	Reference       string   `json:"reference,omitempty"`
}

// ChunkProgress carries per-event progress information emitted during a
// chunked CAS push or fetch operation (§7-6).
type ChunkProgress struct {
	// Event is one of: "ChunkSkipped", "ChunkUploaded", "ChunkFetched",
	// "FileDone", "ArtifactDone".
	Event      string
	File       string
	ChunkIndex int
	Bytes      int64
	DurationMs int64
	Digest     string
}

// ProgressFunc is an optional callback receiving ChunkProgress events.
// Pass nil to suppress; chunk boundaries are still logged via the sori logger.
type ProgressFunc func(ChunkProgress)
