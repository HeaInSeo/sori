package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

var (
	locatorA = authority.Locator{Scheme: "oci", Coordinate: "registry/a:tag"}
	locatorB = authority.Locator{Scheme: "path", Coordinate: "/replica/b"}
	locatorC = authority.Locator{Scheme: "oci", Coordinate: "registry/c:tag"}
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

// An attach retry returns the same Representation (one relation) even when its
// locators differ, since locators are mutable and not identity-bearing; the same op
// id with a different relation fails closed (reconciled BEFORE member equivalence).
func attachIdempotentAndConflict(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	ctx := context.Background()
	rev, first, err := acceptAndAttach(a)
	if err != nil {
		return err
	}
	retry, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatOne, locatorB))
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

// The same AttachOperationID reused on a different Revision is a different relation
// even when format and proofs (and therefore the representation fingerprint) are
// identical: it fails closed and the second Revision stays representation-empty.
func attachCrossRevisionConflict(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	rev1, first, err := acceptAndAttach(a)
	if err != nil {
		return err
	}
	// Same members as rev1, so only the RevisionID distinguishes the two attaches.
	rev2, err := accept(a, "req-2", digestOne)
	if err != nil {
		return err
	}
	if rev2.RevisionID == rev1.RevisionID {
		return fmt.Errorf("distinct requests share revision id %q", rev1.RevisionID)
	}
	if _, err := a.AttachRepresentation(context.Background(), attachReq("attach-1", rev2, formatOne, locatorA)); !errors.Is(err, authority.ErrAttachConflict) {
		return fmt.Errorf("same op, different revision: err = %v, want ErrAttachConflict", err)
	}
	reps, err := listRepresentations(s, rev2.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 0 {
		return fmt.Errorf("conflicting attach left %d relations on second revision, want 0", len(reps))
	}
	reps, err = listRepresentations(s, rev1.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 1 {
		return fmt.Errorf("relations on first revision = %d, want 1", len(reps))
	}
	return sameRepresentationIdentity(reps[0], first)
}

// Concurrent retries of one AttachOperationID create exactly one Representation.
func attachConcurrentSameOperation(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	rev, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	reps := make([]authority.Representation, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reps[i], errs[i] = a.AttachRepresentation(context.Background(), attachReq("attach-race", rev, formatOne, locatorA))
		}(i)
	}
	wg.Wait()
	for i := range concurrency {
		if errs[i] != nil {
			return fmt.Errorf("concurrent attach %d: %w", i, errs[i])
		}
		if err := sameRepresentationIdentity(reps[i], reps[0]); err != nil {
			return fmt.Errorf("concurrent attach %d diverged: %w", i, err)
		}
	}
	listed, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if len(listed) != 1 {
		return fmt.Errorf("relations after concurrent attach = %d, want 1", len(listed))
	}
	return sameRepresentationIdentity(listed[0], reps[0])
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
// the accepted Revision stay intact, and a later attach retry carrying a locator
// different from the original still reconciles without overwriting the availability
// state set since the attach.
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
	retry, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatOne, locatorC))
	if err != nil {
		return fmt.Errorf("attach retry after availability change: %w", err)
	}
	if err := sameRepresentationIdentity(retry, rep); err != nil {
		return fmt.Errorf("attach retry after availability change: %w", err)
	}
	reps, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 1 {
		return fmt.Errorf("relations after attach retry = %d, want 1", len(reps))
	}
	got, err = getRepresentation(s, rep.RepresentationID)
	if err != nil {
		return err
	}
	if got.Healthy || !reflect.DeepEqual(got.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("attach retry overwrote availability: healthy=%v locators=%+v", got.Healthy, got.Locators)
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
