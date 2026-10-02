package authoritytest

import (
	"context"
	"fmt"
	"reflect"

	"github.com/HeaInSeo/sori/authority"
)

const (
	assetA      = authority.AssetID("conformance-asset-a")
	assetB      = authority.AssetID("conformance-asset-b")
	formatOne   = "conformance-format-one"
	formatTwo   = "conformance-format-two"
	digestOne   = "conformance-digest-one"
	digestTwo   = "conformance-digest-two"
	proofAlgo   = "sha256"
	memberKey   = "m1"
	memberRole  = "primary"
	aliasLatest = "latest"
	aliasOther  = "conformance-other-alias"
)

func member(digest string) authority.Member {
	return authority.Member{
		SemanticKey: memberKey,
		Role:        memberRole,
		Proof:       authority.ContentProof{Algorithm: proofAlgo, Digest: digest},
		DataFormat:  "fastq",
		Cardinality: authority.CardinalitySingle,
	}
}

// proofMember is a proof-only representation member (no DataFormat/Cardinality).
func proofMember(digest string) authority.Member {
	return authority.Member{
		SemanticKey: memberKey,
		Role:        memberRole,
		Proof:       authority.ContentProof{Algorithm: proofAlgo, Digest: digest},
	}
}

// derivedManifest exercises every reference-typed manifest field (Members,
// Provenance.InputLineage, Presentation) so deep-copy and durable round-trip
// fidelity are observable.
func derivedManifest(digest string) authority.SemanticManifest {
	return authority.SemanticManifest{
		Origin:  authority.OriginDerivedAsset,
		Members: []authority.Member{member(digest)},
		Provenance: authority.Provenance{
			BuilderIdentity:      "builder@v1",
			RuntimeImageIdentity: "img@sha256:deadbeef",
			FrozenRecipe:         "recipe{p=1}",
			InputLineage:         []string{"input-a", "input-b"},
		},
		Presentation: map[string]string{"title": "conformance"},
	}
}

// externalManifest is an ExternalImport manifest that is valid under both
// ProfileUnspecified and ProfileI4AOCIDigest, so a retry can vary only the profile.
func externalManifest(digest string) authority.SemanticManifest {
	return authority.SemanticManifest{
		Origin:  authority.OriginExternalImport,
		Members: []authority.Member{member(digest)},
		Provenance: authority.Provenance{
			SourceCoordinate: "oci://registry/conformance",
			ObservedVersion:  "v1",
			ObservedChecksum: "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
		},
	}
}

func acceptReq(id authority.RequestID, asset authority.AssetID, digest string) authority.AcceptRequest {
	return authority.AcceptRequest{RequestID: id, AssetID: asset, Manifest: derivedManifest(digest)}
}

func attachReq(op authority.RequestID, rev authority.Revision, format string, locators ...authority.Locator) authority.AttachRequest {
	return authority.AttachRequest{
		AttachOperationID: op,
		AssetID:           rev.AssetID,
		RevisionID:        rev.RevisionID,
		Format:            format,
		MemberProofs:      []authority.Member{proofMember(digestOne)},
		Locators:          locators,
	}
}

// accept accepts one Revision through the Authority facade so the production
// validation + fingerprint path feeds the Store under test.
func accept(a *authority.Authority, id authority.RequestID, digest string) (authority.Revision, error) {
	rev, err := acceptAs(a, acceptReq(id, assetA, digest))
	if err != nil {
		return authority.Revision{}, fmt.Errorf("accept %q: %w", id, err)
	}
	return rev, nil
}

// acceptAs accepts req and checks that the returned Revision carries the submitted
// RequestID. Every first acceptance goes through it before the result becomes the
// oracle for later sameRevision checks, so a Store that consistently omits or
// corrupts Revision.RequestID in both responses and reads cannot pass.
func acceptAs(a *authority.Authority, req authority.AcceptRequest) (authority.Revision, error) {
	rev, err := a.AcceptRevision(context.Background(), req)
	if err != nil {
		return authority.Revision{}, err
	}
	if err := carriesRequest(rev, req.RequestID); err != nil {
		return authority.Revision{}, err
	}
	return rev, nil
}

// carriesRequest checks that rev identifies the publication operation that accepted it.
func carriesRequest(rev authority.Revision, id authority.RequestID) error {
	if rev.RequestID != id {
		return fmt.Errorf("revision %q carries RequestID %q, want %q", rev.RevisionID, rev.RequestID, id)
	}
	return nil
}

// sameRevision compares two Revisions field by field. AcceptedAt is compared with
// time.Equal so a durable backend may drop the monotonic clock / location.
func sameRevision(got, want authority.Revision) error {
	if got.RevisionID != want.RevisionID || got.AssetID != want.AssetID || got.RequestID != want.RequestID ||
		got.Fingerprint != want.Fingerprint || !got.AcceptedAt.Equal(want.AcceptedAt) {
		return fmt.Errorf("revision identity changed: got %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(got.Manifest, want.Manifest) {
		return fmt.Errorf("revision manifest changed: got %+v, want %+v", got.Manifest, want.Manifest)
	}
	return nil
}

// sameRepresentationIdentity compares the immutable identity of a Representation
// (everything except the mutable Locators / Healthy availability facts).
func sameRepresentationIdentity(got, want authority.Representation) error {
	if got.RepresentationID != want.RepresentationID || got.RevisionID != want.RevisionID || got.AssetID != want.AssetID ||
		got.Format != want.Format || got.Fingerprint != want.Fingerprint || got.AttachOperationID != want.AttachOperationID ||
		!got.AttachedAt.Equal(want.AttachedAt) {
		return fmt.Errorf("representation identity changed: got %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(got.MemberProofs, want.MemberProofs) {
		return fmt.Errorf("representation member proofs changed: got %+v, want %+v", got.MemberProofs, want.MemberProofs)
	}
	return nil
}

func sameBindEvent(got, want authority.BindEvent) error {
	if got.BindRequestID != want.BindRequestID || got.Alias != want.Alias || got.AssetID != want.AssetID ||
		got.RevisionID != want.RevisionID || got.Sequence != want.Sequence || !got.BoundAt.Equal(want.BoundAt) {
		return fmt.Errorf("bind event changed: got %+v, want %+v", got, want)
	}
	return nil
}

func sameHistory(got, want []authority.BindEvent) error {
	if len(got) != len(want) {
		return fmt.Errorf("alias history length = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if err := sameBindEvent(got[i], want[i]); err != nil {
			return fmt.Errorf("alias history[%d]: %w", i, err)
		}
	}
	return nil
}

// getRevision fetches a Revision that must exist.
func getRevision(s authority.Store, id authority.RevisionID) (authority.Revision, error) {
	rev, ok, err := s.GetRevision(context.Background(), id)
	if err != nil {
		return authority.Revision{}, fmt.Errorf("get revision %q: %w", id, err)
	}
	if !ok {
		return authority.Revision{}, fmt.Errorf("get revision %q: not found", id)
	}
	return rev, nil
}

// getRepresentation fetches a Representation that must exist.
func getRepresentation(s authority.Store, id authority.RepresentationID) (authority.Representation, error) {
	rep, ok, err := s.GetRepresentation(context.Background(), id)
	if err != nil {
		return authority.Representation{}, fmt.Errorf("get representation %q: %w", id, err)
	}
	if !ok {
		return authority.Representation{}, fmt.Errorf("get representation %q: not found", id)
	}
	return rep, nil
}
