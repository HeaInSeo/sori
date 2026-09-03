package authority

import (
	"fmt"
	"strings"
)

// OriginKind is the typed publication origin (SORI-I1M §6). Internal only — the exact
// public enum names remain OPEN and this is not a public schema.
type OriginKind int

const (
	// OriginUnspecified is the zero value and is never a valid accepted origin.
	OriginUnspecified OriginKind = iota
	// OriginExternalImport: data imported from an external source/provider.
	OriginExternalImport
	// OriginDerivedAsset: data produced by a builder from identity-bearing inputs.
	OriginDerivedAsset
	// OriginProducedPromotion: a Produced Artifact promoted from execution truth.
	OriginProducedPromotion
)

func (k OriginKind) String() string {
	switch k {
	case OriginExternalImport:
		return "external_import"
	case OriginDerivedAsset:
		return "derived_asset"
	case OriginProducedPromotion:
		return "produced_promotion"
	default:
		return "unspecified"
	}
}

func (k OriginKind) valid() bool {
	return k == OriginExternalImport || k == OriginDerivedAsset || k == OriginProducedPromotion
}

// ContentProof is an authoritative content-proof reference for a member. It reuses
// the chunked CAS digest substrate (SORI-I1M §9): Digest is intended to carry a
// chunked whole-file/whole-content sha256 (chunked.ChunkIndexFile.Digest) or an
// equivalent authoritative content digest. The algorithm string is descriptive
// only and carries no public-schema commitment.
type ContentProof struct {
	Algorithm string
	Digest    string
}

func (p ContentProof) present() bool {
	return strings.TrimSpace(p.Digest) != ""
}

// Member is one logical member of the Asset Revision: an opaque-but-stable semantic
// key + role plus an authoritative content-proof reference.
type Member struct {
	SemanticKey string
	Role        string
	Proof       ContentProof
}

// Provenance carries origin-required, identity-bearing provenance/lineage. Only the
// fields relevant to the manifest's OriginKind are required (see validateProvenance).
// No presentation/discovery metadata belongs here.
type Provenance struct {
	// --- External Import ---
	SourceCoordinate string
	ObservedVersion  string
	ObservedChecksum string
	// UpstreamBuilderKnown must be set truthfully. When false the upstream builder
	// is explicitly UNKNOWN / NOT SUPPLIED and must never be fabricated (§6).
	UpstreamBuilderKnown bool
	UpstreamBuilder      string

	// --- Derived Asset ---
	BuilderIdentity      string
	RuntimeImageIdentity string
	FrozenRecipe         string
	InputLineage         []string

	// --- Produced Artifact Promotion ---
	ProducerRunID       string
	ProducerOutput      string
	PromotionProvenance string
}

// SemanticManifest is the minimum semantic manifest of an Asset Revision.
type SemanticManifest struct {
	Origin     OriginKind
	Members    []Member
	Provenance Provenance
	// Presentation is a non-authoritative discovery/UX overlay. It is EXCLUDED from
	// the identity-bearing fingerprint and can never cause a semantic conflict (§5).
	Presentation map[string]string
}

// validateAcceptRequest enforces structural validity, member closure, authoritative
// proofs, and origin-required provenance (SORI-I1M §3, §6). It is pure and performs
// no I/O; a failure rejects the acceptance fail-closed before any store mutation.
func validateAcceptRequest(req AcceptRequest) error {
	if req.RequestID == "" {
		return fmt.Errorf("%w: empty request id", ErrInvalidManifest)
	}
	if req.AssetID == "" {
		return fmt.Errorf("%w: empty asset id", ErrInvalidManifest)
	}
	m := req.Manifest
	if !m.Origin.valid() {
		return fmt.Errorf("%w: unspecified/invalid origin", ErrInvalidManifest)
	}
	if err := validateMembers(m.Members); err != nil {
		return err
	}
	return validateProvenance(m.Origin, m.Provenance)
}

func validateMembers(members []Member) error {
	if len(members) == 0 {
		return fmt.Errorf("%w: member set is empty (member closure requires >=1 member)", ErrInvalidManifest)
	}
	seen := make(map[string]struct{}, len(members))
	for i := range members {
		mem := members[i]
		if strings.TrimSpace(mem.SemanticKey) == "" || strings.TrimSpace(mem.Role) == "" {
			return fmt.Errorf("%w: member %d missing semantic key/role", ErrInvalidManifest, i)
		}
		if !mem.Proof.present() {
			return fmt.Errorf("%w: member %q missing authoritative content proof", ErrInvalidManifest, mem.SemanticKey)
		}
		if _, dup := seen[mem.SemanticKey]; dup {
			return fmt.Errorf("%w: duplicate member semantic key %q", ErrInvalidManifest, mem.SemanticKey)
		}
		seen[mem.SemanticKey] = struct{}{}
	}
	return nil
}

func validateProvenance(origin OriginKind, p Provenance) error {
	switch origin {
	case OriginExternalImport:
		// Truthful source coordinate required; an unknown upstream builder is
		// acceptable ONLY when explicitly marked unknown (never fabricated).
		if p.SourceCoordinate == "" {
			return fmt.Errorf("%w: external import requires a source coordinate", ErrInvalidManifest)
		}
		if !p.UpstreamBuilderKnown && p.UpstreamBuilder != "" {
			return fmt.Errorf("%w: upstream builder marked unknown but a value was supplied (no fabrication)", ErrInvalidManifest)
		}
		return nil
	case OriginDerivedAsset:
		if p.BuilderIdentity == "" || p.RuntimeImageIdentity == "" || p.FrozenRecipe == "" {
			return fmt.Errorf("%w: derived asset requires builder identity, runtime-image identity, and frozen recipe", ErrInvalidManifest)
		}
		if len(p.InputLineage) == 0 {
			return fmt.Errorf("%w: derived asset requires identity-bearing input lineage", ErrInvalidManifest)
		}
		for i, in := range p.InputLineage {
			if strings.TrimSpace(in) == "" {
				return fmt.Errorf("%w: derived asset input lineage entry %d is empty", ErrInvalidManifest, i)
			}
		}
		return nil
	case OriginProducedPromotion:
		if p.ProducerRunID == "" || p.ProducerOutput == "" {
			return fmt.Errorf("%w: produced promotion requires RunID-scoped producer/output identity", ErrInvalidManifest)
		}
		return nil
	default:
		return fmt.Errorf("%w: unspecified origin has no provenance contract", ErrInvalidManifest)
	}
}
