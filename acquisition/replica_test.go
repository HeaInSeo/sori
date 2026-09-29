package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Cross-replica resume tests (W40). Replicas A and B share the checkpoint and
// authority stores but stage under different StagingRoots, as separate Pods on
// separate nodes would. Removing A's staged directory models B's node not
// having A's replica-local copy.

// replicaAcquirer returns an Acquirer over h's shared stores that stages under
// its own root, modeling another replica.
func replicaAcquirer(h *harness, stagingRoot string) *Acquirer {
	a := h.acquirer()
	a.StagingRoot = stagingRoot
	return a
}

// crashAStaged runs replica A until it crashes after STAGED (before
// ACCEPT_PENDING), then removes A's staged copy as if A's node were gone. It
// returns the STAGED checkpoint A left behind.
func crashAStaged(t *testing.T, h *harness, req Request) Operation {
	t.Helper()
	if _, err := h.acquirer().Acquire(context.Background(), req); !errors.Is(err, errCrash) {
		t.Fatalf("replica A: expected simulated crash, got %v", err)
	}
	staged := mustGetOp(t, h, req.OperationID)
	if staged.Phase != PhaseStaged || staged.StagedPath == "" {
		t.Fatalf("replica A: expected STAGED checkpoint, got %+v", staged)
	}
	if err := os.RemoveAll(staged.StagedPath); err != nil {
		t.Fatal(err)
	}
	return staged
}

// TestI4A_CrossReplicaStagedUnavailableIsRetryable: replica B resuming a STAGED
// checkpoint whose copy exists only on replica A must not report an integrity
// failure. The operation resets to PINNED with its identity unchanged, and B's
// retry re-transfers and accepts exactly one Revision.
func TestI4A_CrossReplicaStagedUnavailableIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.cps = &crashStore{CheckpointStore: h.mem, crashOn: PhaseAcceptPending}
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-replica-1", fx.digest, reg.endpoint(""))

	staged := crashAStaged(t, h, req)
	hits := reg.hits()
	replicaB := replicaAcquirer(h, t.TempDir())

	_, err := replicaB.Acquire(ctx, req)
	if !errors.Is(err, ErrStagedUnavailable) {
		t.Fatalf("replica B: expected ErrStagedUnavailable, got %v", err)
	}
	if IsFailClosed(err) || errors.Is(err, ErrStagedInvalid) || errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("replica B: unavailable staged copy was classified as an integrity failure: %v", err)
	}
	reset := mustGetOp(t, h, req.OperationID)
	if reset.Phase != PhasePinned || reset.StagedPath != "" || reset.StagedMembers != nil {
		t.Fatalf("expected reset to PINNED without a staged locator, got %+v", reset)
	}
	if reset.ID != staged.ID || reset.Fingerprint != staged.Fingerprint || reset.Subject != staged.Subject ||
		reset.PublicationRequestID != staged.PublicationRequestID {
		t.Fatalf("reset changed operation identity: before %+v after %+v", staged, reset)
	}
	if reset.Version <= staged.Version {
		t.Fatalf("reset must be a CAS update: version %d -> %d", staged.Version, reset.Version)
	}
	if reg.hits() != hits || revisionCount(t, h) != 0 {
		t.Fatal("the unavailable resume must neither transfer nor accept")
	}

	res, err := replicaB.Acquire(ctx, req)
	if err != nil || !res.Accepted() {
		t.Fatalf("replica B retry: accepted=%v err=%v", res.Accepted(), err)
	}
	if reg.hits() == hits {
		t.Fatal("replica B retry did not re-transfer")
	}
	if revisionCount(t, h) != 1 {
		t.Fatal("expected exactly one revision")
	}
	if !filepath.IsAbs(res.Operation.StagedPath) || filepath.Dir(res.Operation.StagedPath) != replicaB.StagingRoot {
		t.Fatalf("replica B staged under %q, want its own root %q", res.Operation.StagedPath, replicaB.StagingRoot)
	}
}

// TestI4A_CrossReplicaPresentButCorruptStagedCopyStillFailsClosed is the
// negative control: a staged directory that exists on this replica but whose
// content is incomplete is an integrity failure, not unavailability.
func TestI4A_CrossReplicaPresentButCorruptStagedCopyStillFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.cps = &crashStore{CheckpointStore: h.mem, crashOn: PhaseAcceptPending}
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-replica-2", fx.digest, reg.endpoint(""))

	if _, err := h.acquirer().Acquire(ctx, req); !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	op := mustGetOp(t, h, req.OperationID)
	// The directory stays, but the pinned manifest blob is gone.
	manifest := filepath.Join(op.StagedPath, "blobs", op.Subject.Algorithm().String(), op.Subject.Encoded())
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}

	_, err := h.acquirer().Acquire(ctx, req)
	if !IsFailClosed(err) || !errors.Is(err, ErrStagedInvalid) || errors.Is(err, ErrStagedUnavailable) {
		t.Fatalf("expected a fail-closed ErrStagedInvalid, got %v", err)
	}
	if got := mustGetOp(t, h, req.OperationID); got.Phase != PhasePinned || revisionCount(t, h) != 0 {
		t.Fatalf("expected reset to PINNED with no revision, got %+v", got)
	}
}

// TestI4A_CrossReplicaAckLossConvergesWithoutTransfer: A's authority accept
// committed but the ACCEPTED checkpoint was lost and A's node is gone. B
// reconciles to the same Revision with no transfer, even though A's staged
// copy is not present on B.
func TestI4A_CrossReplicaAckLossConvergesWithoutTransfer(t *testing.T) {
	h := newHarness(t)
	h.cps = &crashStore{CheckpointStore: h.mem, crashOn: PhaseAccepted}
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-replica-3", fx.digest, reg.endpoint(""))

	if _, err := h.acquirer().Acquire(ctx, req); !errors.Is(err, errCrash) {
		t.Fatalf("replica A: expected simulated crash, got %v", err)
	}
	pending := mustGetOp(t, h, req.OperationID)
	if pending.Phase != PhaseAcceptPending {
		t.Fatalf("expected %s, got %q", PhaseAcceptPending, pending.Phase)
	}
	if err := os.RemoveAll(pending.StagedPath); err != nil {
		t.Fatal(err)
	}
	hits := reg.hits()

	res, err := replicaAcquirer(h, t.TempDir()).Acquire(ctx, req)
	if err != nil || !res.Accepted() {
		t.Fatalf("replica B reconcile: accepted=%v err=%v", res.Accepted(), err)
	}
	if res.Revision.RevisionID != "sori-rev-1" || revisionCount(t, h) != 1 {
		t.Fatalf("reconcile minted a second revision: %q", res.Revision.RevisionID)
	}
	if reg.hits() != hits {
		t.Fatal("reconcile after ACK loss re-transferred")
	}
}
