package authority

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// SORI-I1M §7 mandatory acceptance tests. White-box (package authority) so the
// atomic-commit fault hook (MemoryStore.beforeCommit) can be exercised.

const (
	roleData   = "primary"
	proofAlgo  = "sha256"
	demoDigest = "aaaa1111"
)

func newAuthority() (*Authority, *MemoryStore) {
	st := NewMemoryStore()
	return New(st), st
}

func member(key, digest string) Member {
	return Member{SemanticKey: key, Role: roleData, Proof: ContentProof{Algorithm: proofAlgo, Digest: digest}}
}

func externalManifest(digest string) SemanticManifest {
	return SemanticManifest{
		Origin:  OriginExternalImport,
		Members: []Member{member("m1", digest)},
		Provenance: Provenance{
			SourceCoordinate:     "s3://bucket/dataset@v1",
			ObservedVersion:      "v1",
			ObservedChecksum:     "sha256:zzzz",
			UpstreamBuilderKnown: false, // explicitly unknown upstream builder
		},
	}
}

func derivedManifest(builder, digest string) SemanticManifest {
	return SemanticManifest{
		Origin:  OriginDerivedAsset,
		Members: []Member{member("m1", digest)},
		Provenance: Provenance{
			BuilderIdentity:      builder,
			RuntimeImageIdentity: "img@sha256:deadbeef",
			FrozenRecipe:         "recipe{p=1}",
			InputLineage:         []string{"input-a", "input-b"},
		},
	}
}

// Test 1: durable commit succeeds, the response is lost, the same request retries →
// exactly the same Revision.
func TestI1M_1_RetrySameRequestReturnsSameRevision(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	req := AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest)}

	rev1, err := a.AcceptRevision(ctx, req)
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	rev2, err := a.AcceptRevision(ctx, req)
	if err != nil {
		t.Fatalf("retry accept: %v", err)
	}
	if rev1.RevisionID != rev2.RevisionID {
		t.Fatalf("retry produced a different Revision: %q vs %q", rev1.RevisionID, rev2.RevisionID)
	}
}

// Test 2: same request id + different member proof → conflict / fail-closed.
func TestI1M_2_SameRequestDifferentProofConflicts(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest("digest-A")}); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	_, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest("digest-B")})
	if !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("want ErrRequestConflict, got %v", err)
	}
}

// Test 3: same request id + same bytes but different derived builder → conflict.
func TestI1M_3_SameRequestDifferentBuilderConflicts(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: derivedManifest("builder-X", demoDigest)}); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	// Same member bytes, different builder identity → different identity-bearing
	// fingerprint → conflict.
	_, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: derivedManifest("builder-Y", demoDigest)})
	if !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("want ErrRequestConflict for a different builder, got %v", err)
	}
}

// Test 4: alias bind/rebind response loss + retry → no duplicate history; historical
// binding remains queryable.
func TestI1M_4_AliasBindingIdempotentNoDuplicateHistory(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest)})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	bind := BindRequest{BindRequestID: "bind-1", Alias: "latest", AssetID: "asset-1", RevisionID: rev.RevisionID}
	ev1, err := a.BindAlias(ctx, bind)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	ev2, err := a.BindAlias(ctx, bind) // response-loss retry, same operation id
	if err != nil {
		t.Fatalf("bind retry: %v", err)
	}
	if ev1.Sequence != ev2.Sequence {
		t.Fatalf("retry duplicated history: seq %d vs %d", ev1.Sequence, ev2.Sequence)
	}
	hist, _ := a.AliasHistory(ctx, "latest")
	if len(hist) != 1 {
		t.Fatalf("alias history len = %d, want 1 (no duplicate)", len(hist))
	}

	// A genuine rebind (new operation id) appends; the original remains queryable.
	rev2, _ := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-2", AssetID: "asset-1", Manifest: externalManifest("digest-2")})
	if _, err := a.BindAlias(ctx, BindRequest{BindRequestID: "bind-2", Alias: "latest", AssetID: "asset-1", RevisionID: rev2.RevisionID}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	hist, _ = a.AliasHistory(ctx, "latest")
	if len(hist) != 2 || hist[0].RevisionID != rev.RevisionID {
		t.Fatalf("history should append and keep the original binding, got %+v", hist)
	}

	// Same operation id re-used for a different binding → conflict.
	_, err = a.BindAlias(ctx, BindRequest{BindRequestID: "bind-1", Alias: "latest", AssetID: "asset-1", RevisionID: rev2.RevisionID})
	if !errors.Is(err, ErrAliasBindingConflict) {
		t.Fatalf("want ErrAliasBindingConflict, got %v", err)
	}
}

// Test 5: crash during authority-store commit → a half-accepted Revision is never
// observable, and the failed attempt leaves no durable trace.
func TestI1M_5_CrashDuringCommitLeavesNothingObservable(t *testing.T) {
	a, st := newAuthority()
	ctx := context.Background()
	boom := errors.New("simulated crash at commit")
	st.beforeCommit = func() error { return boom }

	req := AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest)}
	if _, err := a.AcceptRevision(ctx, req); !errors.Is(err, boom) {
		t.Fatalf("want the injected crash error, got %v", err)
	}
	// Nothing observable: no revision, and the request id was not consumed.
	if hist, _ := a.AliasHistory(ctx, "latest"); len(hist) != 0 {
		t.Fatalf("unexpected history after crash")
	}
	st.beforeCommit = nil
	rev, err := a.AcceptRevision(ctx, req)
	if err != nil {
		t.Fatalf("re-accept after crash: %v", err)
	}
	if got, ok, _ := a.GetRevision(ctx, rev.RevisionID); !ok || got.RevisionID != rev.RevisionID {
		t.Fatalf("re-accepted revision not observable")
	}
}

// Test 6: physical push success does not create authority truth — a Revision exists
// only after AcceptRevision.
func TestI1M_6_PhysicalPushDoesNotInferRevision(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	// Simulate "OCI/package push succeeded" by NOT calling AcceptRevision at all.
	if _, ok, _ := a.GetRevision(ctx, "sori-rev-1"); ok {
		t.Fatalf("a Revision existed without acceptance (push inferred authority truth)")
	}
	if hist, _ := a.AliasHistory(ctx, "latest"); len(hist) != 0 {
		t.Fatalf("alias truth existed without acceptance")
	}
}

// Test 7: acceptance succeeds with zero attached representations (there is no
// representation concept in I1M); runtime usability is out of scope and unresolved.
func TestI1M_7_AcceptSucceedsWithoutRepresentations(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest)})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	// The accepted Revision carries members + provenance but no representation
	// attachment (SORI-I1M §8 defers representations to SORI-I2R).
	if len(rev.Manifest.Members) == 0 {
		t.Fatalf("accepted revision unexpectedly has no members")
	}
	if got, ok, _ := a.GetRevision(ctx, rev.RevisionID); !ok || len(got.Manifest.Members) != 1 {
		t.Fatalf("revision not durably accepted with its members")
	}
}

// Test 8: derived origin missing required facts → reject; external import with an
// explicitly-unknown upstream builder → accept when other minimum facts are valid.
func TestI1M_8_ProvenanceByOrigin(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()

	bad := derivedManifest("", demoDigest) // missing builder identity
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-bad", AssetID: "asset-1", Manifest: bad}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("derived missing builder should be rejected, got %v", err)
	}

	// External import with UpstreamBuilderKnown=false is valid (unknown, not fabricated).
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-ok", AssetID: "asset-1", Manifest: externalManifest(demoDigest)}); err != nil {
		t.Fatalf("external import with explicit-unknown builder should accept, got %v", err)
	}

	// Fabricating a builder value while marking it unknown is rejected.
	fab := externalManifest(demoDigest)
	fab.Provenance.UpstreamBuilder = "made-up"
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-fab", AssetID: "asset-1", Manifest: fab}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("fabricated unknown builder should be rejected, got %v", err)
	}
}

// Test 9: a presentation-only change does not alter the semantic fingerprint or
// produce a conflict.
func TestI1M_9_PresentationNotIdentityBearing(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()

	base := externalManifest(demoDigest)
	base.Presentation = map[string]string{"displayName": "Original"}
	rev1, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: base})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Same request, only presentation differs → idempotent, same Revision, no conflict.
	changed := externalManifest(demoDigest)
	changed.Presentation = map[string]string{"displayName": "Renamed"}
	rev2, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: changed})
	if err != nil {
		t.Fatalf("presentation-only change caused an error: %v", err)
	}
	if rev1.RevisionID != rev2.RevisionID {
		t.Fatalf("presentation change altered the Revision: %q vs %q", rev1.RevisionID, rev2.RevisionID)
	}
	if computeFingerprint("asset-1", base) != computeFingerprint("asset-1", changed) {
		t.Fatalf("presentation change altered the identity-bearing fingerprint")
	}
}

// Test 10: packaging/presentation values (a member content digest, a manifest digest,
// a StableRef/tag/path) cannot substitute for Revision identity/acceptance authority.
func TestI1M_10_PackagingCannotSubstituteForAuthority(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest)})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	// The immutable Revision identity is not the member content-proof digest, nor the
	// internal fingerprint, nor any packaging value — authority truth comes only from
	// acceptance.
	if string(rev.RevisionID) == demoDigest || string(rev.RevisionID) == rev.Fingerprint {
		t.Fatalf("Revision identity was derived from a packaging/content value")
	}
	// A digest that never went through acceptance is not a Revision.
	if _, ok, _ := a.GetRevision(ctx, RevisionID(demoDigest)); ok {
		t.Fatalf("a content/manifest digest resolved as a Revision without acceptance")
	}
	_ = fmt.Sprint(rev)
}

// Immutability regression (SORI-I1M §8): an accepted Revision's identity-bearing
// member set / provenance / presentation must not be mutable through the caller's
// input slices/maps or through a value returned by GetRevision.
func TestI1M_AcceptedRevisionIsImmutable(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	m := derivedManifest("builder-X", "orig-digest")
	m.Presentation = map[string]string{"displayName": "Orig"}
	req := AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: m}

	rev, err := a.AcceptRevision(ctx, req)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// (a) Mutate the caller's input AFTER acceptance.
	m.Members[0].Proof.Digest = "TAMPERED"
	m.Members[0].Role = "TAMPERED"
	m.Provenance.InputLineage[0] = "TAMPERED"
	m.Presentation["displayName"] = "TAMPERED"

	// (b) Mutate a value returned from GetRevision.
	got, ok, _ := a.GetRevision(ctx, rev.RevisionID)
	if !ok {
		t.Fatalf("revision missing")
	}
	got.Manifest.Members[0].Proof.Digest = "TAMPERED-VIA-GET"
	got.Manifest.Provenance.InputLineage[0] = "TAMPERED-VIA-GET"

	// The stored authority truth must be unchanged.
	fresh, _, _ := a.GetRevision(ctx, rev.RevisionID)
	if fresh.Manifest.Members[0].Proof.Digest != "orig-digest" || fresh.Manifest.Members[0].Role != roleData {
		t.Fatalf("stored member set was mutated: %+v", fresh.Manifest.Members[0])
	}
	if fresh.Manifest.Provenance.InputLineage[0] != "input-a" {
		t.Fatalf("stored input lineage was mutated: %v", fresh.Manifest.Provenance.InputLineage)
	}
	if fresh.Manifest.Presentation["displayName"] != "Orig" {
		t.Fatalf("stored presentation was mutated: %v", fresh.Manifest.Presentation)
	}
	// The retry idempotency still holds despite the caller's post-accept mutation of m:
	// the accepted fingerprint was computed and frozen at acceptance time.
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: derivedManifest("builder-X", "orig-digest")}); err != nil {
		t.Fatalf("retry after caller mutation should still reconcile, got %v", err)
	}
}

// Input lineage order is identity-bearing (§5): the same request id meaning two
// order-distinct derived lineages must conflict, never silently merge.
func TestI1M_InputLineageOrderIsIdentityBearing(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	ab := derivedManifest("builder-X", demoDigest)
	ab.Provenance.InputLineage = []string{"A", "B"}
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: ab}); err != nil {
		t.Fatalf("accept [A,B]: %v", err)
	}
	ba := derivedManifest("builder-X", demoDigest)
	ba.Provenance.InputLineage = []string{"B", "A"}
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: ba}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("order-distinct lineage under same request id must conflict, got %v", err)
	}
}

// BindAlias must reject binding an alias to a Revision owned by a different asset.
func TestI1M_BindAliasRejectsCrossAsset(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest)})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	_, err = a.BindAlias(ctx, BindRequest{BindRequestID: "bind-x", Alias: "latest", AssetID: "asset-2", RevisionID: rev.RevisionID})
	if !errors.Is(err, ErrAliasBindingConflict) {
		t.Fatalf("cross-asset alias binding must be rejected, got %v", err)
	}
}

// A non-nil EMPTY Presentation map must also be isolated: index-assignment mutates a
// map in place, so a shared empty map would let the caller reach stored truth.
func TestI1M_EmptyPresentationMapIsIsolated(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	m := externalManifest(demoDigest)
	m.Presentation = map[string]string{} // non-nil, empty
	rev, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: m})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	m.Presentation["x"] = "tampered" // would write into a shared stored map
	got, _, _ := a.GetRevision(ctx, rev.RevisionID)
	if _, tampered := got.Manifest.Presentation["x"]; tampered {
		t.Fatalf("stored presentation was mutated via a shared empty map")
	}
}
