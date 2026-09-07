package sori_test

import (
	"context"
	"encoding/json"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"

	"github.com/HeaInSeo/sori"
)

// fetchReferrerManifest returns the pushed referrer manifest and its predecessor
// descriptor (the shape the OCI referrers API surfaces to discovery/filtering).
func fetchReferrerManifest(
	t *testing.T, ctx context.Context, store *oci.Store, subjectDigest, referrerDigest string,
) (ocispec.Manifest, ocispec.Descriptor) {
	t.Helper()
	subjectDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    godigest.Digest(subjectDigest),
	}
	preds, err := store.Predecessors(ctx, subjectDesc)
	if err != nil {
		t.Fatalf("Predecessors: %v", err)
	}
	var found ocispec.Descriptor
	for _, p := range preds {
		if p.Digest.String() == referrerDigest {
			found = p
			break
		}
	}
	if found.Digest == "" {
		t.Fatalf("referrer %q not found among %d predecessors", referrerDigest, len(preds))
	}
	rc, err := store.Fetch(ctx, found)
	if err != nil {
		t.Fatalf("Fetch referrer manifest: %v", err)
	}
	defer rc.Close()
	var m ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatalf("decode referrer manifest: %v", err)
	}
	return m, found
}

// TestReferrer_TypedTopLevelArtifactType proves the P-A invariant for every typed
// referrer produced via pushSpecReferrer: the semantic kind is recorded as the OCI
// top-level artifactType (so the referrer is discoverable by its exact semantic type,
// on both the manifest and the referrers-API predecessor descriptor), the same
// semantic value is kept in config.mediaType (dual-record for legacy migration), the
// subject binding is intact, and the config payload bytes are unchanged.
func TestReferrer_TypedTopLevelArtifactType(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		mediaType string
		push      func(*oci.Store, string, []byte) (sori.SpecReferrerResult, error)
	}{
		{"toolspec", sori.MediaTypeToolSpec, func(s *oci.Store, d string, j []byte) (sori.SpecReferrerResult, error) {
			return sori.PushToolSpecReferrer(ctx, s, d, j)
		}},
		{"dataspec", sori.MediaTypeDataSpec, func(s *oci.Store, d string, j []byte) (sori.SpecReferrerResult, error) {
			return sori.PushDataSpecReferrer(ctx, s, d, j)
		}},
		{"toolprofile", sori.MediaTypeToolProfile, func(s *oci.Store, d string, j []byte) (sori.SpecReferrerResult, error) {
			return sori.PushToolProfileReferrer(ctx, s, d, j)
		}},
		{"security", sori.MediaTypeSecurityScan, func(s *oci.Store, d string, j []byte) (sori.SpecReferrerResult, error) {
			return sori.PushSecurityReferrer(ctx, s, d, j)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := oci.New(t.TempDir())
			if err != nil {
				t.Fatalf("oci.New: %v", err)
			}
			subjectDigest := pushFakeSubject(t, ctx, store)
			specJSON, err := sori.MarshalSpec(map[string]string{"kind": tc.name})
			if err != nil {
				t.Fatalf("MarshalSpec: %v", err)
			}
			res, err := tc.push(store, subjectDigest, specJSON)
			if err != nil {
				t.Fatalf("push %s: %v", tc.name, err)
			}

			m, pred := fetchReferrerManifest(t, ctx, store, subjectDigest, res.ReferrerDigest)

			if m.ArtifactType != tc.mediaType {
				t.Fatalf("manifest.ArtifactType: got %q want semantic %q", m.ArtifactType, tc.mediaType)
			}
			if m.ArtifactType == ocispec.MediaTypeImageManifest {
				t.Fatalf("manifest.ArtifactType must not be the generic image-manifest media type")
			}
			// The referrers-API-facing descriptor carries the same typed artifactType.
			if pred.ArtifactType != tc.mediaType {
				t.Fatalf("predecessor descriptor artifactType: got %q want %q", pred.ArtifactType, tc.mediaType)
			}
			// config.mediaType keeps the same semantic value (dual-record).
			if m.Config.MediaType != tc.mediaType {
				t.Fatalf("config.MediaType: got %q want %q", m.Config.MediaType, tc.mediaType)
			}
			// subject binding unchanged.
			if m.Subject == nil || m.Subject.Digest.String() != subjectDigest {
				t.Fatalf("subject binding: got %+v want %q", m.Subject, subjectDigest)
			}
			// config payload bytes unchanged (config digest is the digest of specJSON).
			if m.Config.Digest != godigest.FromBytes(specJSON) {
				t.Fatalf("config payload bytes changed: got %q want %q", m.Config.Digest, godigest.FromBytes(specJSON))
			}
		})
	}
}

// TestReferrer_ToolSpecVsDataSpecTypedDistinct confirms distinct semantic kinds
// produce distinct top-level artifactTypes (not collapsed to a shared generic value).
func TestReferrer_ToolSpecVsDataSpecTypedDistinct(t *testing.T) {
	ctx := context.Background()
	specJSON, _ := sori.MarshalSpec(map[string]string{"k": "v"})

	toolStore, _ := oci.New(t.TempDir())
	td := pushFakeSubject(t, ctx, toolStore)
	tr, err := sori.PushToolSpecReferrer(ctx, toolStore, td, specJSON)
	if err != nil {
		t.Fatalf("PushToolSpecReferrer: %v", err)
	}
	tm, _ := fetchReferrerManifest(t, ctx, toolStore, td, tr.ReferrerDigest)

	dataStore, _ := oci.New(t.TempDir())
	dd := pushFakeSubject(t, ctx, dataStore)
	dr, err := sori.PushDataSpecReferrer(ctx, dataStore, dd, specJSON)
	if err != nil {
		t.Fatalf("PushDataSpecReferrer: %v", err)
	}
	dm, _ := fetchReferrerManifest(t, ctx, dataStore, dd, dr.ReferrerDigest)

	if tm.ArtifactType == dm.ArtifactType {
		t.Fatalf("distinct semantic kinds must have distinct top-level artifactType, both %q", tm.ArtifactType)
	}
	if tm.ArtifactType != sori.MediaTypeToolSpec || dm.ArtifactType != sori.MediaTypeDataSpec {
		t.Fatalf("unexpected typed artifactTypes: tool=%q data=%q", tm.ArtifactType, dm.ArtifactType)
	}
}
