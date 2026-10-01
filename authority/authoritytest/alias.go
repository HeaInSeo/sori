package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

func bind(a *authority.Authority, id authority.RequestID, rev authority.Revision) (authority.BindEvent, error) {
	ev, err := a.BindAlias(context.Background(), authority.BindRequest{
		BindRequestID: id, Alias: aliasLatest, AssetID: rev.AssetID, RevisionID: rev.RevisionID,
	})
	if err != nil {
		return authority.BindEvent{}, fmt.Errorf("bind %q: %w", id, err)
	}
	return ev, nil
}

// twoRevisions accepts two distinct Revisions of assetA.
func twoRevisions(a *authority.Authority) (authority.Revision, authority.Revision, error) {
	r1, err := accept(a, "req-1", digestOne)
	if err != nil {
		return authority.Revision{}, authority.Revision{}, err
	}
	r2, err := accept(a, "req-2", digestTwo)
	if err != nil {
		return authority.Revision{}, authority.Revision{}, err
	}
	return r1, r2, nil
}

// bindSequence binds the alias r1 → r2 → r1 (a rebind back) and returns the events.
func bindSequence(a *authority.Authority, r1, r2 authority.Revision) ([]authority.BindEvent, error) {
	steps := []struct {
		id  authority.RequestID
		rev authority.Revision
	}{{"bind-1", r1}, {"bind-2", r2}, {"bind-3", r1}}
	events := make([]authority.BindEvent, 0, len(steps))
	for _, st := range steps {
		ev, err := bind(a, st.id, st.rev)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// Every rebind appends; history is oldest-first with strictly increasing Sequence and
// rebinding back to an earlier Revision is a new event, not a rewrite.
func bindAppendOnlyHistory(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	r1, r2, err := twoRevisions(a)
	if err != nil {
		return err
	}
	events, err := bindSequence(a, r1, r2)
	if err != nil {
		return err
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence <= events[i-1].Sequence {
			return fmt.Errorf("sequence not strictly increasing: %d then %d", events[i-1].Sequence, events[i].Sequence)
		}
	}
	hist, err := s.AliasHistory(context.Background(), aliasLatest)
	if err != nil {
		return fmt.Errorf("alias history: %w", err)
	}
	if err := sameHistory(hist, events); err != nil {
		return err
	}
	empty, err := s.AliasHistory(context.Background(), "conformance-unbound-alias")
	if err != nil || len(empty) != 0 {
		return fmt.Errorf("unbound alias history = %+v, err=%v; want empty", empty, err)
	}
	return nil
}

// An ack-loss retry returns the original event without a duplicate; the same
// BindRequestID for a different binding, an unknown Revision and a cross-asset
// Revision all fail closed without appending.
func bindIdempotentAndConflict(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	ctx := context.Background()
	r1, r2, err := twoRevisions(a)
	if err != nil {
		return err
	}
	first, err := bind(a, "bind-1", r1)
	if err != nil {
		return err
	}
	retry, err := bind(a, "bind-1", r1)
	if err != nil {
		return fmt.Errorf("idempotent retry: %w", err)
	}
	if err := sameBindEvent(retry, first); err != nil {
		return fmt.Errorf("retry: %w", err)
	}
	if _, err := bind(a, "bind-1", r2); !errors.Is(err, authority.ErrAliasBindingConflict) {
		return fmt.Errorf("same bind id, different revision: err = %v, want ErrAliasBindingConflict", err)
	}
	unknown := authority.BindRequest{BindRequestID: "bind-unknown", Alias: aliasLatest, AssetID: assetA, RevisionID: "conformance-unknown"}
	if _, err := a.BindAlias(ctx, unknown); !errors.Is(err, authority.ErrRevisionNotFound) {
		return fmt.Errorf("unknown revision: err = %v, want ErrRevisionNotFound", err)
	}
	cross := authority.BindRequest{BindRequestID: "bind-cross", Alias: aliasLatest, AssetID: assetB, RevisionID: r1.RevisionID}
	if _, err := a.BindAlias(ctx, cross); !errors.Is(err, authority.ErrAliasBindingConflict) {
		return fmt.Errorf("cross-asset binding: err = %v, want ErrAliasBindingConflict", err)
	}
	hist, err := s.AliasHistory(ctx, aliasLatest)
	if err != nil {
		return fmt.Errorf("alias history: %w", err)
	}
	return sameHistory(hist, []authority.BindEvent{first})
}

// Concurrent retries of one BindRequestID append exactly one history event.
func bindConcurrentSameRequest(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	rev, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	events := make([]authority.BindEvent, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			events[i], errs[i] = bind(a, "bind-race", rev)
		}(i)
	}
	wg.Wait()
	for i := range concurrency {
		if errs[i] != nil {
			return fmt.Errorf("concurrent bind %d: %w", i, errs[i])
		}
		if err := sameBindEvent(events[i], events[0]); err != nil {
			return fmt.Errorf("concurrent bind %d diverged: %w", i, err)
		}
	}
	hist, err := s.AliasHistory(context.Background(), aliasLatest)
	if err != nil {
		return fmt.Errorf("alias history: %w", err)
	}
	return sameHistory(hist, events[:1])
}
