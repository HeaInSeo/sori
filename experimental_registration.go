package sori

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
)

// DisplaySpec is the display-oriented portion of the experimental registration
// model.
//
// Experimental: this type is NodeVault-oriented and is not yet part of the
// frozen core contract.
type DisplaySpec struct {
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// DataRegisterRequest is the current registration input model used by the
// experimental registration helpers.
//
// Experimental: this shape may still change as the NodeVault-facing contract is
// reviewed.
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

// RegisteredDataDefinition is the current persisted view of the experimental
// registration model.
//
// Experimental: this type is not yet part of the frozen core contract.
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

// BuildRegisteredDataDefinition builds the current experimental registration
// view from the generic core metadata inputs.
//
// Experimental: this adapter remains available for callers that still need the
// registration model, but it is not yet part of the frozen core contract.
func BuildRegisteredDataDefinition(req DataRegisterRequest, pkg *PackageResult, push *PushResult) (*RegisteredDataDefinition, error) {
	meta, err := BuildArtifactMetadata(ArtifactMetadataInput{
		Kind:        "dataset",
		Name:        req.DataName,
		Version:     req.Version,
		StableRef:   defaultString(req.StableRef, buildRegisteredStableRef(req.DataName, req.Version)),
		DisplayName: defaultString(req.Display.Label, req.DataName),
		Description: req.Description,
		Category:    req.Display.Category,
		Tags:        req.Display.Tags,
		Format:      req.Format,
		SourceURI:   req.SourceURI,
	}, pkg, push)
	if err != nil {
		return nil, err
	}
	def := ArtifactMetadataToRegisteredDataDefinition(meta, req)
	raw, err := json.Marshal(struct {
		DataName    string      `json:"data_name"`
		Version     string      `json:"version,omitempty"`
		Description string      `json:"description,omitempty"`
		Format      string      `json:"format,omitempty"`
		SourceURI   string      `json:"source_uri,omitempty"`
		Checksum    string      `json:"checksum,omitempty"`
		StorageURI  string      `json:"storage_uri,omitempty"`
		StableRef   string      `json:"stable_ref"`
		Display     DisplaySpec `json:"display,omitempty"`
	}{
		DataName:    def.DataName,
		Version:     def.Version,
		Description: def.Description,
		Format:      def.Format,
		SourceURI:   def.SourceURI,
		Checksum:    def.Checksum,
		StorageURI:  def.StorageURI,
		StableRef:   def.StableRef,
		Display:     def.Display,
	})
	if err != nil {
		return nil, transportError("BuildRegisteredDataDefinition", "marshal registered data definition", err)
	}
	def.CASHash = digest.FromBytes(raw).String()
	return def, nil
}

// ArtifactMetadataToRegisteredDataDefinition adapts generic ArtifactMetadata
// into the current experimental registration shape.
//
// Experimental: this adapter is not yet part of the frozen core contract.
func ArtifactMetadataToRegisteredDataDefinition(meta *ArtifactMetadata, req DataRegisterRequest) *RegisteredDataDefinition {
	if meta == nil {
		return nil
	}
	display := DisplaySpec{
		Label:       defaultString(req.Display.Label, meta.Display.Name),
		Description: defaultString(req.Display.Description, meta.Display.Description),
		Category:    defaultString(req.Display.Category, meta.Display.Category),
		Tags:        cloneStringSlice(firstNonEmptyTags(req.Display.Tags, meta.Display.Tags)),
	}
	checksum := req.Checksum
	if strings.TrimSpace(checksum) == "" {
		checksum = meta.Location.ManifestDigest
	}
	storageURI := req.StorageURI
	if strings.TrimSpace(storageURI) == "" {
		storageURI = firstNonEmptyString(meta.Location.Reference, meta.Location.LocalTag)
	}
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

func buildRegisteredStableRef(dataName, version string) string {
	dataName = strings.TrimSpace(dataName)
	version = strings.TrimSpace(version)
	if dataName == "" {
		return ""
	}
	if version == "" {
		return dataName
	}
	return dataName + "@" + version
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
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
