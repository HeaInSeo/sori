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

// presentationStrictAccept reconciles a reused RequestID against the full manifest,
// so a presentation-only retry is wrongly reported as ErrRequestConflict.
type presentationStrictAccept struct {
	*authority.MemoryStore
	mu    sync.Mutex
	prior map[authority.RequestID]map[string]string
}

func newPresentationStrictAccept(m *authority.MemoryStore) authority.Store {
	return &presentationStrictAccept{MemoryStore: m, prior: map[authority.RequestID]map[string]string{}}
}

func (s *presentationStrictAccept) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fp string) (authority.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prior[req.RequestID]; ok && !reflect.DeepEqual(p, req.Manifest.Presentation) {
		return authority.Revision{}, fmt.Errorf("%w: request %q", authority.ErrRequestConflict, req.RequestID)
	}
	rev, err := s.MemoryStore.AcceptRevision(ctx, req, fp)
	if err == nil {
		if _, ok := s.prior[req.RequestID]; !ok {
			s.prior[req.RequestID] = req.Manifest.Presentation
		}
	}
	return rev, err
}

// profileStrictAccept keeps AcceptRequest.Profile in its idempotency record, so a
// valid profile-only retry is wrongly reported as ErrRequestConflict.
type profileStrictAccept struct {
	*authority.MemoryStore
	mu    sync.Mutex
	prior map[authority.RequestID]authority.AcceptProfile
}

func newProfileStrictAccept(m *authority.MemoryStore) authority.Store {
	return &profileStrictAccept{MemoryStore: m, prior: map[authority.RequestID]authority.AcceptProfile{}}
}

func (s *profileStrictAccept) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fp string) (authority.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prior[req.RequestID]; ok && p != req.Profile {
		return authority.Revision{}, fmt.Errorf("%w: request %q", authority.ErrRequestConflict, req.RequestID)
	}
	rev, err := s.MemoryStore.AcceptRevision(ctx, req, fp)
	if err == nil {
		if _, ok := s.prior[req.RequestID]; !ok {
			s.prior[req.RequestID] = req.Profile
		}
	}
	return rev, err
}

// reopenHarness wraps each new MemoryStore and reopens to the same instance, so a
// wrapper's restored idempotency/availability behavior is what the Reopen cases see.
func reopenHarness(wrap func(*authority.MemoryStore) authority.Store) Harness {
	return Harness{
		New:    func(*testing.T) authority.Store { return wrap(authority.NewMemoryStore()) },
		Reopen: func(_ *testing.T, s authority.Store) authority.Store { return s },
	}
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

// blindBind reconciles a reused BindRequestID with a comparison that ignores one
// binding field, so a different binding is returned as an idempotent hit.
type blindBind struct {
	*authority.MemoryStore
	same  func(prior authority.BindEvent, req authority.BindRequest) bool
	mu    sync.Mutex
	prior map[authority.RequestID]authority.BindEvent
}

func newBlindBind(same func(authority.BindEvent, authority.BindRequest) bool) func(*authority.MemoryStore) authority.Store {
	return func(m *authority.MemoryStore) authority.Store {
		return &blindBind{MemoryStore: m, same: same, prior: map[authority.RequestID]authority.BindEvent{}}
	}
}

func (s *blindBind) BindAlias(ctx context.Context, req authority.BindRequest) (authority.BindEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prior[req.BindRequestID]; ok && s.same(p, req) {
		return p, nil
	}
	ev, err := s.MemoryStore.BindAlias(ctx, req)
	if err == nil {
		s.prior[req.BindRequestID] = ev
	}
	return ev, err
}

// revisionBlindAttach reconciles a reused AttachOperationID by fingerprint only,
// ignoring the RevisionID the relation is attached to. A hit returns the stored
// Representation, so only the missing RevisionID comparison is wrong.
type revisionBlindAttach struct {
	*authority.MemoryStore
	mu    sync.Mutex
	prior map[authority.RequestID]authority.Representation
}

func newRevisionBlindAttach(m *authority.MemoryStore) authority.Store {
	return &revisionBlindAttach{MemoryStore: m, prior: map[authority.RequestID]authority.Representation{}}
}

func (s *revisionBlindAttach) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, revMembers []authority.Member) (authority.Representation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prior[req.AttachOperationID]; ok && p.Fingerprint == fp {
		cur, found, err := s.GetRepresentation(ctx, p.RepresentationID)
		if err != nil || !found {
			return authority.Representation{}, fmt.Errorf("stored representation %q: found=%v err=%v", p.RepresentationID, found, err)
		}
		return cur, nil
	}
	rep, err := s.MemoryStore.AttachRepresentation(ctx, req, fp, revMembers)
	if err == nil {
		s.prior[req.AttachOperationID] = rep
	}
	return rep, err
}

// duplicatingAttach treats every attach retry as a new operation.
type duplicatingAttach struct {
	*authority.MemoryStore
	mu    sync.Mutex
	calls int
}

func (s *duplicatingAttach) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, revMembers []authority.Member) (authority.Representation, error) {
	s.mu.Lock()
	s.calls++
	req.AttachOperationID = authority.RequestID(fmt.Sprintf("%s-%d", req.AttachOperationID, s.calls))
	s.mu.Unlock()
	return s.MemoryStore.AttachRepresentation(ctx, req, fp, revMembers)
}

// locatorStrictAttach includes Locators in the attach idempotency record, so a
// retry with a different locator is wrongly reported as ErrAttachConflict.
type locatorStrictAttach struct {
	*authority.MemoryStore
	mu    sync.Mutex
	prior map[authority.RequestID][]authority.Locator
}

func newLocatorStrictAttach(m *authority.MemoryStore) authority.Store {
	return &locatorStrictAttach{MemoryStore: m, prior: map[authority.RequestID][]authority.Locator{}}
}

func (s *locatorStrictAttach) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, revMembers []authority.Member) (authority.Representation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prior[req.AttachOperationID]; ok && !reflect.DeepEqual(p, req.Locators) {
		return authority.Representation{}, fmt.Errorf("%w: attach operation %q", authority.ErrAttachConflict, req.AttachOperationID)
	}
	rep, err := s.MemoryStore.AttachRepresentation(ctx, req, fp, revMembers)
	if err == nil {
		if _, ok := s.prior[req.AttachOperationID]; !ok {
			s.prior[req.AttachOperationID] = req.Locators
		}
	}
	return rep, err
}

// availabilityResettingAttach applies an idempotent attach retry's locators and
// health over the current availability state.
type availabilityResettingAttach struct {
	*authority.MemoryStore
	mu   sync.Mutex
	seen map[authority.RequestID]bool
}

func newAvailabilityResettingAttach(m *authority.MemoryStore) authority.Store {
	return &availabilityResettingAttach{MemoryStore: m, seen: map[authority.RequestID]bool{}}
}

func (s *availabilityResettingAttach) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, revMembers []authority.Member) (authority.Representation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep, err := s.MemoryStore.AttachRepresentation(ctx, req, fp, revMembers)
	if err != nil {
		return rep, err
	}
	if s.seen[req.AttachOperationID] {
		if err := s.SetRepresentationLocators(ctx, rep.RepresentationID, req.Locators); err != nil {
			return authority.Representation{}, err
		}
		if err := s.SetRepresentationHealth(ctx, rep.RepresentationID, true); err != nil {
			return authority.Representation{}, err
		}
	}
	s.seen[req.AttachOperationID] = true
	return rep, nil
}

// requestEchoAttach stores availability correctly but answers an idempotent attach
// retry with the retry request's locators and the initial healthy state.
type requestEchoAttach struct {
	*authority.MemoryStore
	mu   sync.Mutex
	seen map[authority.RequestID]bool
}

func newRequestEchoAttach(m *authority.MemoryStore) authority.Store {
	return &requestEchoAttach{MemoryStore: m, seen: map[authority.RequestID]bool{}}
}

func (s *requestEchoAttach) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, revMembers []authority.Member) (authority.Representation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep, err := s.MemoryStore.AttachRepresentation(ctx, req, fp, revMembers)
	if err != nil {
		return rep, err
	}
	if s.seen[req.AttachOperationID] {
		rep.Locators = append([]authority.Locator(nil), req.Locators...)
		rep.Healthy = true
	}
	s.seen[req.AttachOperationID] = true
	return rep, nil
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
			mutant:  "accept reconcile compares presentation",
			harness: memoryHarness(newPresentationStrictAccept),
			rejects: []string{"AcceptRevision/IdempotentRetry"},
		},
		{
			mutant:  "duplicating bind",
			harness: memoryHarness(func(m *authority.MemoryStore) authority.Store { return &duplicatingBind{MemoryStore: m} }),
			rejects: []string{"BindAlias/IdempotentRetryAndConflict", "BindAlias/ConcurrentSameBindRequestID"},
		},
		{
			mutant: "bind reconcile ignores alias",
			harness: memoryHarness(newBlindBind(func(p authority.BindEvent, r authority.BindRequest) bool {
				return p.AssetID == r.AssetID && p.RevisionID == r.RevisionID
			})),
			rejects: []string{"BindAlias/SameBindRequestIDDifferentAliasOrAsset"},
		},
		{
			mutant: "bind reconcile ignores asset",
			harness: memoryHarness(newBlindBind(func(p authority.BindEvent, r authority.BindRequest) bool {
				return p.Alias == r.Alias && p.RevisionID == r.RevisionID
			})),
			rejects: []string{"BindAlias/SameBindRequestIDDifferentAliasOrAsset"},
		},
		{
			mutant:  "attach reconcile ignores revision",
			harness: memoryHarness(newRevisionBlindAttach),
			rejects: []string{"AttachRepresentation/SameOperationIDDifferentRevision"},
		},
		{
			mutant:  "duplicating attach",
			harness: memoryHarness(func(m *authority.MemoryStore) authority.Store { return &duplicatingAttach{MemoryStore: m} }),
			rejects: []string{"AttachRepresentation/IdempotentRetryAndConflict", "AttachRepresentation/ConcurrentSameOperationID"},
		},
		{
			mutant:  "attach reconcile compares locators",
			harness: memoryHarness(newLocatorStrictAttach),
			rejects: []string{"AttachRepresentation/IdempotentRetryAndConflict", "Representation/AvailabilityPreservesIdentity"},
		},
		{
			mutant:  "attach retry overwrites availability",
			harness: memoryHarness(newAvailabilityResettingAttach),
			rejects: []string{"Representation/AvailabilityPreservesIdentity"},
		},
		{
			mutant:  "attach retry returns request availability",
			harness: memoryHarness(newRequestEchoAttach),
			rejects: []string{"Representation/AvailabilityPreservesIdentity"},
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
		{
			mutant:  "accept reconcile compares profile",
			harness: memoryHarness(newProfileStrictAccept),
			rejects: []string{"AcceptRevision/IdempotentRetry"},
		},
		{
			mutant:  "restored accept record compares presentation",
			harness: reopenHarness(newPresentationStrictAccept),
			rejects: []string{"Reopen/Revision"},
		},
		{
			mutant:  "restored attach record compares locators",
			harness: reopenHarness(newLocatorStrictAttach),
			rejects: []string{"Reopen/Representation"},
		},
		{
			mutant:  "attach retry after reopen overwrites availability",
			harness: reopenHarness(newAvailabilityResettingAttach),
			rejects: []string{"Reopen/Representation"},
		},
		{
			mutant:  "attach retry after reopen returns request availability",
			harness: reopenHarness(newRequestEchoAttach),
			rejects: []string{"Reopen/Representation"},
		},
		{
			mutant:  "restored accept record compares profile",
			harness: reopenHarness(newProfileStrictAccept),
			rejects: []string{"Reopen/Revision"},
		},
		{
			mutant: "restored bind record ignores alias",
			harness: reopenHarness(newBlindBind(func(p authority.BindEvent, r authority.BindRequest) bool {
				return p.AssetID == r.AssetID && p.RevisionID == r.RevisionID
			})),
			rejects: []string{"Reopen/AliasHistory"},
		},
		{
			mutant: "restored bind record ignores asset",
			harness: reopenHarness(newBlindBind(func(p authority.BindEvent, r authority.BindRequest) bool {
				return p.Alias == r.Alias && p.RevisionID == r.RevisionID
			})),
			rejects: []string{"Reopen/AliasHistory"},
		},
		{
			mutant:  "restored attach record ignores revision",
			harness: reopenHarness(newRevisionBlindAttach),
			rejects: []string{"Reopen/Representation"},
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
