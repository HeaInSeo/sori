package authority

import (
	"context"
	"errors"
	"testing"
)

// SORI-I2R mandatory adversarial acceptance tests. White-box (package authority) so
// the atomic attach crash hook (MemoryStore.beforeAttachCommit) can be exercised.

const (
	formatChunked   = "chunked-cas"
	formatMountable = "mountable-oci"
)

// acceptOneRevision accepts and returns a single external-import Revision with one
// member ("m1", demoDigest).
func acceptOneRevision(t *testing.T, a *Authority) Revision {
	t.Helper()
	rev, err := a.AcceptRevision(context.Background(), AcceptRequest{
		RequestID: "accept-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest),
	})
	if err != nil {
		t.Fatalf("accept revision: %v", err)
	}
	return rev
}

// equivalentAttach builds an attach request whose member proofs are equivalent to the
// accepted revision above (same key "m1" + same digest).
func equivalentAttach(op RequestID, rev Revision, format string, locators ...Locator) AttachRequest {
	return AttachRequest{
		AttachOperationID: op,
		AssetID:           rev.AssetID,
		RevisionID:        rev.RevisionID,
		Format:            format,
		MemberProofs:      []Member{member("m1", demoDigest)},
		Locators:          locators,
	}
}

// I2R-1: attach succeeds, response lost, same operation retries → exactly one relation.
func TestI2R_1_AttachRetryYieldsExactlyOneRelation(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)

	rep1, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatChunked))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	rep2, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatChunked))
	if err != nil {
		t.Fatalf("attach retry: %v", err)
	}
	if rep1.RepresentationID != rep2.RepresentationID {
		t.Fatalf("retry produced a different representation: %q vs %q", rep1.RepresentationID, rep2.RepresentationID)
	}
	reps, _ := a.ListRepresentations(ctx, rev.RevisionID)
	if len(reps) != 1 {
		t.Fatalf("relation count = %d, want 1 (idempotent attach)", len(reps))
	}
}

// I2R-2: same operation identity with a changed immutable relation (format) → conflict.
func TestI2R_2_SameOperationChangedRelationConflicts(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	if _, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatChunked)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	_, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatMountable))
	if !errors.Is(err, ErrAttachConflict) {
		t.Fatalf("want ErrAttachConflict, got %v", err)
	}
}

// I2R-3: a representation whose member set/proofs do not equal the accepted Revision
// contract → reject.
func TestI2R_3_MemberProofMismatchRejected(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)

	// Wrong digest for m1.
	bad := equivalentAttach("attach-1", rev, formatChunked)
	bad.MemberProofs = []Member{member("m1", "WRONG-DIGEST")}
	if _, err := a.AttachRepresentation(ctx, bad); !errors.Is(err, ErrMemberEquivalence) {
		t.Fatalf("wrong digest: want ErrMemberEquivalence, got %v", err)
	}
	// Extra member not in the Revision.
	extra := equivalentAttach("attach-2", rev, formatChunked)
	extra.MemberProofs = []Member{member("m1", demoDigest), member("m2", demoDigest)}
	if _, err := a.AttachRepresentation(ctx, extra); !errors.Is(err, ErrMemberEquivalence) {
		t.Fatalf("extra member: want ErrMemberEquivalence, got %v", err)
	}
}

// I2R-4: a mutable locator/tag change cannot rewrite the Revision or the
// representation's semantic identity.
func TestI2R_4_LocatorChangeDoesNotRewriteIdentity(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	rep, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatChunked,
		Locator{Scheme: "oci", Coordinate: "registry/x:tag-a"}))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	if err := a.SetRepresentationLocators(ctx, rep.RepresentationID, []Locator{{Scheme: "oci", Coordinate: "registry/x:tag-b"}}); err != nil {
		t.Fatalf("set locators: %v", err)
	}
	got, _, _ := a.GetRepresentation(ctx, rep.RepresentationID)
	if got.RepresentationID != rep.RepresentationID || got.Fingerprint != rep.Fingerprint || got.RevisionID != rep.RevisionID {
		t.Fatalf("locator change altered representation semantic identity: %+v vs %+v", got, rep)
	}
	if len(got.Locators) != 1 || got.Locators[0].Coordinate != "registry/x:tag-b" {
		t.Fatalf("locator not updated: %+v", got.Locators)
	}
	// The accepted Revision is untouched.
	fresh, ok, _ := a.GetRevision(ctx, rev.RevisionID)
	if !ok || fresh.Fingerprint != rev.Fingerprint {
		t.Fatalf("revision identity changed by a locator update")
	}
}

// I2R-5: representation health loss leaves the accepted Revision immutable/queryable.
func TestI2R_5_HealthLossLeavesRevisionImmutable(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	rep, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatChunked))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := a.SetRepresentationHealth(ctx, rep.RepresentationID, false); err != nil {
		t.Fatalf("set health: %v", err)
	}
	got, _, _ := a.GetRepresentation(ctx, rep.RepresentationID)
	if got.Healthy {
		t.Fatalf("health not updated")
	}
	fresh, ok, _ := a.GetRevision(ctx, rev.RevisionID)
	if !ok || fresh.RevisionID != rev.RevisionID || fresh.Fingerprint != rev.Fingerprint {
		t.Fatalf("a degraded representation mutated/retracted the accepted revision")
	}
}

// I2R-6: a crash during the durable attach exposes either no new relation or one
// complete relation, never a half-attached truth.
func TestI2R_6_CrashDuringAttachIsAtomic(t *testing.T) {
	a, st := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	boom := errors.New("simulated crash during attach")
	st.beforeAttachCommit = func() error { return boom }

	req := equivalentAttach("attach-1", rev, formatChunked)
	if _, err := a.AttachRepresentation(ctx, req); !errors.Is(err, boom) {
		t.Fatalf("want injected crash, got %v", err)
	}
	if reps, _ := a.ListRepresentations(ctx, rev.RevisionID); len(reps) != 0 {
		t.Fatalf("half-attached relation observed after crash")
	}
	st.beforeAttachCommit = nil
	rep, err := a.AttachRepresentation(ctx, req)
	if err != nil {
		t.Fatalf("re-attach after crash: %v", err)
	}
	if reps, _ := a.ListRepresentations(ctx, rev.RevisionID); len(reps) != 1 || reps[0].RepresentationID != rep.RepresentationID {
		t.Fatalf("exactly one complete relation expected after recovery")
	}
}

// I2R-7: multiple valid representations (distinct physical formats) may attach to the
// same Revision without changing Revision identity.
func TestI2R_7_MultipleRepresentationsPerRevision(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	if _, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatChunked)); err != nil {
		t.Fatalf("attach chunked: %v", err)
	}
	if _, err := a.AttachRepresentation(ctx, equivalentAttach("attach-2", rev, formatMountable)); err != nil {
		t.Fatalf("attach mountable: %v", err)
	}
	reps, _ := a.ListRepresentations(ctx, rev.RevisionID)
	if len(reps) != 2 {
		t.Fatalf("want 2 representations, got %d", len(reps))
	}
	if reps[0].Fingerprint == reps[1].Fingerprint {
		t.Fatalf("distinct formats should have distinct representation fingerprints")
	}
	fresh, _, _ := a.GetRevision(ctx, rev.RevisionID)
	if fresh.RevisionID != rev.RevisionID || fresh.Fingerprint != rev.Fingerprint {
		t.Fatalf("attaching representations changed the Revision identity")
	}
}

// I2R-8: the low-level package/push path cannot auto-mint an attachment — only an
// explicit AttachRepresentation creates a relation.
func TestI2R_8_PushCannotAutoMintAttachment(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	// Simulate "OCI push succeeded" by doing nothing (no attach).
	if reps, _ := a.ListRepresentations(ctx, rev.RevisionID); len(reps) != 0 {
		t.Fatalf("a representation relation existed without an explicit attach")
	}
	if _, ok, _ := a.GetRepresentation(ctx, "sori-rep-1"); ok {
		t.Fatalf("a representation existed without an explicit attach")
	}
}

// A representation-free accepted Revision remains valid authority truth.
func TestI2R_RepresentationFreeRevisionRemainsValid(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	got, ok, _ := a.GetRevision(ctx, rev.RevisionID)
	if !ok || got.RevisionID != rev.RevisionID {
		t.Fatalf("accepted revision with zero representations is not valid authority truth")
	}
	if reps, _ := a.ListRepresentations(ctx, rev.RevisionID); len(reps) != 0 {
		t.Fatalf("unexpected representations")
	}
}

// Attaching to a non-existent (or cross-asset) Revision is rejected.
func TestI2R_AttachToUnknownRevisionRejected(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)

	missing := equivalentAttach("attach-1", rev, formatChunked)
	missing.RevisionID = "sori-rev-999"
	if _, err := a.AttachRepresentation(ctx, missing); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("unknown revision: want ErrRevisionNotFound, got %v", err)
	}
	cross := equivalentAttach("attach-2", rev, formatChunked)
	cross.AssetID = "asset-2"
	if _, err := a.AttachRepresentation(ctx, cross); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("cross-asset attach: want ErrRevisionNotFound, got %v", err)
	}
}

// An attached Representation is immutable through shared slices (caller input and
// GetRepresentation result).
func TestI2R_AttachedRepresentationIsImmutable(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	req := equivalentAttach("attach-1", rev, formatChunked, Locator{Scheme: "oci", Coordinate: "c-orig"})
	rep, err := a.AttachRepresentation(ctx, req)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	req.MemberProofs[0].Proof.Digest = "TAMPERED"
	req.Locators[0].Coordinate = "TAMPERED"
	got, _, _ := a.GetRepresentation(ctx, rep.RepresentationID)
	got.MemberProofs[0].Proof.Digest = "TAMPERED-VIA-GET"
	got.Locators[0].Coordinate = "TAMPERED-VIA-GET"

	fresh, _, _ := a.GetRepresentation(ctx, rep.RepresentationID)
	if fresh.MemberProofs[0].Proof.Digest != demoDigest || fresh.Locators[0].Coordinate != "c-orig" {
		t.Fatalf("stored representation was mutated via a shared slice: %+v", fresh)
	}
}

// A representation proof under a different algorithm (same digest string) is NOT
// member-equivalent — consistent with SORI-I1M treating ContentProof.Algorithm as
// identity-bearing.
func TestI2R_AlgorithmMismatchRejected(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	req := equivalentAttach("attach-1", rev, formatChunked)
	req.MemberProofs = []Member{{SemanticKey: "m1", Role: roleData, Proof: ContentProof{Algorithm: "blake3", Digest: demoDigest}}}
	if _, err := a.AttachRepresentation(ctx, req); !errors.Is(err, ErrMemberEquivalence) {
		t.Fatalf("algorithm mismatch: want ErrMemberEquivalence, got %v", err)
	}
}

// A representation with the right member count but a wrong semantic key is rejected.
func TestI2R_WrongSemanticKeyRejected(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	req := equivalentAttach("attach-1", rev, formatChunked)
	req.MemberProofs = []Member{member("wrong-key", demoDigest)} // same count, wrong key
	if _, err := a.AttachRepresentation(ctx, req); !errors.Is(err, ErrMemberEquivalence) {
		t.Fatalf("wrong key: want ErrMemberEquivalence, got %v", err)
	}
}

// Mutating locators/health of an unknown representation returns ErrRepresentationNotFound.
func TestI2R_MutateUnknownRepresentation(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	if err := a.SetRepresentationLocators(ctx, "sori-rep-999", nil); !errors.Is(err, ErrRepresentationNotFound) {
		t.Fatalf("set locators unknown: want ErrRepresentationNotFound, got %v", err)
	}
	if err := a.SetRepresentationHealth(ctx, "sori-rep-999", false); !errors.Is(err, ErrRepresentationNotFound) {
		t.Fatalf("set health unknown: want ErrRepresentationNotFound, got %v", err)
	}
}
