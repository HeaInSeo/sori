package sori

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HeaInSeo/sori/catalogutil"
	"github.com/opencontainers/go-digest"
)

const registeredDataCatalogJSON = "registered-data.json"

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

// DataRegisterResponse is returned by the experimental registration helpers.
//
// Experimental: this type is not yet part of the frozen core contract.
type DataRegisterResponse struct {
	CASHash string                    `json:"cas_hash"`
	Data    *RegisteredDataDefinition `json:"data"`
}

// DataCatalog is a local JSON-backed catalog for the current experimental
// registration model.
//
// Experimental: this helper remains in the root package for compatibility but
// is not yet part of the intended long-lived core surface.
//
// Deprecated: no production caller in this repository (sorictl, adapters/nodevault)
// or in the known downstream consumers (NodeKit, NodeVault) invokes DataCatalog;
// it is exercised only by this package's own unit test. Kept because it is listed
// in docs/public-api.md / docs/stable-api-promotion.md as Experimental public API
// that may still be consumed by external importers. See sori issue #3.
type DataCatalog struct {
	mu      sync.RWMutex
	rootDir string
}

type registeredDataCatalog struct {
	Version int                        `json:"version"`
	Data    []RegisteredDataDefinition `json:"data"`
}

// NewDataCatalog constructs the local catalog used by the experimental
// registration helpers.
//
// Experimental: this helper is not yet part of the frozen core contract.
//
// Deprecated: unused outside this package's own unit test; see the Deprecated
// note on DataCatalog and sori issue #3.
func NewDataCatalog(rootDir string) *DataCatalog {
	return &DataCatalog{rootDir: rootDir}
}

// RegisterPackagedData stores a registration record for pkg and push in the
// current experimental registration catalog.
//
// Experimental: prefer the core client path plus BuildArtifactMetadata unless
// a caller explicitly needs the current registration model.
//
// Deprecated: unused outside this package's own unit test; see the Deprecated
// note on DataCatalog and sori issue #3.
func RegisterPackagedData(ctx context.Context, rootDir string, req DataRegisterRequest, pkg *PackageResult, push *PushResult) (*DataRegisterResponse, error) {
	cat := NewDataCatalog(rootDir)
	return cat.Register(ctx, req, pkg, push)
}

// Register stores or updates a registration record in the local experimental
// catalog.
//
// Experimental: this method is not yet part of the frozen core contract.
//
// Deprecated: unused outside this package's own unit test; see the Deprecated
// note on DataCatalog and sori issue #3.
func (c *DataCatalog) Register(_ context.Context, req DataRegisterRequest, pkg *PackageResult, push *PushResult) (*DataRegisterResponse, error) {
	def, err := BuildRegisteredDataDefinition(req, pkg, push)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	coll, err := c.load()
	if err != nil {
		return nil, err
	}

	for i := range coll.Data {
		if coll.Data[i].CASHash == def.CASHash {
			coll.Data[i] = *def
			if err := c.save(coll); err != nil {
				return nil, err
			}
			return &DataRegisterResponse{CASHash: def.CASHash, Data: def}, nil
		}
	}

	coll.Data = append(coll.Data, *def)
	coll.Version++
	sort.Slice(coll.Data, func(i, j int) bool {
		if coll.Data[i].StableRef == coll.Data[j].StableRef {
			return coll.Data[i].RegisteredAt > coll.Data[j].RegisteredAt
		}
		return coll.Data[i].StableRef < coll.Data[j].StableRef
	})
	if err := c.save(coll); err != nil {
		return nil, err
	}
	return &DataRegisterResponse{CASHash: def.CASHash, Data: def}, nil
}

// Get returns one entry from the local experimental catalog by CAS hash.
//
// Experimental: this method is not yet part of the frozen core contract.
//
// Deprecated: unused outside this package's own unit test; see the Deprecated
// note on DataCatalog and sori issue #3.
func (c *DataCatalog) Get(casHash string) (*RegisteredDataDefinition, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	coll, err := c.load()
	if err != nil {
		return nil, err
	}
	for i := range coll.Data {
		if coll.Data[i].CASHash == casHash {
			item := coll.Data[i]
			return &item, nil
		}
	}
	return nil, notFoundError("DataCatalog.Get", fmt.Sprintf("registered data %q not found", casHash), nil)
}

// List returns entries from the local experimental catalog, optionally filtered
// by stableRef.
//
// Experimental: this method is not yet part of the frozen core contract.
//
// Deprecated: unused outside this package's own unit test; see the Deprecated
// note on DataCatalog and sori issue #3.
func (c *DataCatalog) List(stableRef string) ([]RegisteredDataDefinition, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	coll, err := c.load()
	if err != nil {
		return nil, err
	}
	if stableRef == "" {
		out := make([]RegisteredDataDefinition, len(coll.Data))
		copy(out, coll.Data)
		return out, nil
	}

	out := make([]RegisteredDataDefinition, 0, len(coll.Data))
	for _, item := range coll.Data {
		if item.StableRef == stableRef {
			out = append(out, item)
		}
	}
	return out, nil
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

func (c *DataCatalog) load() (*registeredDataCatalog, error) {
	coll, err := catalogutil.LoadOrInit(c.rootDir, registeredDataCatalogJSON, registeredDataCatalog{Version: 1, Data: nil})
	if err != nil {
		return nil, err
	}
	if coll.Version == 0 {
		coll.Version = 1
	}
	return coll, nil
}

func (c *DataCatalog) save(coll *registeredDataCatalog) error {
	return catalogutil.Save(c.rootDir, registeredDataCatalogJSON, coll)
}
