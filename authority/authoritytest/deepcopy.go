package authoritytest

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

// mutated is written into caller-held copies to detect aliasing of committed state.
const mutated = "mutated"

// mutateManifest rewrites every reference-typed field of m in place.
func mutateManifest(m authority.SemanticManifest) {
	if len(m.Members) > 0 {
		m.Members[0].Proof.Digest = mutated
	}
	if len(m.Provenance.InputLineage) > 0 {
		m.Provenance.InputLineage[0] = mutated
	}
	if m.Presentation != nil {
		m.Presentation["title"] = mutated
	}
}

// Neither the accept input, nor a returned Revision, nor a read copy may alias the
// committed Revision.
func deepCopyRevision(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	req := acceptReq("req-1", assetA, digestOne)
	rev, err := acceptAs(a, req)
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	want := rev
	want.Manifest = derivedManifest(digestOne)
	mutateManifest(req.Manifest)
	mutateManifest(rev.Manifest)
	read, err := getRevision(s, rev.RevisionID)
	if err != nil {
		return err
	}
	mutateManifest(read.Manifest)
	got, err := getRevision(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if err := sameRevision(got, want); err != nil {
		return fmt.Errorf("committed revision reachable through caller memory: %w", err)
	}
	return nil
}

func mutateRepresentation(r authority.Representation) {
	if len(r.MemberProofs) > 0 {
		r.MemberProofs[0].Proof.Digest = mutated
	}
	if len(r.Locators) > 0 {
		r.Locators[0].Coordinate = mutated
	}
}

// Attach input, returned/read Representations, and the locator slice passed to
// SetRepresentationLocators may not alias committed state.
func deepCopyRepresentation(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	ctx := context.Background()
	rev, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	req := attachReq("attach-1", rev, formatOne, locatorA)
	rep, err := a.AttachRepresentation(ctx, req)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	want := rep
	want.MemberProofs = []authority.Member{proofMember(digestOne)}
	req.MemberProofs[0].Proof.Digest = mutated
	req.Locators[0].Coordinate = mutated
	mutateRepresentation(rep)
	if err := checkRepresentationState(s, want, []authority.Locator{locatorA}); err != nil {
		return fmt.Errorf("after mutating attach input/result: %w", err)
	}
	locs := []authority.Locator{locatorB}
	if err := a.SetRepresentationLocators(ctx, want.RepresentationID, locs); err != nil {
		return fmt.Errorf("set locators: %w", err)
	}
	locs[0].Coordinate = mutated
	read, err := getRepresentation(s, want.RepresentationID)
	if err != nil {
		return err
	}
	mutateRepresentation(read)
	listed, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	for i := range listed {
		mutateRepresentation(listed[i])
	}
	if err := checkRepresentationState(s, want, []authority.Locator{locatorB}); err != nil {
		return fmt.Errorf("after mutating locator input/read/list copies: %w", err)
	}
	return nil
}

func checkRepresentationState(s authority.Store, want authority.Representation, locators []authority.Locator) error {
	got, err := getRepresentation(s, want.RepresentationID)
	if err != nil {
		return err
	}
	if err := sameRepresentationIdentity(got, want); err != nil {
		return err
	}
	if !reflect.DeepEqual(got.Locators, locators) {
		return fmt.Errorf("locators = %+v, want %+v", got.Locators, locators)
	}
	return nil
}

// A returned alias history slice may not alias the committed append-only history.
func deepCopyAliasHistory(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	rev, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	ev, err := bind(a, "bind-1", rev)
	if err != nil {
		return err
	}
	hist, err := s.AliasHistory(context.Background(), aliasLatest)
	if err != nil {
		return fmt.Errorf("alias history: %w", err)
	}
	if len(hist) > 0 {
		hist[0].RevisionID = mutated
		hist[0].Sequence = -1
	}
	again, err := s.AliasHistory(context.Background(), aliasLatest)
	if err != nil {
		return fmt.Errorf("alias history: %w", err)
	}
	return sameHistory(again, []authority.BindEvent{ev})
}
