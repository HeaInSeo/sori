package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

// An accepted Revision, its RequestID reconcile and its conflict survive a reopen.
func reopenRevision(t *testing.T, h Harness) error {
	s := h.New(t)
	rev, err := accept(authority.New(s), "req-1", digestOne)
	if err != nil {
		return err
	}
	s = h.Reopen(t, s)
	a := authority.New(s)
	if err := checkRevisionUnchanged(s, rev); err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	retry, err := accept(a, "req-1", digestOne)
	if err != nil {
		return fmt.Errorf("retry after reopen: %w", err)
	}
	if err := sameRevision(retry, rev); err != nil {
		return fmt.Errorf("retry after reopen: %w", err)
	}
	if _, err := a.AcceptRevision(context.Background(), acceptReq("req-1", assetA, digestTwo)); !errors.Is(err, authority.ErrRequestConflict) {
		return fmt.Errorf("conflict after reopen: err = %v, want ErrRequestConflict", err)
	}
	next, err := accept(a, "req-2", digestOne)
	if err != nil {
		return err
	}
	if next.RevisionID == rev.RevisionID {
		return fmt.Errorf("revision id %q reused after reopen", rev.RevisionID)
	}
	return nil
}

// Alias history, bind reconcile/conflict and Sequence monotonicity survive a reopen.
func reopenAliasHistory(t *testing.T, h Harness) error {
	s := h.New(t)
	r1, r2, err := twoRevisions(authority.New(s))
	if err != nil {
		return err
	}
	events, err := bindSequence(authority.New(s), r1, r2)
	if err != nil {
		return err
	}
	s = h.Reopen(t, s)
	a := authority.New(s)
	retry, err := bind(a, "bind-2", r2)
	if err != nil {
		return fmt.Errorf("bind retry after reopen: %w", err)
	}
	if err := sameBindEvent(retry, events[1]); err != nil {
		return fmt.Errorf("bind retry after reopen: %w", err)
	}
	if _, err := bind(a, "bind-2", r1); !errors.Is(err, authority.ErrAliasBindingConflict) {
		return fmt.Errorf("bind conflict after reopen: err = %v, want ErrAliasBindingConflict", err)
	}
	next, err := bind(a, "bind-4", r2)
	if err != nil {
		return err
	}
	if next.Sequence <= events[len(events)-1].Sequence {
		return fmt.Errorf("sequence %d after reopen not greater than %d", next.Sequence, events[len(events)-1].Sequence)
	}
	hist, err := s.AliasHistory(context.Background(), aliasLatest)
	if err != nil {
		return fmt.Errorf("alias history: %w", err)
	}
	return sameHistory(hist, append(events, next))
}

// Representation identity, availability, attach reconcile/conflict and list order
// survive a reopen.
func reopenRepresentation(t *testing.T, h Harness) error {
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
	s = h.Reopen(t, s)
	a = authority.New(s)
	got, err := getRepresentation(s, rep.RepresentationID)
	if err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	if err := sameRepresentationIdentity(got, rep); err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	if got.Healthy || !reflect.DeepEqual(got.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("availability lost on reopen: healthy=%v locators=%+v", got.Healthy, got.Locators)
	}
	retry, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatOne, locatorA))
	if err != nil {
		return fmt.Errorf("attach retry after reopen: %w", err)
	}
	if err := sameRepresentationIdentity(retry, rep); err != nil {
		return fmt.Errorf("attach retry after reopen: %w", err)
	}
	if _, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatTwo)); !errors.Is(err, authority.ErrAttachConflict) {
		return fmt.Errorf("attach conflict after reopen: err = %v, want ErrAttachConflict", err)
	}
	reps, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 1 {
		return fmt.Errorf("relations after reopen = %d, want 1", len(reps))
	}
	return sameRepresentationIdentity(reps[0], rep)
}
