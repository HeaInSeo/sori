package authority

import (
	"context"
	"errors"
	"testing"
)

const i4aDigest = "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"

func i4aManifest() SemanticManifest {
	m := externalManifest(demoDigest)
	m.Provenance.ObservedVersion = "v1"
	m.Provenance.ObservedChecksum = i4aDigest
	return m
}

// SORI-I4A: ObservedVersion + ObservedChecksum are REQUIRED (and the checksum must be
// a well-formed digest) only for I4A-produced ExternalImport requests.
func TestI4A_ProfileRequiresObservedVersionAndChecksum(t *testing.T) {
	cases := map[string]func(*AcceptRequest){
		"missing observed version":  func(r *AcceptRequest) { r.Manifest.Provenance.ObservedVersion = "" },
		"missing observed checksum": func(r *AcceptRequest) { r.Manifest.Provenance.ObservedChecksum = "" },
		"malformed checksum":        func(r *AcceptRequest) { r.Manifest.Provenance.ObservedChecksum = "sha256:zzzz" },
		"mutable tag as checksum":   func(r *AcceptRequest) { r.Manifest.Provenance.ObservedChecksum = "latest" },
		"non external origin": func(r *AcceptRequest) {
			r.Manifest = derivedManifest("builder", demoDigest)
		},
		"unknown profile": func(r *AcceptRequest) { r.Profile = AcceptProfile(99) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, _ := newAuthority()
			req := AcceptRequest{RequestID: "req-i4a", AssetID: "asset-1", Manifest: i4aManifest(), Profile: ProfileI4AOCIDigest}
			mutate(&req)
			if _, err := a.AcceptRevision(context.Background(), req); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("expected ErrInvalidManifest, got %v", err)
			}
		})
	}

	a, _ := newAuthority()
	req := AcceptRequest{RequestID: "req-i4a", AssetID: "asset-1", Manifest: i4aManifest(), Profile: ProfileI4AOCIDigest}
	if _, err := a.AcceptRevision(context.Background(), req); err != nil {
		t.Fatalf("valid I4A request rejected: %v", err)
	}
}

// SORI-I4A AC6: the stricter I4A gate does not retroactively invalidate I1M
// revisions — legacy ExternalImport requests without observed version/checksum are
// still accepted and still reconcile idempotently, and the profile is not
// identity-bearing (the fingerprint is unchanged).
func TestI4A_LegacyI1MRevisionsNotRetroactivelyInvalidated(t *testing.T) {
	a, _ := newAuthority()
	ctx := context.Background()
	legacy := externalManifest(demoDigest)
	legacy.Provenance.ObservedVersion = ""
	legacy.Provenance.ObservedChecksum = ""
	req := AcceptRequest{RequestID: "req-legacy", AssetID: "asset-1", Manifest: legacy}

	rev, err := a.AcceptRevision(ctx, req)
	if err != nil {
		t.Fatalf("legacy I1M accept: %v", err)
	}
	again, err := a.AcceptRevision(ctx, req)
	if err != nil || again.RevisionID != rev.RevisionID {
		t.Fatalf("legacy reconcile: rev=%q err=%v", again.RevisionID, err)
	}
	// The existing I1M fixture with a non-digest checksum also stays valid.
	if _, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-legacy-2", AssetID: "asset-1", Manifest: externalManifest(demoDigest)}); err != nil {
		t.Fatalf("legacy I1M accept with free-form checksum: %v", err)
	}
	if got, ok, err := a.GetRevision(ctx, rev.RevisionID); err != nil || !ok || got.Fingerprint != rev.Fingerprint {
		t.Fatalf("legacy revision not readable/unchanged: ok=%v err=%v", ok, err)
	}

	m := i4aManifest()
	if computeFingerprint("asset-1", m) != computeFingerprint("asset-1", cloneManifest(m)) {
		t.Fatal("fingerprint not deterministic")
	}
	plain, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-plain", AssetID: "asset-2", Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	profiled, err := a.AcceptRevision(ctx, AcceptRequest{RequestID: "req-profiled", AssetID: "asset-2", Manifest: m, Profile: ProfileI4AOCIDigest})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Fingerprint != profiled.Fingerprint {
		t.Fatal("acceptance profile must not be identity-bearing")
	}
}
