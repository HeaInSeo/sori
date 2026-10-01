package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

// Suite self-checks: each deliberately broken Store must be rejected by the named
// case, so a regression in the suite itself (a check that silently passes) is caught.

func findCase(t *testing.T, name string) conformanceCase {
	t.Helper()
	for _, c := range append(append([]conformanceCase{}, contractCases...), reopenCases...) {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no conformance case %q", name)
	return conformanceCase{}
}

func memoryHarness(wrap func(*authority.MemoryStore) authority.Store) Harness {
	return Harness{New: func(*testing.T) authority.Store { return wrap(authority.NewMemoryStore()) }}
}

// nonIdempotentAccept hands out a fresh RevisionID on every retry.
type nonIdempotentAccept struct {
	*authority.MemoryStore
	calls int
}

func (s *nonIdempotentAccept) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fp string) (authority.Revision, error) {
	rev, err := s.MemoryStore.AcceptRevision(ctx, req, fp)
	s.calls++
	rev.RevisionID = authority.RevisionID(fmt.Sprintf("%s-%d", rev.RevisionID, s.calls))
	return rev, err
}

// conflictSwallowingAccept reports success instead of ErrRequestConflict.
type conflictSwallowingAccept struct{ *authority.MemoryStore }

func (s conflictSwallowingAccept) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fp string) (authority.Revision, error) {
	rev, err := s.MemoryStore.AcceptRevision(ctx, req, fp)
	if errors.Is(err, authority.ErrRequestConflict) {
		return authority.Revision{RequestID: req.RequestID, AssetID: req.AssetID, Fingerprint: fp}, nil
	}
	return rev, err
}

// duplicatingBind treats every bind retry as a new operation.
type duplicatingBind struct {
	*authority.MemoryStore
	mu    sync.Mutex
	calls int
}

func (s *duplicatingBind) BindAlias(ctx context.Context, req authority.BindRequest) (authority.BindEvent, error) {
	s.mu.Lock()
	s.calls++
	req.BindRequestID = authority.RequestID(fmt.Sprintf("%s-%d", req.BindRequestID, s.calls))
	s.mu.Unlock()
	return s.MemoryStore.BindAlias(ctx, req)
}

// sharedRead returns one cached, shared copy on every read.
type sharedRead struct {
	*authority.MemoryStore
	revs map[authority.RevisionID]authority.Revision
	hist map[string][]authority.BindEvent
}

func newSharedRead(m *authority.MemoryStore) authority.Store {
	return &sharedRead{MemoryStore: m, revs: map[authority.RevisionID]authority.Revision{}, hist: map[string][]authority.BindEvent{}}
}

func (s *sharedRead) GetRevision(ctx context.Context, id authority.RevisionID) (authority.Revision, bool, error) {
	if rev, ok := s.revs[id]; ok {
		return rev, true, nil
	}
	rev, ok, err := s.MemoryStore.GetRevision(ctx, id)
	if ok {
		s.revs[id] = rev
	}
	return rev, ok, err
}

func (s *sharedRead) AliasHistory(ctx context.Context, alias string) ([]authority.BindEvent, error) {
	if h, ok := s.hist[alias]; ok {
		return h, nil
	}
	h, err := s.MemoryStore.AliasHistory(ctx, alias)
	s.hist[alias] = h
	return h, err
}

// noopHealth acknowledges health updates without applying them.
type noopHealth struct{ *authority.MemoryStore }

func (noopHealth) SetRepresentationHealth(context.Context, authority.RepresentationID, bool) error {
	return nil
}

func TestSuiteRejectsBrokenStores(t *testing.T) {
	cases := []struct {
		mutant  string
		harness Harness
		rejects []string
	}{
		{
			mutant:  "non-idempotent accept",
			harness: memoryHarness(func(m *authority.MemoryStore) authority.Store { return &nonIdempotentAccept{MemoryStore: m} }),
			rejects: []string{"AcceptRevision/IdempotentRetry"},
		},
		{
			mutant:  "conflict swallowed",
			harness: memoryHarness(func(m *authority.MemoryStore) authority.Store { return conflictSwallowingAccept{m} }),
			rejects: []string{"AcceptRevision/RequestConflict", "AcceptRevision/ConcurrentSameRequestIDDifferentContent"},
		},
		{
			mutant:  "duplicating bind",
			harness: memoryHarness(func(m *authority.MemoryStore) authority.Store { return &duplicatingBind{MemoryStore: m} }),
			rejects: []string{"BindAlias/IdempotentRetryAndConflict", "BindAlias/ConcurrentSameBindRequestID"},
		},
		{
			mutant:  "shared read copies",
			harness: memoryHarness(newSharedRead),
			rejects: []string{"DeepCopy/Revision", "DeepCopy/AliasHistory"},
		},
		{
			mutant:  "health update dropped",
			harness: memoryHarness(func(m *authority.MemoryStore) authority.Store { return noopHealth{m} }),
			rejects: []string{"Representation/AvailabilityPreservesIdentity"},
		},
		{
			mutant: "reopen forgets state",
			harness: Harness{
				New:    func(*testing.T) authority.Store { return authority.NewMemoryStore() },
				Reopen: func(*testing.T, authority.Store) authority.Store { return authority.NewMemoryStore() },
			},
			rejects: []string{"Reopen/Revision", "Reopen/AliasHistory", "Reopen/Representation"},
		},
	}
	for _, tc := range cases {
		for _, name := range tc.rejects {
			t.Run(tc.mutant+"/"+name, func(t *testing.T) {
				if err := findCase(t, name).run(t, tc.harness); err == nil {
					t.Fatalf("case %q accepted broken store %q", name, tc.mutant)
				}
			})
		}
	}
}

// The Reopen cases must accept a Store whose Reopen genuinely preserves state. Here
// "reopen" hands back the same MemoryStore instance: this proves the cases are
// self-consistent, NOT that MemoryStore is durable.
func TestReopenCasesAcceptStatePreservingReopen(t *testing.T) {
	h := Harness{
		New:    func(*testing.T) authority.Store { return authority.NewMemoryStore() },
		Reopen: func(_ *testing.T, s authority.Store) authority.Store { return s },
	}
	for _, c := range reopenCases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(t, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}
