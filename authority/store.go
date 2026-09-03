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
			return cloneRevision(existing), nil // idempotent reconcile: same request, same content
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
		// Deep-copy the manifest so the accepted Revision's identity-bearing member
		// set / provenance is isolated from the caller's input: mutating the request
		// after acceptance must not rewrite committed authority truth (SORI-I1M §8).
		Manifest:   cloneManifest(req.Manifest),
		AcceptedAt: s.now(),
	}
	// Atomic commit: both writes happen together under the lock, so the Revision is
	// observable only as a complete accepted unit.
	s.revisions[rev.RevisionID] = rev
	s.byRequest[req.RequestID] = rev.RevisionID
	// Return an independent copy so a caller mutating the result cannot reach the
	// committed record either.
	return cloneRevision(rev), nil
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
	target, ok := s.revisions[req.RevisionID]
	if !ok {
		return BindEvent{}, fmt.Errorf("%w: %q", ErrRevisionNotFound, req.RevisionID)
	}
	if target.AssetID != req.AssetID {
		return BindEvent{}, fmt.Errorf("%w: revision %q belongs to a different asset", ErrAliasBindingConflict, req.RevisionID)
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
	if !ok {
		return Revision{}, false, nil
	}
	// Hand every reader an independent copy so a consumer mutating the returned
	// manifest cannot corrupt committed authority truth for other readers (§8).
	return cloneRevision(rev), true, nil
}

// cloneRevision returns a deep copy of rev whose reference-typed manifest fields
// (Members, Provenance.InputLineage, Presentation) share no backing storage with the
// original, so the accepted Revision is effectively immutable across callers.
func cloneRevision(rev Revision) Revision {
	rev.Manifest = cloneManifest(rev.Manifest)
	return rev
}

// cloneManifest deep-copies the reference-typed fields of a SemanticManifest.
func cloneManifest(m SemanticManifest) SemanticManifest {
	if len(m.Members) > 0 {
		members := make([]Member, len(m.Members))
		copy(members, m.Members) // Member is a value type (ContentProof is a value)
		m.Members = members
	}
	if len(m.Provenance.InputLineage) > 0 {
		lineage := make([]string, len(m.Provenance.InputLineage))
		copy(lineage, m.Provenance.InputLineage)
		m.Provenance.InputLineage = lineage
	}
	// Clone whenever the map is non-nil (not just len>0): a non-nil EMPTY map is
	// still index-mutable in place, so sharing it would leave the stored/returned
	// manifest's presentation reachable by the caller.
	if m.Presentation != nil {
		presentation := make(map[string]string, len(m.Presentation))
		for k, v := range m.Presentation {
			presentation[k] = v
		}
		m.Presentation = presentation
	}
	return m
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
