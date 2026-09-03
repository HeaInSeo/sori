package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SORI-I2R: attach 0..N physical Representations to one already-accepted immutable
// Revision. A Representation is a physical materialization (e.g. a chunked-CAS OCI
// package vs a mountable OCI image) of the SAME logical members the Revision accepted;
// it never redefines or rewrites the accepted Revision (SORI-I1M authority truth).

// RepresentationID is the opaque internal identity of a physical Representation. It is
// derived from the representation's identity-bearing facts (physical format + the
// member-equivalence proof set) — NEVER from a locator/replica coordinate — so a
// mutable locator/tag change never alters it. Internal only: not a public
// identifier/hash commitment.
type RepresentationID string

// Locator is a mutable coordinate describing where a Representation is physically
// available (registry/tag/path/replica). It is NOT identity-bearing: multiple locators
// may describe availability of the same Representation, and changing one never changes
// the Representation's or the Revision's semantic identity.
type Locator struct {
	Scheme     string
	Coordinate string
}

// Representation sentinel errors.
var (
	// ErrInvalidRepresentation reports a structurally invalid attach request.
	ErrInvalidRepresentation = errors.New("authority: invalid representation attach request")
	// ErrMemberEquivalence reports that a representation's member set/proofs do not
	// prove semantic equivalence to the accepted Revision's logical member contract.
	ErrMemberEquivalence = errors.New("authority: representation does not prove member equivalence to the accepted revision")
	// ErrAttachConflict reports the same attach-operation id re-used with a different
	// immutable relation (representation) semantics.
	ErrAttachConflict = errors.New("authority: attach operation id conflicts with a prior representation relation")
	// ErrRepresentationNotFound reports an unknown RepresentationID.
	ErrRepresentationNotFound = errors.New("authority: representation not found")
)

// AttachRequest attaches a physical Representation to an accepted Revision. The attach
// is idempotent by AttachOperationID; the immutable relation semantics are
// (RevisionID, Format, member-equivalence proof) — Locators are mutable and NOT part
// of it.
type AttachRequest struct {
	AttachOperationID RequestID
	AssetID           AssetID
	RevisionID        RevisionID
	// Format is the opaque physical-form identifier (e.g. "chunked-cas", "mountable-oci").
	// Exact vocabulary is OPEN; it is identity-bearing so distinct formats of the same
	// logical Revision are distinct Representations.
	Format string
	// MemberProofs is the representation's realized member set; it MUST prove semantic
	// equivalence to the accepted Revision's logical members (same semantic keys + same
	// authoritative content-proof digests). Path/tag/location alone is insufficient.
	MemberProofs []Member
	// Locators are 0..N mutable availability coordinates; NOT identity-bearing.
	Locators []Locator
}

// Representation is a physical materialization attached to an accepted Revision.
// RepresentationID/Fingerprint are its semantic identity (format + member equivalence);
// Locators and Healthy are mutable availability facts that never change that identity
// or the accepted Revision.
type Representation struct {
	RepresentationID  RepresentationID
	RevisionID        RevisionID
	AssetID           AssetID
	Format            string
	Fingerprint       string
	MemberProofs      []Member
	Locators          []Locator
	Healthy           bool
	AttachOperationID RequestID
	AttachedAt        time.Time
}

// validateAttachRequest enforces structural validity of an attach request. Member
// equivalence to the target Revision is checked separately (needs the Revision).
func validateAttachRequest(req AttachRequest) error {
	if req.AttachOperationID == "" {
		return fmt.Errorf("%w: empty attach operation id", ErrInvalidRepresentation)
	}
	if req.AssetID == "" || req.RevisionID == "" {
		return fmt.Errorf("%w: empty asset/revision id", ErrInvalidRepresentation)
	}
	if strings.TrimSpace(req.Format) == "" {
		return fmt.Errorf("%w: empty representation format", ErrInvalidRepresentation)
	}
	// The representation's member set must itself be structurally valid (non-empty,
	// keyed, proof-bearing) before equivalence can be meaningful.
	return validateMembers(req.MemberProofs)
}

// membersEquivalent reports whether a representation's member set proves semantic
// equivalence to the accepted Revision's members: identical set of semantic keys, each
// carrying the same authoritative content proof (algorithm AND digest). The algorithm
// is identity-bearing here to stay consistent with SORI-I1M's revision fingerprint,
// which also treats ContentProof.Algorithm as identity-bearing — two proofs under
// different algorithms are not the same content proof. Roles and presentation are not
// part of content equivalence.
//
// Precondition: revMembers has unique semantic keys (enforced by validateMembers at
// acceptance) and repMembers likewise (enforced by validateAttachRequest before this
// is called); the len + matched-count check relies on that uniqueness.
func membersEquivalent(repMembers, revMembers []Member) bool {
	if len(repMembers) != len(revMembers) {
		return false
	}
	type proof struct{ algorithm, digest string }
	want := make(map[string]proof, len(revMembers))
	for i := range revMembers {
		want[revMembers[i].SemanticKey] = proof{revMembers[i].Proof.Algorithm, revMembers[i].Proof.Digest}
	}
	matched := 0
	for i := range repMembers {
		p, ok := want[repMembers[i].SemanticKey]
		if !ok || p.algorithm != repMembers[i].Proof.Algorithm || p.digest != repMembers[i].Proof.Digest {
			return false
		}
		matched++
	}
	return matched == len(want)
}

// computeRepresentationFingerprint canonicalizes the representation's identity-bearing
// facts (physical format + member-equivalence proof set) into a stable internal
// comparison fingerprint. Locators/health are deliberately excluded, so a locator/tag
// or health change never alters it. Internal comparison evidence only — not a public
// Representation-ID or hash algorithm.
func computeRepresentationFingerprint(format string, members []Member) string {
	type fm struct {
		Key       string `json:"key"`
		Algorithm string `json:"algorithm"`
		Digest    string `json:"digest"`
	}
	list := make([]fm, 0, len(members))
	for i := range members {
		list = append(list, fm{Key: members[i].SemanticKey, Algorithm: members[i].Proof.Algorithm, Digest: members[i].Proof.Digest})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Key != list[j].Key {
			return list[i].Key < list[j].Key
		}
		if list[i].Algorithm != list[j].Algorithm {
			return list[i].Algorithm < list[j].Algorithm
		}
		return list[i].Digest < list[j].Digest
	})
	payload := struct {
		Format  string `json:"format"`
		Members []fm   `json:"members"`
	}{Format: format, Members: list}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// cloneRepresentation deep-copies the reference-typed fields (MemberProofs, Locators)
// so a stored/returned Representation shares no backing storage with callers.
func cloneRepresentation(r Representation) Representation {
	if len(r.MemberProofs) > 0 {
		members := make([]Member, len(r.MemberProofs))
		copy(members, r.MemberProofs)
		r.MemberProofs = members
	}
	if r.Locators != nil {
		locators := make([]Locator, len(r.Locators))
		copy(locators, r.Locators)
		r.Locators = locators
	}
	return r
}
