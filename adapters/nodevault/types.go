package nodevault

import "github.com/HeaInSeo/sori"

// DataSpecMediaType is the canonical media type for DataSpec referrer artifacts.
// It is re-exported from the root package for adapter callers.
const DataSpecMediaType = sori.MediaTypeDataSpec

type DataSpec struct {
	Identity   DataIdentity   `json:"identity"`
	Data       DataSection    `json:"data"`
	Display    DataDisplay    `json:"display"`
	Provenance DataProvenance `json:"provenance"`
}

type DataIdentity struct {
	StableRef string `json:"stableRef"`
	Dataset   string `json:"dataset,omitempty"`
	Version   string `json:"version,omitempty"`
}

type DataSection struct {
	ArtifactType   string           `json:"artifactType"`
	Repository     string           `json:"repository,omitempty"`
	Reference      string           `json:"reference,omitempty"`
	ManifestDigest string           `json:"manifestDigest,omitempty"`
	ConfigDigest   string           `json:"configDigest"`
	TotalSize      int64            `json:"totalSize"`
	Partitions     []sori.Partition `json:"partitions"`
}

type DataDisplay struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type DataProvenance struct {
	PackagedAt string `json:"packagedAt"`
	SourceDir  string `json:"sourceDir"`
	LocalTag   string `json:"localTag"`
}

type DisplaySpec struct {
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type DataRegisterRequest struct {
	RequestID   string      `json:"request_id,omitempty"`
	DataName    string      `json:"data_name"`
	Version     string      `json:"version,omitempty"`
	Description string      `json:"description,omitempty"`
	Format      string      `json:"format,omitempty"`
	SourceURI   string      `json:"source_uri,omitempty"`
	Checksum    string      `json:"checksum,omitempty"`
	StorageURI  string      `json:"storage_uri,omitempty"`
	StableRef   string      `json:"stable_ref,omitempty"`
	Display     DisplaySpec `json:"display,omitempty"`
}

type RegisteredDataDefinition struct {
	CASHash         string      `json:"cas_hash"`
	DataName        string      `json:"data_name"`
	Version         string      `json:"version,omitempty"`
	Description     string      `json:"description,omitempty"`
	Format          string      `json:"format,omitempty"`
	SourceURI       string      `json:"source_uri,omitempty"`
	Checksum        string      `json:"checksum,omitempty"`
	StorageURI      string      `json:"storage_uri,omitempty"`
	StableRef       string      `json:"stable_ref"`
	Display         DisplaySpec `json:"display,omitempty"`
	RegisteredAt    int64       `json:"registered_at"`
	LifecyclePhase  string      `json:"lifecycle_phase"`
	IntegrityHealth string      `json:"integrity_health"`
}
