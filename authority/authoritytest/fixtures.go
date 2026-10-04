package authoritytest

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/HeaInSeo/sori/authority"
)

const (
	assetA      = authority.AssetID("conformance-asset-a")
	assetB      = authority.AssetID("conformance-asset-b")
	formatOne   = "conformance-format-one"
	formatTwo   = "conformance-format-two"
	digestOne   = "conformance-digest-one"
	digestTwo   = "conformance-digest-two"
	digestThree = "conformance-digest-three"
	proofAlgo   = "sha256"
	proofAlgo2  = "conformance-other-algorithm"
	memberKey   = "m1"
	memberKey2  = "m2"
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

// twoMemberManifest is derivedManifest(digestOne) with a second member keyed
// memberKey2 whose proof is second, so a Store that persists or compares only the
// first member is observable.
func twoMemberManifest(second string) authority.SemanticManifest {
	m := derivedManifest(digestOne)
	extra := member(second)
	extra.SemanticKey = memberKey2
	m.Members = append(m.Members, extra)
	return m
}

// twoMemberProofs are the proofs matching twoMemberManifest(second), in member order.
func twoMemberProofs(second string) []authority.Member {
	extra := proofMember(second)
	extra.SemanticKey = memberKey2
	return []authority.Member{proofMember(digestOne), extra}
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

// acceptAs accepts req and checks that the returned Revision records req (see
// acceptedAs). Every first acceptance goes through it before the result becomes the
// oracle for later sameRevision checks, so a Store that consistently omits or
// corrupts Revision.RequestID or Revision.Fingerprint in both responses and reads
// cannot pass.
func acceptAs(a *authority.Authority, req authority.AcceptRequest) (authority.Revision, error) {
	rev, err := a.AcceptRevision(context.Background(), req)
	if err != nil {
		return authority.Revision{}, err
	}
	if err := acceptedAs(rev, req); err != nil {
		return authority.Revision{}, err
	}
	return rev, nil
}

// acceptedAs checks that rev identifies the publication operation that accepted it,
// records the submitted asset and manifest, and carries the fingerprint the facade
// supplied to the Store for req.
func acceptedAs(rev authority.Revision, req authority.AcceptRequest) error {
	if rev.RequestID != req.RequestID {
		return fmt.Errorf("revision %q carries RequestID %q, want %q", rev.RevisionID, rev.RequestID, req.RequestID)
	}
	if rev.AssetID != req.AssetID {
		return fmt.Errorf("revision %q carries AssetID %q, want %q", rev.RevisionID, rev.AssetID, req.AssetID)
	}
	if !sameManifest(rev.Manifest, req.Manifest) {
		return fmt.Errorf("revision %q carries manifest %+v, want the submitted %+v", rev.RevisionID, rev.Manifest, req.Manifest)
	}
	want, err := suppliedRevisionFingerprint(req)
	if err != nil {
		return err
	}
	if rev.Fingerprint != want {
		return fmt.Errorf("revision %q carries Fingerprint %q, want the supplied %q", rev.RevisionID, rev.Fingerprint, want)
	}
	return nil
}

// fingerprintProbe is a Store that only records the fingerprint the facade supplies
// to AcceptRevision or AttachRepresentation. It derives the expected fingerprint
// through the production facade, independently of the Store under test. Its
// GetRevision reports every revision as accepted for asset, so an attach reaches
// the Store; any other method panics on the nil embedded Store.
type fingerprintProbe struct {
	authority.Store
	asset       authority.AssetID
	fingerprint string
}

func (p *fingerprintProbe) AcceptRevision(_ context.Context, _ authority.AcceptRequest, fp string) (authority.Revision, error) {
	p.fingerprint = fp
	return authority.Revision{}, nil
}

func (p *fingerprintProbe) GetRevision(_ context.Context, id authority.RevisionID) (authority.Revision, bool, error) {
	return authority.Revision{RevisionID: id, AssetID: p.asset}, true, nil
}

func (p *fingerprintProbe) AttachRepresentation(_ context.Context, _ authority.AttachRequest, fp string, _ []authority.Member) (authority.Representation, error) {
	p.fingerprint = fp
	return authority.Representation{}, nil
}

// suppliedRevisionFingerprint returns the fingerprint the facade supplies to the
// Store when accepting req.
func suppliedRevisionFingerprint(req authority.AcceptRequest) (string, error) {
	p := &fingerprintProbe{}
	if _, err := authority.New(p).AcceptRevision(context.Background(), req); err != nil {
		return "", fmt.Errorf("derive fingerprint of %q: %w", req.RequestID, err)
	}
	if p.fingerprint == "" {
		return "", fmt.Errorf("derive fingerprint of %q: facade supplied none", req.RequestID)
	}
	return p.fingerprint, nil
}

// suppliedRepresentationFingerprint returns the fingerprint the facade supplies to
// the Store when attaching req.
func suppliedRepresentationFingerprint(req authority.AttachRequest) (string, error) {
	p := &fingerprintProbe{asset: req.AssetID}
	if _, err := authority.New(p).AttachRepresentation(context.Background(), req); err != nil {
		return "", fmt.Errorf("derive fingerprint of attach %q: %w", req.AttachOperationID, err)
	}
	if p.fingerprint == "" {
		return "", fmt.Errorf("derive fingerprint of attach %q: facade supplied none", req.AttachOperationID)
	}
	return p.fingerprint, nil
}

// bindAs binds through the facade and requires the returned event to record the submitted
// request before a case uses it as an oracle.
func bindAs(a *authority.Authority, req authority.BindRequest) (authority.BindEvent, error) {
	ev, err := a.BindAlias(context.Background(), req)
	if err != nil {
		return authority.BindEvent{}, err
	}
	if err := bindMatches(ev, req); err != nil {
		return authority.BindEvent{}, err
	}
	return ev, nil
}

// bindMatches checks that ev records the operation and logical binding of req.
func bindMatches(ev authority.BindEvent, req authority.BindRequest) error {
	if ev.BindRequestID != req.BindRequestID || ev.Alias != req.Alias || ev.AssetID != req.AssetID || ev.RevisionID != req.RevisionID {
		return fmt.Errorf("bind event %+v does not record request %+v", ev, req)
	}
	return nil
}

// attachAs attaches through the facade and requires the returned Representation to carry
// the submitted relation, attach identity and supplied fingerprint before a case uses
// it as an oracle.
func attachAs(a *authority.Authority, req authority.AttachRequest) (authority.Representation, error) {
	rep, err := a.AttachRepresentation(context.Background(), req)
	if err != nil {
		return authority.Representation{}, err
	}
	if err := attachMatches(rep, req); err != nil {
		return authority.Representation{}, err
	}
	return rep, nil
}

// attachMatches checks the immutable fields of rep against the attach request that created
// it, including the fingerprint the facade supplied for req. Locators and health are
// mutable availability, so they are not compared.
func attachMatches(rep authority.Representation, req authority.AttachRequest) error {
	if rep.AttachOperationID != req.AttachOperationID || rep.AssetID != req.AssetID ||
		rep.RevisionID != req.RevisionID || rep.Format != req.Format {
		return fmt.Errorf("representation %q (op %q, asset %q, revision %q, format %q) does not record attach %+v",
			rep.RepresentationID, rep.AttachOperationID, rep.AssetID, rep.RevisionID, rep.Format, req)
	}
	if !sameMemberSet(rep.MemberProofs, req.MemberProofs) {
		return fmt.Errorf("representation %q carries member proofs %+v, want the submitted %+v", rep.RepresentationID, rep.MemberProofs, req.MemberProofs)
	}
	want, err := suppliedRepresentationFingerprint(req)
	if err != nil {
		return err
	}
	if rep.Fingerprint != want {
		return fmt.Errorf("representation %q carries Fingerprint %q, want the supplied %q", rep.RepresentationID, rep.Fingerprint, want)
	}
	return nil
}

// sameManifest compares two manifests with their members compared as a set: member
// order is not identity-bearing (the revision fingerprint normalizes it).
func sameManifest(got, want authority.SemanticManifest) bool {
	if !sameMemberSet(got.Members, want.Members) {
		return false
	}
	got.Members, want.Members = nil, nil
	return reflect.DeepEqual(got, want)
}

// sameMemberSet reports whether a and b hold the same members in any order.
func sameMemberSet(a, b []authority.Member) bool {
	return reflect.DeepEqual(sortedMembers(a), sortedMembers(b))
}

func sortedMembers(in []authority.Member) []authority.Member {
	out := slices.Clone(in)
	slices.SortFunc(out, func(x, y authority.Member) int {
		return cmp.Or(cmp.Compare(x.SemanticKey, y.SemanticKey), cmp.Compare(x.Role, y.Role),
			cmp.Compare(x.Proof.Algorithm, y.Proof.Algorithm), cmp.Compare(x.Proof.Digest, y.Proof.Digest),
			cmp.Compare(x.DataFormat, y.DataFormat), cmp.Compare(x.Cardinality, y.Cardinality))
	})
	return out
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
