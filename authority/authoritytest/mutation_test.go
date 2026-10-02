package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

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

// memberDigestAccept reconciles a reused RequestID on (AssetID, member digests) only,
// ignoring the rest of the supplied fingerprint: a hit is forwarded with the prior
// fingerprint, so a provenance-only change is returned as the original Revision.
type memberDigestAccept struct {
	*authority.MemoryStore
	mu    sync.Mutex
	prior map[authority.RequestID]authority.Revision
}

func newMemberDigestAccept(m *authority.MemoryStore) authority.Store {
	return &memberDigestAccept{MemoryStore: m, prior: map[authority.RequestID]authority.Revision{}}
}

func memberDigests(m authority.SemanticManifest) []string {
	out := make([]string, 0, len(m.Members))
	for _, mem := range m.Members {
		out = append(out, mem.Proof.Digest)
	}
	return out
}

func (s *memberDigestAccept) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fp string) (authority.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prior[req.RequestID]; ok && p.AssetID == req.AssetID &&
		slices.Equal(memberDigests(p.Manifest), memberDigests(req.Manifest)) {
		fp = p.Fingerprint
	}
	rev, err := s.MemoryStore.AcceptRevision(ctx, req, fp)
	if err == nil {
		if _, ok := s.prior[req.RequestID]; !ok {
			s.prior[req.RequestID] = rev
		}
	}
	return rev, err
}

// requestIDRewritingAccept commits and reconciles correctly but reports every
// Revision, on accept and on read alike, with its RequestID rewritten. The responses
// stay self-consistent, so only a check against the submitted request catches it.
type requestIDRewritingAccept struct {
	*authority.MemoryStore
	rewrite func(authority.RequestID) authority.RequestID
}

func newRequestIDRewritingAccept(rewrite func(authority.RequestID) authority.RequestID) func(*authority.MemoryStore) authority.Store {
	return func(m *authority.MemoryStore) authority.Store {
		return requestIDRewritingAccept{MemoryStore: m, rewrite: rewrite}
	}
}

func (s requestIDRewritingAccept) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fp string) (authority.Revision, error) {
	rev, err := s.MemoryStore.AcceptRevision(ctx, req, fp)
	if err == nil {
		rev.RequestID = s.rewrite(rev.RequestID)
	}
	return rev, err
}

func (s requestIDRewritingAccept) GetRevision(ctx context.Context, id authority.RevisionID) (authority.Revision, bool, error) {
	rev, ok, err := s.MemoryStore.GetRevision(ctx, id)
	if ok {
		rev.RequestID = s.rewrite(rev.RequestID)
	}
	return rev, ok, err
}

func omitRequestID(authority.RequestID) authority.RequestID { return "" }

func corruptRequestID(id authority.RequestID) authority.RequestID { return id + "-corrupt" }

// bindEventRewriting binds and records correctly but reports every BindEvent, on bind and
// on history reads alike, rewritten. The responses stay self-consistent, so only a check
// against the submitted BindRequest catches it.
type bindEventRewriting struct {
	*authority.MemoryStore
	rewrite func(*authority.BindEvent)
}

func newBindEventRewriting(rewrite func(*authority.BindEvent)) func(*authority.MemoryStore) authority.Store {
	return func(m *authority.MemoryStore) authority.Store {
		return bindEventRewriting{MemoryStore: m, rewrite: rewrite}
	}
}

func (s bindEventRewriting) BindAlias(ctx context.Context, req authority.BindRequest) (authority.BindEvent, error) {
	ev, err := s.MemoryStore.BindAlias(ctx, req)
	if err == nil {
		s.rewrite(&ev)
	}
	return ev, err
}

func (s bindEventRewriting) AliasHistory(ctx context.Context, alias string) ([]authority.BindEvent, error) {
	evs, err := s.MemoryStore.AliasHistory(ctx, alias)
	for i := range evs {
		s.rewrite(&evs[i])
	}
	return evs, err
}

func omitBindRequestID(ev *authority.BindEvent) { ev.BindRequestID = "" }

func corruptBindAsset(ev *authority.BindEvent) { ev.AssetID += "-corrupt" }

// representationRewriting attaches and stores correctly but reports every
// Representation, on attach, get and list alike, rewritten.
type representationRewriting struct {
	*authority.MemoryStore
	rewrite func(*authority.Representation)
}

func newRepresentationRewriting(rewrite func(*authority.Representation)) func(*authority.MemoryStore) authority.Store {
	return func(m *authority.MemoryStore) authority.Store {
		return representationRewriting{MemoryStore: m, rewrite: rewrite}
	}
}

func (s representationRewriting) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, members []authority.Member) (authority.Representation, error) {
	rep, err := s.MemoryStore.AttachRepresentation(ctx, req, fp, members)
	if err == nil {
		s.rewrite(&rep)
	}
	return rep, err
}

func (s representationRewriting) GetRepresentation(ctx context.Context, id authority.RepresentationID) (authority.Representation, bool, error) {
	rep, ok, err := s.MemoryStore.GetRepresentation(ctx, id)
	if ok {
		s.rewrite(&rep)
	}
	return rep, ok, err
}

func (s representationRewriting) ListRepresentations(ctx context.Context, revID authority.RevisionID) ([]authority.Representation, error) {
	reps, err := s.MemoryStore.ListRepresentations(ctx, revID)
	for i := range reps {
		s.rewrite(&reps[i])
	}
	return reps, err
}

func corruptRepresentationAsset(rep *authority.Representation) { rep.AssetID += "-corrupt" }

func corruptRepresentationRevision(rep *authority.Representation) { rep.RevisionID += "-corrupt" }

// bindOracleCases are the cases whose first BindEvent becomes the oracle.
var bindOracleCases = []string{
	"BindAlias/AppendOnlyHistoryOrder",
	"BindAlias/IdempotentRetryAndConflict",
	"BindAlias/SameBindRequestIDDifferentAliasOrAsset",
	"BindAlias/ConcurrentSameBindRequestID",
	"BindAlias/ConcurrentSameBindRequestIDDifferentRevision",
	"DeepCopy/AliasHistory",
}

// representationOracleCases are the cases whose first Representation becomes the oracle.
var representationOracleCases = []string{
	"AttachRepresentation/IdempotentRetryAndConflict",
	"AttachRepresentation/SameOperationIDDifferentRevision",
	"AttachRepresentation/ConcurrentSameOperationID",
	"AttachRepresentation/ConcurrentSameOperationIDDifferentFormat",
	"AttachRepresentation/ListOrder",
	"Representation/AvailabilityPreservesIdentity",
	"DeepCopy/Representation",
}

// requestIDOracleCases are the cases whose first accepted Revision becomes the oracle.
var requestIDOracleCases = []string{
	"AcceptRevision/IdempotentRetry",
	"AcceptRevision/RequestConflict",
	"AcceptRevision/ConcurrentSameRequestID",
	"AcceptRevision/ConcurrentSameRequestIDDifferentContent",
	"DeepCopy/Revision",
}

// raceWindow bounds how long a gated call waits for a conflicting racer.
const raceWindow = 200 * time.Millisecond

// raceGate widens a check-then-act gap: a call for an operation id waits until a
// different request for the same id has arrived (or raceWindow passes), so two
// conflicting racers both pass the conflict check before either commits. Sequential
// callers only see a delay.
type raceGate struct {
	mu    sync.Mutex
	first map[authority.RequestID]string
	open  map[authority.RequestID]chan struct{}
}

func (g *raceGate) wait(id authority.RequestID, key string) {
	g.mu.Lock()
	if g.open == nil {
		g.first, g.open = map[authority.RequestID]string{}, map[authority.RequestID]chan struct{}{}
	}
	ch, ok := g.open[id]
	switch {
	case !ok:
		ch = make(chan struct{})
		g.open[id], g.first[id] = ch, key
	case g.first[id] != "" && g.first[id] != key:
		close(ch)
		g.first[id] = ""
	}
	g.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(raceWindow):
	}
}

// tupleKeyedBind checks a reused BindRequestID correctly when it is already committed,
// but commits under a uniqueness key on the full binding rather than the request id:
// two racing different bindings both pass the check and both are appended.
type tupleKeyedBind struct {
	*authority.MemoryStore
	gate raceGate
	mu   sync.Mutex
	done map[authority.RequestID]authority.BindEvent
}

func newTupleKeyedBind(m *authority.MemoryStore) authority.Store {
	return &tupleKeyedBind{MemoryStore: m, done: map[authority.RequestID]authority.BindEvent{}}
}

func sameBinding(p authority.BindEvent, r authority.BindRequest) bool {
	return p.Alias == r.Alias && p.AssetID == r.AssetID && p.RevisionID == r.RevisionID
}

func (s *tupleKeyedBind) BindAlias(ctx context.Context, req authority.BindRequest) (authority.BindEvent, error) {
	id := req.BindRequestID
	s.mu.Lock()
	p, ok := s.done[id]
	s.mu.Unlock()
	if ok && sameBinding(p, req) {
		return p, nil
	}
	if ok {
		return authority.BindEvent{}, fmt.Errorf("%w: bind request %q", authority.ErrAliasBindingConflict, id)
	}
	s.gate.wait(id, fmt.Sprint(req.Alias, req.AssetID, req.RevisionID))
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok = s.done[id]
	if ok && sameBinding(p, req) {
		return p, nil
	}
	inner := req
	if ok {
		inner.BindRequestID = id + "#tuple"
	}
	ev, err := s.MemoryStore.BindAlias(ctx, inner)
	if err == nil && !ok {
		s.done[id] = ev
	}
	ev.BindRequestID = id
	return ev, err
}

// tupleKeyedAttach is tupleKeyedBind for AttachRepresentation: the uniqueness key is the
// relation (RevisionID, fingerprint), not the AttachOperationID.
type tupleKeyedAttach struct {
	*authority.MemoryStore
	gate raceGate
	mu   sync.Mutex
	done map[authority.RequestID]authority.Representation
}

func newTupleKeyedAttach(m *authority.MemoryStore) authority.Store {
	return &tupleKeyedAttach{MemoryStore: m, done: map[authority.RequestID]authority.Representation{}}
}

func (s *tupleKeyedAttach) reconcile(ctx context.Context, req authority.AttachRequest, fp string) (authority.Representation, bool, error) {
	p, ok := s.done[req.AttachOperationID]
	if !ok {
		return authority.Representation{}, false, nil
	}
	if p.RevisionID != req.RevisionID || p.Fingerprint != fp {
		return authority.Representation{}, true, fmt.Errorf("%w: attach operation %q", authority.ErrAttachConflict, req.AttachOperationID)
	}
	cur, found, err := s.GetRepresentation(ctx, p.RepresentationID)
	if err != nil || !found {
		return authority.Representation{}, true, fmt.Errorf("stored representation %q: found=%v err=%v", p.RepresentationID, found, err)
	}
	return cur, true, nil
}

func (s *tupleKeyedAttach) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, revMembers []authority.Member) (authority.Representation, error) {
	id := req.AttachOperationID
	s.mu.Lock()
	rep, hit, err := s.reconcile(ctx, req, fp)
	s.mu.Unlock()
	if hit {
		return rep, err
	}
	s.gate.wait(id, fmt.Sprint(req.RevisionID, fp))
	s.mu.Lock()
	defer s.mu.Unlock()
	_, committed := s.done[id]
	if rep, hit, err := s.reconcile(ctx, req, fp); hit && err == nil {
		return rep, nil
	}
	inner := req
	if committed {
		inner.AttachOperationID = id + "#tuple"
	}
	rep, err = s.MemoryStore.AttachRepresentation(ctx, inner, fp, revMembers)
	if err == nil && !committed {
		s.done[id] = rep
	}
	rep.AttachOperationID = id
	return rep, err
}

// reopenHarness wraps each new MemoryStore and reopens to the same instance, so a
// wrapper's restored idempotency/availability behavior is what the Reopen cases see.
func reopenHarness(wrap func(*authority.MemoryStore) authority.Store) Harness {
	return Harness{
		New:    func(*testing.T) authority.Store { return wrap(authority.NewMemoryStore()) },
		Reopen: func(_ *testing.T, s authority.Store) authority.Store { return s },
	}
}

// allocatorResetOnReopen reloads every record on reopen but restores its ID
// allocators one step behind, so the first fresh Revision or Representation after
// reopen reuses the last persisted ID and overwrites that record.
type allocatorResetOnReopen struct {
	*authority.MemoryStore
	mu       sync.Mutex
	reopened bool
	revIDs   []authority.RevisionID
	repIDs   []authority.RepresentationID
	revs     map[authority.RevisionID]authority.Revision
	reps     map[authority.RepresentationID]authority.Representation
}

func allocatorResetHarness() Harness {
	return Harness{
		New: func(*testing.T) authority.Store {
			return &allocatorResetOnReopen{
				MemoryStore: authority.NewMemoryStore(),
				revs:        map[authority.RevisionID]authority.Revision{},
				reps:        map[authority.RepresentationID]authority.Representation{},
			}
		},
		Reopen: func(t *testing.T, s authority.Store) authority.Store {
			r, ok := s.(*allocatorResetOnReopen)
			if !ok {
				t.Fatalf("reopen: unexpected store %T", s)
			}
			r.mu.Lock()
			r.reopened = true
			r.mu.Unlock()
			return r
		},
	}
}

func (s *allocatorResetOnReopen) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fp string) (authority.Revision, error) {
	rev, err := s.MemoryStore.AcceptRevision(ctx, req, fp)
	if err != nil {
		return rev, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case slices.Contains(s.revIDs, rev.RevisionID):
	case !s.reopened:
		s.revIDs = append(s.revIDs, rev.RevisionID)
	default:
		rev.RevisionID = s.revIDs[len(s.revIDs)-1]
		s.revs[rev.RevisionID] = rev
	}
	return rev, nil
}

func (s *allocatorResetOnReopen) GetRevision(ctx context.Context, id authority.RevisionID) (authority.Revision, bool, error) {
	s.mu.Lock()
	rev, ok := s.revs[id]
	s.mu.Unlock()
	if ok {
		return rev, true, nil
	}
	return s.MemoryStore.GetRevision(ctx, id)
}

func (s *allocatorResetOnReopen) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fp string, revMembers []authority.Member) (authority.Representation, error) {
	rep, err := s.MemoryStore.AttachRepresentation(ctx, req, fp, revMembers)
	if err != nil {
		return rep, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case slices.Contains(s.repIDs, rep.RepresentationID):
	case !s.reopened:
		s.repIDs = append(s.repIDs, rep.RepresentationID)
	default:
		rep.RepresentationID = s.repIDs[len(s.repIDs)-1]
		s.reps[rep.RepresentationID] = rep
	}
	return rep, nil
}

func (s *allocatorResetOnReopen) GetRepresentation(ctx context.Context, id authority.RepresentationID) (authority.Representation, bool, error) {
	s.mu.Lock()
	rep, ok := s.reps[id]
	s.mu.Unlock()
	if ok {
		return rep, true, nil
	}
	return s.MemoryStore.GetRepresentation(ctx, id)
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
		{
			mutant:  "reopen restores id allocators one behind",
			harness: allocatorResetHarness(),
			rejects: []string{"Reopen/Revision", "Reopen/Representation"},
		},
		{
			mutant:  "accept reconcile compares only asset and member digests",
			harness: memoryHarness(newMemberDigestAccept),
			rejects: []string{"AcceptRevision/RequestConflict"},
		},
		{
			mutant:  "restored accept record compares only asset and member digests",
			harness: reopenHarness(newMemberDigestAccept),
			rejects: []string{"Reopen/Revision"},
		},
		{
			mutant:  "bind unique on full binding, not request id",
			harness: memoryHarness(newTupleKeyedBind),
			rejects: []string{"BindAlias/ConcurrentSameBindRequestIDDifferentRevision"},
		},
		{
			mutant:  "attach unique on relation, not operation id",
			harness: memoryHarness(newTupleKeyedAttach),
			rejects: []string{"AttachRepresentation/ConcurrentSameOperationIDDifferentFormat"},
		},
		{
			mutant:  "accept and read omit request id",
			harness: memoryHarness(newRequestIDRewritingAccept(omitRequestID)),
			rejects: requestIDOracleCases,
		},
		{
			mutant:  "accept and read corrupt request id",
			harness: memoryHarness(newRequestIDRewritingAccept(corruptRequestID)),
			rejects: requestIDOracleCases,
		},
		{
			mutant:  "accept and read omit request id across reopen",
			harness: reopenHarness(newRequestIDRewritingAccept(omitRequestID)),
			rejects: []string{"Reopen/Revision"},
		},
		{
			mutant:  "accept and read corrupt request id across reopen",
			harness: reopenHarness(newRequestIDRewritingAccept(corruptRequestID)),
			rejects: []string{"Reopen/Revision"},
		},
		{
			mutant:  "bind and history omit bind request id",
			harness: memoryHarness(newBindEventRewriting(omitBindRequestID)),
			rejects: bindOracleCases,
		},
		{
			mutant:  "bind and history rewrite asset",
			harness: memoryHarness(newBindEventRewriting(corruptBindAsset)),
			rejects: bindOracleCases,
		},
		{
			mutant:  "bind and history omit bind request id across reopen",
			harness: reopenHarness(newBindEventRewriting(omitBindRequestID)),
			rejects: []string{"Reopen/AliasHistory"},
		},
		{
			mutant:  "attach, get and list rewrite asset",
			harness: memoryHarness(newRepresentationRewriting(corruptRepresentationAsset)),
			rejects: representationOracleCases,
		},
		{
			mutant:  "attach, get and list rewrite revision",
			harness: memoryHarness(newRepresentationRewriting(corruptRepresentationRevision)),
			rejects: representationOracleCases,
		},
		{
			mutant:  "attach, get and list rewrite revision across reopen",
			harness: reopenHarness(newRepresentationRewriting(corruptRepresentationRevision)),
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
