package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

var (
	locatorA = authority.Locator{Scheme: "oci", Coordinate: "registry/a:tag"}
	locatorB = authority.Locator{Scheme: "path", Coordinate: "/replica/b"}
)

// acceptAndAttach accepts one Revision and attaches one Representation to it.
func acceptAndAttach(a *authority.Authority) (authority.Revision, authority.Representation, error) {
	rev, err := accept(a, "req-1", digestOne)
	if err != nil {
		return authority.Revision{}, authority.Representation{}, err
	}
	rep, err := a.AttachRepresentation(context.Background(), attachReq("attach-1", rev, formatOne, locatorA))
	if err != nil {
		return authority.Revision{}, authority.Representation{}, fmt.Errorf("attach: %w", err)
	}
	return rev, rep, nil
}

func listRepresentations(s authority.Store, revID authority.RevisionID) ([]authority.Representation, error) {
	reps, err := s.ListRepresentations(context.Background(), revID)
	if err != nil {
		return nil, fmt.Errorf("list representations: %w", err)
	}
	return reps, nil
}

// An attach retry returns the same Representation (one relation); the same op id
// with a different relation fails closed (reconciled BEFORE member equivalence).
func attachIdempotentAndConflict(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	ctx := context.Background()
	rev, first, err := acceptAndAttach(a)
	if err != nil {
		return err
	}
	retry, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatOne, locatorA))
	if err != nil {
		return fmt.Errorf("idempotent retry: %w", err)
	}
	if err := sameRepresentationIdentity(retry, first); err != nil {
		return fmt.Errorf("retry: %w", err)
	}
	if _, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatTwo)); !errors.Is(err, authority.ErrAttachConflict) {
		return fmt.Errorf("same op, different format: err = %v, want ErrAttachConflict", err)
	}
	mismatch := attachReq("attach-1", rev, formatOne)
	mismatch.MemberProofs = []authority.Member{proofMember(digestTwo)}
	if _, err := a.AttachRepresentation(ctx, mismatch); !errors.Is(err, authority.ErrAttachConflict) {
		return fmt.Errorf("same op, different proofs: err = %v, want ErrAttachConflict (reconcile before equivalence)", err)
	}
	reps, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 1 {
		return fmt.Errorf("relations after retry/conflict = %d, want 1", len(reps))
	}
	return sameRepresentationIdentity(reps[0], first)
}

// A new attach operation whose proofs do not match the accepted Revision is rejected
// and appends nothing.
func attachMemberEquivalence(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	rev, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	bad := attachReq("attach-bad", rev, formatOne)
	bad.MemberProofs = []authority.Member{proofMember(digestTwo)}
	if _, err := a.AttachRepresentation(context.Background(), bad); !errors.Is(err, authority.ErrMemberEquivalence) {
		return fmt.Errorf("mismatched proofs: err = %v, want ErrMemberEquivalence", err)
	}
	reps, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 0 {
		return fmt.Errorf("rejected attach left %d relations, want 0", len(reps))
	}
	return nil
}

// Multiple Representations of one Revision are listed in attach order.
func attachListOrder(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	rev, first, err := acceptAndAttach(a)
	if err != nil {
		return err
	}
	second, err := a.AttachRepresentation(context.Background(), attachReq("attach-2", rev, formatTwo))
	if err != nil {
		return fmt.Errorf("second attach: %w", err)
	}
	if second.RepresentationID == first.RepresentationID {
		return fmt.Errorf("distinct formats share representation id %q", first.RepresentationID)
	}
	reps, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 2 {
		return fmt.Errorf("relations = %d, want 2", len(reps))
	}
	if err := sameRepresentationIdentity(reps[0], first); err != nil {
		return fmt.Errorf("list[0]: %w", err)
	}
	return sameRepresentationIdentity(reps[1], second)
}

// Locator and health updates change only availability: Representation identity and
// the accepted Revision stay intact, and a later attach retry still reconciles.
func representationAvailability(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	ctx := context.Background()
	rev, rep, err := acceptAndAttach(a)
	if err != nil {
		return err
	}
	if err := a.SetRepresentationLocators(ctx, rep.RepresentationID, []authority.Locator{locatorB}); err != nil {
		return fmt.Errorf("set locators: %w", err)
	}
	if err := a.SetRepresentationHealth(ctx, rep.RepresentationID, false); err != nil {
		return fmt.Errorf("set health: %w", err)
	}
	got, err := getRepresentation(s, rep.RepresentationID)
	if err != nil {
		return err
	}
	if err := sameRepresentationIdentity(got, rep); err != nil {
		return err
	}
	if got.Healthy || !reflect.DeepEqual(got.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("availability not applied: healthy=%v locators=%+v", got.Healthy, got.Locators)
	}
	if err := checkRevisionUnchanged(s, rev); err != nil {
		return err
	}
	retry, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatOne, locatorA))
	if err != nil {
		return fmt.Errorf("attach retry after availability change: %w", err)
	}
	if err := sameRepresentationIdentity(retry, rep); err != nil {
		return fmt.Errorf("attach retry after availability change: %w", err)
	}
	return checkUnknownRepresentation(s)
}

func checkRevisionUnchanged(s authority.Store, want authority.Revision) error {
	got, err := getRevision(s, want.RevisionID)
	if err != nil {
		return err
	}
	return sameRevision(got, want)
}

func checkUnknownRepresentation(s authority.Store) error {
	ctx := context.Background()
	const unknown = authority.RepresentationID("conformance-unknown-representation")
	if err := s.SetRepresentationLocators(ctx, unknown, nil); !errors.Is(err, authority.ErrRepresentationNotFound) {
		return fmt.Errorf("locators on unknown: err = %v, want ErrRepresentationNotFound", err)
	}
	if err := s.SetRepresentationHealth(ctx, unknown, true); !errors.Is(err, authority.ErrRepresentationNotFound) {
		return fmt.Errorf("health on unknown: err = %v, want ErrRepresentationNotFound", err)
	}
	if _, ok, err := s.GetRepresentation(ctx, unknown); ok || err != nil {
		return fmt.Errorf("get unknown: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	return nil
}
