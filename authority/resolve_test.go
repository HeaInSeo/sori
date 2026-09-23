package authority

import (
	"context"
	"errors"
	"testing"
)

// SORI-I3P exact Revision·member resolve tests.

// I3P-1: an exact (asset, revision, member) resolves to the accepted Member with its
// authoritative DataFormat and Cardinality, plus the attached representations.
func TestI3P_1_ResolvesExactMemberDeclarations(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev := acceptOneRevision(t, a)
	rep, err := a.AttachRepresentation(ctx, equivalentAttach("attach-1", rev, formatChunked))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	mem, reps, err := a.ResolveRevisionMember(ctx, rev.AssetID, rev.RevisionID, "m1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mem.SemanticKey != "m1" || mem.DataFormat != demoFormat || mem.Cardinality != CardinalitySingle {
		t.Fatalf("resolved member = %+v, want m1/%s/%s", mem, demoFormat, CardinalitySingle)
	}
	if mem.Proof.Digest != demoDigest {
		t.Fatalf("resolved proof digest = %q, want %q", mem.Proof.Digest, demoDigest)
	}
	if len(reps) != 1 || reps[0].RepresentationID != rep.RepresentationID {
		t.Fatalf("representations = %+v, want exactly %q", reps, rep.RepresentationID)
	}
}

// I3P-2: an unknown revision id fails closed with ErrRevisionNotFound.
func TestI3P_2_UnknownRevisionNotFound(t *testing.T) {
	a, _ := newAuthority()
	acceptOneRevision(t, a)
	_, _, err := a.ResolveRevisionMember(context.Background(), "asset-1", "sori-rev-unknown", "m1")
	if !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("want ErrRevisionNotFound, got %v", err)
	}
}

// I3P-3: a real revision id under a different asset fails closed with
// ErrRevisionNotFound and does not leak the other asset's member.
func TestI3P_3_CrossAssetRevisionNotFound(t *testing.T) {
	a, _ := newAuthority()
	rev := acceptOneRevision(t, a)
	mem, reps, err := a.ResolveRevisionMember(context.Background(), "asset-other", rev.RevisionID, "m1")
	if !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("want ErrRevisionNotFound, got %v", err)
	}
	if mem != (Member{}) || reps != nil {
		t.Fatalf("cross-asset resolve leaked member=%+v reps=%+v", mem, reps)
	}
}

// I3P-4: a semantic key that is not a member of the Revision fails closed with
// ErrMemberNotFound, distinct from ErrRevisionNotFound.
func TestI3P_4_MissingMemberNotFound(t *testing.T) {
	a, _ := newAuthority()
	rev := acceptOneRevision(t, a)
	_, _, err := a.ResolveRevisionMember(context.Background(), rev.AssetID, rev.RevisionID, "m-missing")
	if !errors.Is(err, ErrMemberNotFound) {
		t.Fatalf("want ErrMemberNotFound, got %v", err)
	}
	if errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("missing member must not be reported as a missing revision: %v", err)
	}
}

// I3P-5: an unhealthy representation is still returned (not filtered) and the
// resolved member is unchanged; Sori makes no eligibility decision.
func TestI3P_5_UnhealthyRepresentationNotFiltered(t *testing.T) {
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

	mem, reps, err := a.ResolveRevisionMember(ctx, rev.AssetID, rev.RevisionID, "m1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mem.DataFormat != demoFormat || mem.Cardinality != CardinalitySingle {
		t.Fatalf("health change altered member declarations: %+v", mem)
	}
	if len(reps) != 1 || reps[0].RepresentationID != rep.RepresentationID || reps[0].Healthy {
		t.Fatalf("representations = %+v, want the one unhealthy representation", reps)
	}
}

// I3P-6: after an alias is rebound to a newer Revision, resolving the originally
// pinned Revision still returns its own member declarations.
func TestI3P_6_AliasRebindKeepsPinnedRead(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	rev1 := acceptOneRevision(t, a)

	m2 := externalManifest("digest-2")
	m2.Members[0].DataFormat = "bam"
	rev2, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "accept-2", AssetID: rev1.AssetID, Manifest: m2})
	if err != nil {
		t.Fatalf("accept second revision: %v", err)
	}
	if _, err := a.BindAlias(ctx, BindRequest{BindRequestID: "bind-1", Alias: "latest", AssetID: rev1.AssetID, RevisionID: rev1.RevisionID}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := a.BindAlias(ctx, BindRequest{BindRequestID: "bind-2", Alias: "latest", AssetID: rev1.AssetID, RevisionID: rev2.RevisionID}); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	mem, _, err := a.ResolveRevisionMember(ctx, rev1.AssetID, rev1.RevisionID, "m1")
	if err != nil {
		t.Fatalf("resolve pinned revision: %v", err)
	}
	if mem.DataFormat != demoFormat || mem.Proof.Digest != demoDigest {
		t.Fatalf("pinned read followed the rebind: %+v", mem)
	}
}

// I3P-7: a Revision with zero representations still resolves its member, with an
// empty representation list and no error.
func TestI3P_7_ZeroRepresentationsStillResolves(t *testing.T) {
	a, _ := newAuthority()
	rev := acceptOneRevision(t, a)
	mem, reps, err := a.ResolveRevisionMember(context.Background(), rev.AssetID, rev.RevisionID, "m1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mem.SemanticKey != "m1" {
		t.Fatalf("resolved member = %+v, want m1", mem)
	}
	if len(reps) != 0 {
		t.Fatalf("representations = %+v, want none", reps)
	}
}

// I3P-8: acceptance requires every member to declare a DataFormat and a valid
// Cardinality; a missing or unspecified declaration is rejected fail-closed.
func TestI3P_8_AcceptanceRequiresMemberDeclarations(t *testing.T) {
	cases := map[string]func(*Member){
		"missing data format":     func(m *Member) { m.DataFormat = "" },
		"blank data format":       func(m *Member) { m.DataFormat = "  " },
		"unspecified cardinality": func(m *Member) { m.Cardinality = CardinalityUnspecified },
		"unknown cardinality":     func(m *Member) { m.Cardinality = "MANY" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, _ := newAuthority()
			m := externalManifest(demoDigest)
			mutate(&m.Members[0])
			_, err := a.AcceptRevision(context.Background(), AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: m})
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("want ErrInvalidManifest, got %v", err)
			}
		})
	}
}

// I3P-9: DataFormat and Cardinality are identity-bearing, so the same request id with
// a different declaration conflicts instead of reconciling to the prior Revision.
func TestI3P_9_DeclarationsAreIdentityBearing(t *testing.T) {
	cases := map[string]func(*Member){
		"data format": func(m *Member) { m.DataFormat = "bam" },
		"cardinality": func(m *Member) { m.Cardinality = CardinalityMultiple },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, _ := newAuthority()
			ctx := context.Background()
			if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: externalManifest(demoDigest)}); err != nil {
				t.Fatalf("first accept: %v", err)
			}
			changed := externalManifest(demoDigest)
			mutate(&changed.Members[0])
			_, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-1", AssetID: "asset-1", Manifest: changed})
			if !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("want ErrRequestConflict, got %v", err)
			}
		})
	}
}
