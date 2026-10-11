package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/HeaInSeo/sori/authority"
)

// DurableCheckpointStore is a CheckpointStore over a file that can be closed and
// opened again.
type DurableCheckpointStore interface {
	CheckpointStore
	Close() error
}

// RunDurableStagedResumeForTest runs the W01.2 E021 staged-resume policy against a
// durable CheckpointStore. Between the crash and the resume the store is closed and
// opened again from its file, so the resume reads only the durable checkpoint:
//
//   - staged copy unavailable on the resuming replica: ErrStagedUnavailable
//     (retryable, not fail-closed), a CAS reset to PINNED with identity unchanged,
//     and a retry that re-transfers and accepts exactly one Revision;
//   - staged copy present but tampered: a fail-closed ErrStagedInvalid with
//     ErrDigestMismatch, a reset to PINNED with no Revision, then a clean retry.
//
// The checks are the same as the in-memory PR27/PR28 regressions, which stay as
// they are. It lives in a _test.go file so a durable adapter package can drive it
// from package acquisition_test without an import cycle.
func RunDurableStagedResumeForTest(t *testing.T, open func(t *testing.T, path string) DurableCheckpointStore) {
	t.Run("UnavailableResetsAndRetries", func(t *testing.T) { durableUnavailable(t, open) })
	t.Run("DigestMismatchFailsClosed", func(t *testing.T) { durableMismatch(t, open) })
}

type durableHarness struct {
	*harness
	path  string
	store DurableCheckpointStore
}

func newDurableHarness(t *testing.T, open func(*testing.T, string) DurableCheckpointStore) *durableHarness {
	t.Helper()
	d := &durableHarness{
		harness: &harness{
			auth:    authority.New(authority.NewMemoryStore()),
			staging: t.TempDir(),
			creds:   map[CredentialRef]auth.Credential{},
		},
		path: filepath.Join(t.TempDir(), "checkpoint.db"),
	}
	d.store = open(t, d.path)
	d.cps = &crashStore{CheckpointStore: d.store, crashOn: PhaseAcceptPending}
	return d
}

// reopen closes the store and opens the same file again; the crash wrapper is gone,
// as after a process restart.
func (d *durableHarness) reopen(t *testing.T, open func(*testing.T, string) DurableCheckpointStore) {
	t.Helper()
	if err := d.store.Close(); err != nil {
		t.Fatalf("close checkpoint store: %v", err)
	}
	d.store = open(t, d.path)
	d.cps = d.store
}

func (d *durableHarness) get(t *testing.T, id OperationID) Operation {
	t.Helper()
	op, ok, err := d.store.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("checkpoint for %q: ok=%v err=%v", id, ok, err)
	}
	return op
}

// crashThenReopen runs until the crash after STAGED, reopens the store, and returns
// the STAGED checkpoint read from the reopened file.
func (d *durableHarness) crashThenReopen(t *testing.T, open func(*testing.T, string) DurableCheckpointStore, req Request) Operation {
	t.Helper()
	if _, err := d.acquirer().Acquire(context.Background(), req); !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	d.reopen(t, open)
	staged := d.get(t, req.OperationID)
	if staged.Phase != PhaseStaged || staged.StagedPath == "" || len(staged.StagedMembers) == 0 {
		t.Fatalf("reopened store lost the STAGED checkpoint: %+v", staged)
	}
	return staged
}

func durableUnavailable(t *testing.T, open func(*testing.T, string) DurableCheckpointStore) {
	d := newDurableHarness(t, open)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-durable-1", fx.digest, reg.endpoint(""))

	staged := d.crashThenReopen(t, open, req)
	if err := os.RemoveAll(staged.StagedPath); err != nil {
		t.Fatal(err)
	}
	hits := reg.hits()
	replicaB := replicaAcquirer(d.harness, t.TempDir())

	_, err := replicaB.Acquire(ctx, req)
	if !errors.Is(err, ErrStagedUnavailable) || IsFailClosed(err) || errors.Is(err, ErrStagedInvalid) || errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("resume after reopen: want retryable ErrStagedUnavailable, got %v", err)
	}
	reset := d.get(t, req.OperationID)
	if reset.Phase != PhasePinned || reset.StagedPath != "" || reset.StagedMembers != nil {
		t.Fatalf("expected a durable reset to PINNED without a staged locator, got %+v", reset)
	}
	if reset.ID != staged.ID || reset.Fingerprint != staged.Fingerprint || reset.Subject != staged.Subject ||
		reset.PublicationRequestID != staged.PublicationRequestID || reset.Version <= staged.Version {
		t.Fatalf("reset must keep identity and be a CAS update: before %+v after %+v", staged, reset)
	}
	if reg.hits() != hits || revisionCount(t, d.harness) != 0 {
		t.Fatal("the unavailable resume must neither transfer nor accept")
	}

	res, err := replicaB.Acquire(ctx, req)
	if err != nil || !res.Accepted() || reg.hits() == hits || revisionCount(t, d.harness) != 1 {
		t.Fatalf("retry: accepted=%v err=%v re-transferred=%v revisions=%d", res.Accepted(), err, reg.hits() != hits, revisionCount(t, d.harness))
	}
	if got := d.get(t, req.OperationID); got.Phase != PhaseAccepted || got.RevisionID != res.Revision.RevisionID {
		t.Fatalf("durable checkpoint after retry = %+v, want ACCEPTED %q", got, res.Revision.RevisionID)
	}
}

func durableMismatch(t *testing.T, open func(*testing.T, string) DurableCheckpointStore) {
	d := newDurableHarness(t, open)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-durable-2", fx.digest, reg.endpoint(""))

	staged := d.crashThenReopen(t, open, req)
	member := digest.Digest(staged.StagedMembers[0].Proof.Digest)
	blob := filepath.Join(staged.StagedPath, "blobs", member.Algorithm().String(), member.Encoded())
	// Staged blobs are written read-only; replace the file to model tampering.
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := d.acquirer().Acquire(ctx, req)
	if !IsFailClosed(err) || !errors.Is(err, ErrStagedInvalid) || !errors.Is(err, ErrDigestMismatch) || errors.Is(err, ErrStagedUnavailable) {
		t.Fatalf("tampered staged copy after reopen: want fail-closed ErrStagedInvalid/ErrDigestMismatch, got %v", err)
	}
	if got := d.get(t, req.OperationID); got.Phase != PhasePinned || got.Version <= staged.Version || revisionCount(t, d.harness) != 0 {
		t.Fatalf("expected a durable reset to PINNED with no revision, got %+v", got)
	}

	res, err := d.acquirer().Acquire(ctx, req)
	if err != nil || !res.Accepted() || revisionCount(t, d.harness) != 1 {
		t.Fatalf("re-transfer: accepted=%v err=%v", res.Accepted(), err)
	}
}
