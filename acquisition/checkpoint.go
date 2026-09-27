package acquisition

import (
	"context"
	"fmt"
	"sync"

	"github.com/opencontainers/go-digest"

	"github.com/HeaInSeo/sori/authority"
)

// Phase is the durable checkpoint phase of an acquisition operation.
type Phase string

const (
	// PhasePinned: the operation exists and its subject digest is pinned. No
	// verified staged copy is recorded yet. Safe to (re)transfer.
	PhasePinned Phase = "PINNED"
	// PhaseStaged: a staged copy matching the pinned digests has been atomically
	// published into the staging area. It is NOT an accepted Revision.
	PhaseStaged Phase = "STAGED"
	// PhaseAcceptPending: the acceptance handoff has been issued but its outcome is
	// explicitly UNKNOWN (e.g. crash or lost response). A retry reconciles through
	// the same publication RequestID and never mints a second Revision.
	PhaseAcceptPending Phase = "ACCEPT_PENDING_UNKNOWN"
	// PhaseAccepted: the authority accepted exactly one Revision for this operation.
	PhaseAccepted Phase = "ACCEPTED"
)

// Operation is the durable record of one logical acquisition operation.
type Operation struct {
	ID OperationID
	// Fingerprint is the frozen identity of the logical acquisition (see
	// operationFingerprint). It excludes endpoint/credential.
	Fingerprint      string
	AssetID          authority.AssetID
	SourceCoordinate string
	// Subject is the pinned manifest digest, fixed at operation creation, before
	// any transfer.
	Subject         digest.Digest
	ObservedVersion string
	UpstreamBuilder string
	Members         []MemberDecl
	// PublicationRequestID is the authority RequestID for the acceptance handoff,
	// minted once when the operation is created.
	PublicationRequestID authority.RequestID

	Phase Phase
	// StagedPath is the verified staged OCI layout (PhaseStaged and later).
	StagedPath string
	// StagedMembers are the members with content proofs taken from the verified
	// staged layer digests (PhaseStaged and later).
	StagedMembers []authority.Member
	RevisionID    authority.RevisionID
	// Attempts counts transfer attempts; LastError records the last failure.
	Attempts  int
	LastError string
	// Version is the optimistic-concurrency version maintained by the store.
	Version int
}

// CheckpointStore is the durable acquisition operation/checkpoint abstraction. It
// makes NO commitment to a concrete DB/service/topology.
type CheckpointStore interface {
	// Begin atomically creates the operation record for op.ID, or returns the
	// existing record. An existing record with a different Fingerprint is rejected
	// with ErrOperationConflict (never overwritten). The returned bool reports
	// whether a new record was created.
	Begin(ctx context.Context, op Operation) (Operation, bool, error)
	// Update replaces the record iff its stored Version equals op.Version and
	// returns the stored record with an incremented Version; otherwise
	// ErrCheckpointStale. Identity fields (ID, Fingerprint, Subject,
	// PublicationRequestID) must not change; a store rejects such an update.
	Update(ctx context.Context, op Operation) (Operation, error)
	// Get returns the record for id.
	Get(ctx context.Context, id OperationID) (Operation, bool, error)
}

// MemoryCheckpointStore is an in-memory reference CheckpointStore. It models the
// atomic, versioned checkpoint boundary; it is not a durability technology choice.
// Sharing one instance across Acquirer instances models a process restart.
type MemoryCheckpointStore struct {
	mu  sync.Mutex
	ops map[OperationID]Operation
}

// NewMemoryCheckpointStore constructs an empty MemoryCheckpointStore.
func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{ops: make(map[OperationID]Operation)}
}

// Begin implements CheckpointStore.
func (s *MemoryCheckpointStore) Begin(_ context.Context, op Operation) (Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.ops[op.ID]; ok {
		if existing.Fingerprint != op.Fingerprint {
			return Operation{}, false, fmt.Errorf("%w: operation %q", ErrOperationConflict, op.ID)
		}
		return cloneOperation(existing), false, nil
	}
	op.Version = 1
	s.ops[op.ID] = cloneOperation(op)
	return cloneOperation(op), true, nil
}

// Update implements CheckpointStore.
func (s *MemoryCheckpointStore) Update(_ context.Context, op Operation) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.ops[op.ID]
	if !ok {
		return Operation{}, fmt.Errorf("%w: unknown operation %q", ErrCheckpointStale, op.ID)
	}
	if existing.Version != op.Version {
		return Operation{}, fmt.Errorf("%w: operation %q at version %d, update from %d", ErrCheckpointStale, op.ID, existing.Version, op.Version)
	}
	if existing.Fingerprint != op.Fingerprint || existing.Subject != op.Subject || existing.PublicationRequestID != op.PublicationRequestID {
		return Operation{}, fmt.Errorf("%w: operation %q identity fields are immutable", ErrOperationConflict, op.ID)
	}
	op.Version = existing.Version + 1
	s.ops[op.ID] = cloneOperation(op)
	return cloneOperation(op), nil
}

// Get implements CheckpointStore.
func (s *MemoryCheckpointStore) Get(_ context.Context, id OperationID) (Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return Operation{}, false, nil
	}
	return cloneOperation(op), true, nil
}

func cloneOperation(op Operation) Operation {
	op.Members = append([]MemberDecl(nil), op.Members...)
	op.StagedMembers = append([]authority.Member(nil), op.StagedMembers...)
	return op
}
