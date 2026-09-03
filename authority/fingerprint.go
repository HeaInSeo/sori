package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// computeFingerprint canonicalizes the identity-bearing facts of an acceptance
// request into a stable internal comparison fingerprint (SORI-I1M §5). It includes
// ONLY identity-bearing facts:
//   - the asset the revision belongs to;
//   - the typed publication origin;
//   - the members {semantic key + role + authoritative content-proof reference};
//   - the origin-required identity-bearing provenance/lineage.
//
// Presentation/discovery metadata (SemanticManifest.Presentation) is deliberately
// excluded, so a presentation-only change never alters the fingerprint. Members and
// input lineage are order-normalized so member ordering is not identity-bearing.
//
// This value is INTERNAL acceptance/comparison evidence only. It is not exported as,
// and must not be frozen into, the public Revision-ID or a public hash algorithm.
func computeFingerprint(assetID AssetID, m SemanticManifest) string {
	type fpMember struct {
		Key       string `json:"key"`
		Role      string `json:"role"`
		Algorithm string `json:"algorithm"`
		Digest    string `json:"digest"`
	}

	members := make([]fpMember, 0, len(m.Members))
	for i := range m.Members {
		mem := m.Members[i]
		members = append(members, fpMember{
			Key:       mem.SemanticKey,
			Role:      mem.Role,
			Algorithm: mem.Proof.Algorithm,
			Digest:    mem.Proof.Digest,
		})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Key != members[j].Key {
			return members[i].Key < members[j].Key
		}
		if members[i].Role != members[j].Role {
			return members[i].Role < members[j].Role
		}
		return members[i].Digest < members[j].Digest
	})

	// Input lineage order is treated as identity-bearing: it is NOT sorted, so
	// order-distinct derived lineages ([A,B] vs [B,A]) produce distinct fingerprints
	// (fail-closed — never silently merge two genuinely different derived manifests).
	// Providing a canonical input order is the caller's responsibility.
	lineage := append([]string(nil), m.Provenance.InputLineage...)

	payload := struct {
		Asset   string     `json:"asset"`
		Origin  int        `json:"origin"`
		Members []fpMember `json:"members"`
		Prov    provFacts  `json:"provenance"`
	}{
		Asset:   string(assetID),
		Origin:  int(m.Origin),
		Members: members,
		Prov: provFacts{
			SourceCoordinate:     m.Provenance.SourceCoordinate,
			ObservedVersion:      m.Provenance.ObservedVersion,
			ObservedChecksum:     m.Provenance.ObservedChecksum,
			UpstreamBuilderKnown: m.Provenance.UpstreamBuilderKnown,
			UpstreamBuilder:      m.Provenance.UpstreamBuilder,
			BuilderIdentity:      m.Provenance.BuilderIdentity,
			RuntimeImageIdentity: m.Provenance.RuntimeImageIdentity,
			FrozenRecipe:         m.Provenance.FrozenRecipe,
			InputLineage:         lineage,
			ProducerRunID:        m.Provenance.ProducerRunID,
			ProducerOutput:       m.Provenance.ProducerOutput,
			PromotionProvenance:  m.Provenance.PromotionProvenance,
		},
	}

	// json.Marshal emits struct fields in declaration order and map-free content
	// here, so the encoding is canonical for equal inputs.
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// provFacts is the identity-bearing provenance projection used by the fingerprint.
// It intentionally omits any presentation/discovery facts.
type provFacts struct {
	SourceCoordinate     string   `json:"sourceCoordinate,omitempty"`
	ObservedVersion      string   `json:"observedVersion,omitempty"`
	ObservedChecksum     string   `json:"observedChecksum,omitempty"`
	UpstreamBuilderKnown bool     `json:"upstreamBuilderKnown,omitempty"`
	UpstreamBuilder      string   `json:"upstreamBuilder,omitempty"`
	BuilderIdentity      string   `json:"builderIdentity,omitempty"`
	RuntimeImageIdentity string   `json:"runtimeImageIdentity,omitempty"`
	FrozenRecipe         string   `json:"frozenRecipe,omitempty"`
	InputLineage         []string `json:"inputLineage,omitempty"`
	ProducerRunID        string   `json:"producerRunId,omitempty"`
	ProducerOutput       string   `json:"producerOutput,omitempty"`
	PromotionProvenance  string   `json:"promotionProvenance,omitempty"`
}
