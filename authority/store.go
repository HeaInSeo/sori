package authority

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Store is the durable authority-store abstraction (SORI-I1M §3). It owns the atomic
// acceptance/reconcile transaction and the append-only alias history. The interface
// makes NO commitment to a concrete DB/service/topology (§8); MemoryStore is a
// reference implementation and the acceptance tests exercise the contract through it.
//
// Durability/atomicity contract: AcceptRevision either exposes no accepted Revision
// or one complete accepted Revision — never a partial one — and a successful result
// is observable only after the durable commit.
type Store interface {
	// AcceptRevision atomically reconciles by RequestID and commits:
	//   - existing RequestID + same asset + same fingerprint → the existing Revision;
	//   - existing RequestID + different asset/fingerprint → ErrRequestConflict;
	//   - new RequestID → assign a RevisionID, commit atomically, return the Revision.
	AcceptRevision(ctx context.Context, req AcceptRequest, fingerprint string) (Revision, error)
	// BindAlias atomically appends an operation-identified alias→Revision binding:
	//   - existing BindRequestID + same logical binding → the existing event (no dup);
	//   - existing BindRequestID + different binding → ErrAliasBindingConflict;
	//   - new BindRequestID → append a new history event.
	BindAlias(ctx context.Context, req BindRequest) (BindEvent, error)
	// GetRevision returns an accepted Revision by id.
	GetRevision(ctx context.Context, id RevisionID) (Revision, bool, error)
	// AliasHistory returns the append-only binding history for an alias, oldest first.
	AliasHistory(ctx context.Context, alias string) ([]BindEvent, error)
}

// MemoryStore is an in-memory reference Store. It is not itself a durability
// technology commitment; it models the atomic commit boundary the contract requires.
type MemoryStore struct {
	mu            sync.Mutex
	revisions     map[RevisionID]Revision
	byRequest     map[RequestID]RevisionID
	bindByRequest map[RequestID]BindEvent
	aliasHistory  map[string][]BindEvent
	revSeq        int
	bindSeq       int

	// beforeCommit, when non-nil, runs just before the atomic AcceptRevision commit.
	// A non-nil error simulates a crash at the commit boundary: it returns without
	// mutating any visible state, so a half-accepted Revision can never be observed.
	// Test-only.
	beforeCommit func() error

	// now supplies the acceptance/binding timestamp; overridable in tests.
	now func() time.Time
}

// NewMemoryStore constructs an empty reference Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		revisions:     make(map[RevisionID]Revision),
		byRequest:     make(map[RequestID]RevisionID),
		bindByRequest: make(map[RequestID]BindEvent),
		aliasHistory:  make(map[string][]BindEvent),
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// AcceptRevision implements Store.
func (s *MemoryStore) AcceptRevision(_ context.Context, req AcceptRequest, fingerprint string) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existingID, ok := s.byRequest[req.RequestID]; ok {
		existing := s.revisions[existingID]
		if existing.AssetID == req.AssetID && existing.Fingerprint == fingerprint {
			return existing, nil // idempotent reconcile: same request, same content
		}
		return Revision{}, fmt.Errorf("%w: request %q", ErrRequestConflict, req.RequestID)
	}

	// Simulate a crash at the commit boundary: nothing below has mutated state yet.
	if s.beforeCommit != nil {
		if err := s.beforeCommit(); err != nil {
			return Revision{}, err
		}
	}

	s.revSeq++
	rev := Revision{
		RevisionID:  RevisionID(fmt.Sprintf("sori-rev-%d", s.revSeq)),
		AssetID:     req.AssetID,
		RequestID:   req.RequestID,
		Fingerprint: fingerprint,
		Manifest:    req.Manifest,
		AcceptedAt:  s.now(),
	}
	// Atomic commit: both writes happen together under the lock, so the Revision is
	// observable only as a complete accepted unit.
	s.revisions[rev.RevisionID] = rev
	s.byRequest[req.RequestID] = rev.RevisionID
	return rev, nil
}

// BindAlias implements Store.
func (s *MemoryStore) BindAlias(_ context.Context, req BindRequest) (BindEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if prior, ok := s.bindByRequest[req.BindRequestID]; ok {
		if prior.Alias == req.Alias && prior.AssetID == req.AssetID && prior.RevisionID == req.RevisionID {
			return prior, nil // idempotent: same operation id, same binding, no dup
		}
		return BindEvent{}, fmt.Errorf("%w: bind request %q", ErrAliasBindingConflict, req.BindRequestID)
	}
	if _, ok := s.revisions[req.RevisionID]; !ok {
		return BindEvent{}, fmt.Errorf("%w: %q", ErrRevisionNotFound, req.RevisionID)
	}

	s.bindSeq++
	ev := BindEvent{
		BindRequestID: req.BindRequestID,
		Alias:         req.Alias,
		AssetID:       req.AssetID,
		RevisionID:    req.RevisionID,
		Sequence:      s.bindSeq,
		BoundAt:       s.now(),
	}
	s.bindByRequest[req.BindRequestID] = ev
	s.aliasHistory[req.Alias] = append(s.aliasHistory[req.Alias], ev)
	return ev, nil
}

// GetRevision implements Store.
func (s *MemoryStore) GetRevision(_ context.Context, id RevisionID) (Revision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rev, ok := s.revisions[id]
	return rev, ok, nil
}

// AliasHistory implements Store.
func (s *MemoryStore) AliasHistory(_ context.Context, alias string) ([]BindEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hist := s.aliasHistory[alias]
	out := make([]BindEvent, len(hist))
	copy(out, hist)
	return out, nil
}
