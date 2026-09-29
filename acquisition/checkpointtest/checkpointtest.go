// Package checkpointtest is a reusable conformance suite for
// acquisition.CheckpointStore implementations.
//
// It separates two tiers:
//
//   - Logical cases run against every store: Begin insert-or-get and fingerprint
//     conflict, concurrent Begin creating one record, CAS Update with Version
//     increments, stale updates that must not overwrite newer state, immutable
//     identity fields, and field-fidelity read-back.
//   - Durability cases (Harness.Reopen) run only when the store can be reopened
//     over the same durable state: all fields survive reopen, Version stays
//     monotonic across reopen, and a stale pre-reopen update is still rejected.
//     A store without Reopen, such as the in-memory reference store, reports
//     these cases as SKIP. Passing only the logical tier is never evidence of
//     durability.
//
// Cross-process guarantees (Begin uniqueness and CAS between two OS processes)
// are outside this suite: goroutines in one process cannot prove them. A
// durable adapter must add its own two-process acceptance on top of Run.
package checkpointtest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/HeaInSeo/sori/acquisition"
	"github.com/HeaInSeo/sori/authority"
)

// Harness supplies the store under test.
type Harness struct {
	// New returns an empty store. Required.
	New func(t *testing.T) acquisition.CheckpointStore
	// Reopen closes s and returns a store reopened over the same durable state.
	// Nil means the store has no durable state; durability cases are skipped.
	Reopen func(t *testing.T, s acquisition.CheckpointStore) acquisition.CheckpointStore
}

// Run executes the conformance suite against h.
func Run(t *testing.T, h Harness) {
	t.Helper()
	if h.New == nil {
		t.Fatal("checkpointtest: Harness.New is required")
	}
	t.Run("logical", func(t *testing.T) {
		t.Run("BeginCreatesThenReturnsExisting", func(t *testing.T) { beginCreatesThenReturnsExisting(t, h) })
		t.Run("BeginConflictNeverOverwrites", func(t *testing.T) { beginConflictNeverOverwrites(t, h) })
		t.Run("ConcurrentBeginCreatesOnce", func(t *testing.T) { concurrentBeginCreatesOnce(t, h) })
		t.Run("UpdateIsCAS", func(t *testing.T) { updateIsCAS(t, h) })
		t.Run("StaleUpdateDoesNotOverwrite", func(t *testing.T) { staleUpdateDoesNotOverwrite(t, h) })
		t.Run("IdentityFieldsImmutable", func(t *testing.T) { identityFieldsImmutable(t, h) })
		t.Run("FieldFidelity", func(t *testing.T) { fieldFidelity(t, h) })
		t.Run("GetUnknown", func(t *testing.T) { getUnknown(t, h) })
	})
	t.Run("durability", func(t *testing.T) {
		if h.Reopen == nil {
			t.Skip("store has no Reopen: durability is not proven by this run")
		}
		t.Run("ReopenPreservesFieldsAndVersion", func(t *testing.T) { reopenPreservesFieldsAndVersion(t, h) })
		t.Run("StalePreReopenUpdateRejected", func(t *testing.T) { stalePreReopenUpdateRejected(t, h) })
	})
}

// Operation returns a valid PINNED operation for id, as an Acquirer would Begin it.
func Operation(id acquisition.OperationID) acquisition.Operation {
	return acquisition.Operation{
		ID:               id,
		Fingerprint:      "fp-" + string(id),
		AssetID:          "asset-1",
		SourceCoordinate: "upstream:example/asset-1",
		Subject:          digest.FromString("manifest-" + string(id)),
		ObservedVersion:  "v1",
		Members: []acquisition.MemberDecl{
			{SemanticKey: "a.fa", Role: "primary", DataFormat: "fasta", Cardinality: authority.CardinalitySingle},
		},
		PublicationRequestID: acquisition.PublicationRequestID(id),
		Phase:                acquisition.PhasePinned,
	}
}

// staged advances op to a fully populated STAGED record.
func staged(op acquisition.Operation) acquisition.Operation {
	op.Phase = acquisition.PhaseStaged
	op.StagedPath = "/staging/" + string(op.ID)
	op.StagedMembers = []authority.Member{{
		SemanticKey: "a.fa", Role: "primary", DataFormat: "fasta", Cardinality: authority.CardinalitySingle,
		Proof: authority.ContentProof{Algorithm: "sha256", Digest: digest.FromString("a.fa").String()},
	}}
	op.Attempts = 2
	op.LastError = "previous attempt failed"
	return op
}

func begin(t *testing.T, s acquisition.CheckpointStore, op acquisition.Operation) acquisition.Operation {
	t.Helper()
	got, created, err := s.Begin(context.Background(), op)
	if err != nil || !created {
		t.Fatalf("Begin(%q): created=%v err=%v", op.ID, created, err)
	}
	return got
}

func update(t *testing.T, s acquisition.CheckpointStore, op acquisition.Operation) acquisition.Operation {
	t.Helper()
	got, err := s.Update(context.Background(), op)
	if err != nil {
		t.Fatalf("Update(%q, v%d): %v", op.ID, op.Version, err)
	}
	return got
}

func mustGet(t *testing.T, s acquisition.CheckpointStore, id acquisition.OperationID) acquisition.Operation {
	t.Helper()
	got, ok, err := s.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("Get(%q): ok=%v err=%v", id, ok, err)
	}
	return got
}

func beginCreatesThenReturnsExisting(t *testing.T, h Harness) {
	s := h.New(t)
	first := begin(t, s, Operation("op-begin"))
	again, created, err := s.Begin(context.Background(), Operation("op-begin"))
	if err != nil || created {
		t.Fatalf("second Begin: created=%v err=%v; want existing record", created, err)
	}
	if !reflect.DeepEqual(again, first) {
		t.Fatalf("second Begin returned %+v, want %+v", again, first)
	}
}

func beginConflictNeverOverwrites(t *testing.T, h Harness) {
	s := h.New(t)
	first := begin(t, s, Operation("op-conflict"))
	other := Operation("op-conflict")
	other.Fingerprint = "fp-different"
	if _, _, err := s.Begin(context.Background(), other); !errors.Is(err, acquisition.ErrOperationConflict) {
		t.Fatalf("conflicting Begin: err=%v, want ErrOperationConflict", err)
	}
	if got := mustGet(t, s, "op-conflict"); !reflect.DeepEqual(got, first) {
		t.Fatalf("conflicting Begin overwrote the record: %+v", got)
	}
}

func concurrentBeginCreatesOnce(t *testing.T, h Harness) {
	s := h.New(t)
	const workers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := s.Begin(context.Background(), Operation("op-race"))
			if err != nil {
				t.Errorf("Begin: %v", err)
				return
			}
			if c {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("concurrent Begin created %d records, want 1", created)
	}
}

func updateIsCAS(t *testing.T, h Harness) {
	s := h.New(t)
	op := begin(t, s, Operation("op-cas"))
	next := update(t, s, staged(op))
	if next.Version != op.Version+1 {
		t.Fatalf("Update version = %d, want %d", next.Version, op.Version+1)
	}
	if got := mustGet(t, s, "op-cas"); got.Version != next.Version || got.Phase != acquisition.PhaseStaged {
		t.Fatalf("stored record = %+v, want STAGED v%d", got, next.Version)
	}
}

func staleUpdateDoesNotOverwrite(t *testing.T, h Harness) {
	s := h.New(t)
	op := begin(t, s, Operation("op-stale"))
	newer := update(t, s, staged(op))
	// A paused owner resumes with its pre-update copy and tries to reset.
	old := op
	old.LastError = "stale owner write"
	if _, err := s.Update(context.Background(), old); !errors.Is(err, acquisition.ErrCheckpointStale) {
		t.Fatalf("stale Update: err=%v, want ErrCheckpointStale", err)
	}
	if got := mustGet(t, s, "op-stale"); !reflect.DeepEqual(got, newer) {
		t.Fatalf("stale Update overwrote newer state: got %+v, want %+v", got, newer)
	}
}

func identityFieldsImmutable(t *testing.T, h Harness) {
	mutations := map[string]func(*acquisition.Operation){
		"Fingerprint":          func(op *acquisition.Operation) { op.Fingerprint = "fp-other" },
		"Subject":              func(op *acquisition.Operation) { op.Subject = digest.FromString("other") },
		"PublicationRequestID": func(op *acquisition.Operation) { op.PublicationRequestID = "other-request" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := h.New(t)
			op := begin(t, s, Operation("op-identity"))
			changed := op
			mutate(&changed)
			if _, err := s.Update(context.Background(), changed); err == nil {
				t.Fatalf("Update changing %s succeeded", name)
			}
			if got := mustGet(t, s, "op-identity"); !reflect.DeepEqual(got, op) {
				t.Fatalf("rejected Update changed the record: %+v", got)
			}
		})
	}
}

func fieldFidelity(t *testing.T, h Harness) {
	s := h.New(t)
	op := begin(t, s, Operation("op-fidelity"))
	want := staged(op)
	want.Phase = acquisition.PhaseAccepted
	want.RevisionID = "sori-rev-9"
	want = update(t, s, want)
	if got := mustGet(t, s, "op-fidelity"); !reflect.DeepEqual(got, want) {
		t.Fatalf("read-back differs:\n got  %+v\n want %+v", got, want)
	}
}

func getUnknown(t *testing.T, h Harness) {
	s := h.New(t)
	if _, ok, err := s.Get(context.Background(), "op-missing"); ok || err != nil {
		t.Fatalf("Get(unknown): ok=%v err=%v; want not found", ok, err)
	}
}

func reopenPreservesFieldsAndVersion(t *testing.T, h Harness) {
	s := h.New(t)
	op := begin(t, s, Operation("op-reopen"))
	before := update(t, s, staged(op))
	s = h.Reopen(t, s)
	got := mustGet(t, s, "op-reopen")
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("reopen changed the record:\n got  %+v\n want %+v", got, before)
	}
	after := got
	after.Phase = acquisition.PhaseAcceptPending
	after = update(t, s, after)
	if after.Version != before.Version+1 {
		t.Fatalf("Version after reopen = %d, want %d (never reset)", after.Version, before.Version+1)
	}
}

func stalePreReopenUpdateRejected(t *testing.T, h Harness) {
	s := h.New(t)
	op := begin(t, s, Operation("op-reopen-stale"))
	newer := update(t, s, staged(op))
	s = h.Reopen(t, s)
	if _, err := s.Update(context.Background(), op); !errors.Is(err, acquisition.ErrCheckpointStale) {
		t.Fatalf("pre-reopen stale Update: err=%v, want ErrCheckpointStale", err)
	}
	if got := mustGet(t, s, "op-reopen-stale"); !reflect.DeepEqual(got, newer) {
		t.Fatalf("stale Update after reopen overwrote state: %+v", got)
	}
}
