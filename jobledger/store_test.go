package jobledger_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/HeaInSeo/sori/jobledger"
)

const (
	installation = "inst-test"
	principal    = "alice"
	meaningA     = "sha256:meaning-a"
	meaningB     = "sha256:meaning-b"
)

var guard1 = jobledger.Guard{InstallationID: installation, ActivationEpoch: 1}

func open(t *testing.T, path string) *jobledger.Store {
	t.Helper()
	s, err := jobledger.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newStore(t *testing.T) (*jobledger.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s := open(t, path)
	if err := s.Init(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	return s, path
}

func req(ns jobledger.NamespaceRecord, key, meaning string) jobledger.AcceptRequest {
	return jobledger.AcceptRequest{Namespace: ns.Ref(), Principal: ns.PrincipalSubject, IdempotencyKey: key, MeaningDigest: meaning, Kind: "LOCAL_IMPORT"}
}

func mustNamespace(t *testing.T, s *jobledger.Store, g jobledger.Guard) jobledger.NamespaceRecord {
	t.Helper()
	ns, err := s.AllocateNamespace(context.Background(), g, principal)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func TestAcceptNewExistingConflict(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, guard1)

	first, err := s.Accept(ctx, guard1, req(ns, "k1", meaningA))
	if err != nil || first.Outcome != jobledger.OutcomeNew || first.Row != "N4" || first.Job == nil {
		t.Fatalf("first accept = %+v, %v", first, err)
	}
	j := first.Job
	if !j.Dispatchable() || j.RecordVersion != 1 || j.CreatedEpoch != 1 || len(j.JobID) != len("job_")+52 || len(j.DomainOpKey) != len("dop_")+52 {
		t.Fatalf("new job %+v", j)
	}
	again, err := s.Accept(ctx, guard1, req(ns, "k1", meaningA))
	if err != nil || again.Outcome != jobledger.OutcomeExisting || !reflect.DeepEqual(again.Job, first.Job) {
		t.Fatalf("replay = %+v, %v; want EXISTING %+v", again, err, first.Job)
	}
	if _, err := s.Accept(ctx, guard1, req(ns, "k1", meaningB)); !errors.Is(err, jobledger.ErrIdempotencyConflict) {
		t.Fatalf("changed meaning: %v, want ErrIdempotencyConflict", err)
	}
	other := req(ns, "k1", meaningA)
	other.Principal = "mallory"
	if _, err := s.Accept(ctx, guard1, other); !errors.Is(err, jobledger.ErrNamespaceInvalid) {
		t.Fatalf("other principal: %v, want ErrNamespaceInvalid", err)
	}
	jobs, err := s.UnfinishedJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("unfinished = %v, %v; want exactly one job", jobs, err)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestClassificationRows(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, guard1)

	cases := []struct {
		name string
		g    jobledger.Guard
		mod  func(*jobledger.AcceptRequest)
		out  jobledger.Outcome
		row  string
	}{
		{"guard unusable", jobledger.Guard{}, nil, jobledger.OutcomeHold, "N0"},
		{"other installation", guard1, func(r *jobledger.AcceptRequest) { r.Namespace.InstallationID = "x" }, jobledger.OutcomeInvalid, "N1"},
		{"forged epoch on known id", guard1, func(r *jobledger.AcceptRequest) { r.Namespace.ActivationEpoch = 0 }, jobledger.OutcomeHold, "N7"},
		{"future epoch", guard1, func(r *jobledger.AcceptRequest) { r.Namespace.ActivationEpoch = 2 }, jobledger.OutcomeHold, "N8"},
		{"lost namespace row", guard1, func(r *jobledger.AcceptRequest) { r.Namespace.NamespaceID = "nsr_missing" }, jobledger.OutcomeHold, "N5"},
		{"cut not absorbed", jobledger.Guard{InstallationID: installation, ActivationEpoch: 1, CutCounter: 1}, nil, jobledger.OutcomeHold, "N4H"},
		{"live new", guard1, nil, jobledger.OutcomeNew, "N4"},
	}
	for _, c := range cases {
		r := req(ns, "k-class", meaningA)
		if c.mod != nil {
			c.mod(&r)
		}
		d, err := s.Lookup(ctx, c.g, r)
		if err != nil || d.Outcome != c.out || d.Row != c.row {
			t.Errorf("%s: %+v, %v; want %s/%s", c.name, d, err, c.out, c.row)
		}
	}
}

func TestFence(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, guard1)
	cases := []struct {
		g    jobledger.Guard
		want error
	}{
		{jobledger.Guard{InstallationID: "other", ActivationEpoch: 1}, jobledger.ErrInstallationMismatch},
		{jobledger.Guard{InstallationID: installation, ActivationEpoch: 2}, jobledger.ErrReadOnlyHold},
		{jobledger.Guard{InstallationID: installation, ActivationEpoch: 1, CutCounter: 1}, jobledger.ErrReadOnlyHold},
		{jobledger.Guard{}, jobledger.ErrReadOnlyHold},
	}
	for _, c := range cases {
		if _, err := s.Accept(ctx, c.g, req(ns, "k", meaningA)); !errors.Is(err, c.want) {
			t.Errorf("guard %+v: %v, want %v", c.g, err, c.want)
		}
	}
	if jobs, _ := s.UnfinishedJobs(ctx); len(jobs) != 0 {
		t.Fatalf("a fenced accept wrote %d jobs", len(jobs))
	}
}

func TestCancelAndTerminalReceipt(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, guard1)
	d, _ := s.Accept(ctx, guard1, req(ns, "k1", meaningA))
	j, err := s.Advance(ctx, guard1, d.Job.JobID, 1, jobledger.Transition{Phase: jobledger.PhaseRunning, RevisionID: "rev_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(ctx, guard1, j.JobID, 1, jobledger.Transition{Phase: jobledger.PhaseRunning}); !errors.Is(err, jobledger.ErrVersionConflict) {
		t.Fatalf("stale advance: %v", err)
	}
	if _, err := s.Advance(ctx, guard1, j.JobID, j.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseRunning, RevisionID: "rev_2"}); !errors.Is(err, jobledger.ErrDescriptorConflict) {
		t.Fatalf("second revision: %v", err)
	}
	c, err := s.RequestCancel(ctx, guard1, j.JobID, "bob")
	if err != nil || c.Phase != jobledger.PhaseCancelRequested || !c.CancelRequested || c.RevisionID != "rev_1" || c.Dispatchable() {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	c2, err := s.RequestCancel(ctx, guard1, j.JobID, "carol")
	if err != nil || !reflect.DeepEqual(c2, c) {
		t.Fatalf("repeat cancel = %+v, %v; want unchanged %+v", c2, err, c)
	}
	if _, err := s.Advance(ctx, guard1, j.JobID, c.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseRunning}); !errors.Is(err, jobledger.ErrInvalidTransition) {
		t.Fatalf("resume after cancel: %v", err)
	}
	done, err := s.Advance(ctx, guard1, j.JobID, c.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseCancelled})
	if err != nil || done.Terminal == nil || done.Terminal.Phase != jobledger.PhaseCancelled || done.Terminal.RevisionID != "rev_1" {
		t.Fatalf("cancelled = %+v, %v", done, err)
	}
	if _, err := s.RequestCancel(ctx, guard1, j.JobID, "bob"); !errors.Is(err, jobledger.ErrAlreadyTerminal) {
		t.Fatalf("cancel terminal: %v", err)
	}
	if again, _ := s.Accept(ctx, guard1, req(ns, "k1", meaningA)); again.Outcome != jobledger.OutcomeExisting || again.Job.JobID != j.JobID {
		t.Fatalf("replay after terminal = %+v", again)
	}
}

// Cancel intent recorded while a Job is in HOLD keeps the phase, but the Job can
// no longer be resumed; it can still be ended.
func TestCancelInHoldIsNotResumed(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, guard1)
	d, _ := s.Accept(ctx, guard1, req(ns, "k-hold", meaningA))
	j, err := s.Advance(ctx, guard1, d.Job.JobID, 1, jobledger.Transition{Phase: jobledger.PhaseHold})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.RequestCancel(ctx, guard1, j.JobID, "bob")
	if err != nil || c.Phase != jobledger.PhaseHold || !c.CancelRequested {
		t.Fatalf("cancel in hold = %+v, %v", c, err)
	}
	if _, err := s.Advance(ctx, guard1, c.JobID, c.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseRunning}); !errors.Is(err, jobledger.ErrInvalidTransition) {
		t.Fatalf("resume from hold with cancel intent: %v, want ErrInvalidTransition", err)
	}
	if got, _, _ := s.GetJob(ctx, c.JobID); !reflect.DeepEqual(got, c) {
		t.Fatalf("refused resume changed the job: %+v", got)
	}
	done, err := s.Advance(ctx, guard1, c.JobID, c.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseCancelled})
	if err != nil || done.Terminal == nil {
		t.Fatalf("cancel from hold = %+v, %v", done, err)
	}
}

// A Job in HOLD with cancel intent whose effect turns out to have committed records
// that success: HOLD -> SUCCEEDED writes the terminal receipt with the accepted
// RevisionID and keeps the cancel intent. It is still never resumed, and the
// terminal receipt is what a replay of the same key returns; nothing re-executes.
func TestCancelInHoldRecordsCommittedSuccess(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, guard1)
	d, _ := s.Accept(ctx, guard1, req(ns, "k-hold-ok", meaningA))
	r, err := s.Advance(ctx, guard1, d.Job.JobID, 1, jobledger.Transition{Phase: jobledger.PhaseRunning, RevisionID: "rev_1"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Advance(ctx, guard1, r.JobID, r.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseHold})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.RequestCancel(ctx, guard1, h.JobID, "bob")
	if err != nil || c.Phase != jobledger.PhaseHold || !c.CancelRequested || c.Dispatchable() {
		t.Fatalf("cancel in hold = %+v, %v", c, err)
	}
	for _, p := range []jobledger.Phase{jobledger.PhaseRunning, jobledger.PhaseQueued} {
		if _, err := s.Advance(ctx, guard1, c.JobID, c.RecordVersion, jobledger.Transition{Phase: p}); !errors.Is(err, jobledger.ErrInvalidTransition) {
			t.Fatalf("hold with cancel intent -> %s: %v, want ErrInvalidTransition", p, err)
		}
	}

	done, err := s.Advance(ctx, guard1, c.JobID, c.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseSucceeded})
	if err != nil {
		t.Fatalf("committed success from hold with cancel intent: %v", err)
	}
	if done.Phase != jobledger.PhaseSucceeded || done.Terminal == nil || done.Terminal.Phase != jobledger.PhaseSucceeded ||
		done.Terminal.RevisionID != "rev_1" || done.RevisionID != "rev_1" || !done.CancelRequested || done.CancelRequestedBy != "bob" {
		t.Fatalf("succeeded = %+v (terminal %+v)", done, done.Terminal)
	}
	if got, _, _ := s.GetJob(ctx, done.JobID); !reflect.DeepEqual(got, done) {
		t.Fatalf("stored job %+v, want %+v", got, done)
	}

	// The receipt is final: no further transition, cancel or re-execution.
	if _, err := s.Advance(ctx, guard1, done.JobID, done.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseRunning}); !errors.Is(err, jobledger.ErrAlreadyTerminal) {
		t.Fatalf("advance after terminal: %v", err)
	}
	if _, err := s.RequestCancel(ctx, guard1, done.JobID, "carol"); !errors.Is(err, jobledger.ErrAlreadyTerminal) {
		t.Fatalf("cancel after terminal: %v", err)
	}
	again, err := s.Accept(ctx, guard1, req(ns, "k-hold-ok", meaningA))
	if err != nil || again.Outcome != jobledger.OutcomeExisting || again.Job == nil || !reflect.DeepEqual(*again.Job, done) {
		t.Fatalf("replay = %+v, %v; want existing %+v", again, err, done)
	}
	if jobs, _ := s.UnfinishedJobs(ctx); len(jobs) != 0 {
		t.Fatalf("unfinished after committed success: %+v", jobs)
	}
}

func TestPolicyHistory(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ch := jobledger.PolicyChange{Actor: "alice", Operation: "block", Resource: "asset-a", Blocked: true, Reason: "quarantine"}
	st, err := s.SetPolicy(ctx, guard1, ch, 0)
	if err != nil || st.Revision != 1 || !st.Blocked {
		t.Fatalf("block = %+v, %v", st, err)
	}
	if _, err := s.SetPolicy(ctx, guard1, ch, 0); !errors.Is(err, jobledger.ErrVersionConflict) {
		t.Fatalf("stale policy write: %v", err)
	}
	if _, err := s.SetPolicy(ctx, jobledger.Guard{InstallationID: installation, ActivationEpoch: 1, CutCounter: 5}, ch, 1); !errors.Is(err, jobledger.ErrReadOnlyHold) {
		t.Fatalf("fenced policy write: %v", err)
	}
	h, err := s.PolicyHistory(ctx, "asset-a")
	if err != nil || len(h) != 1 || h[0].Change != ch {
		t.Fatalf("history = %+v, %v", h, err)
	}
}

func TestPreparedAndBinding(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	paths := []jobledger.MemberPath{{SemanticKey: "a.fa", CanonicalRelativePath: "data/a.fa", WholeFileSHA256: "sha256:aa"}}
	p, created, err := s.Prepare(ctx, guard1, "op-1", "asset-a", "basis-1")
	if err != nil || !created || p.State != jobledger.PreparedFrozen {
		t.Fatalf("prepare = %+v, %v, %v", p, created, err)
	}
	if _, err := s.BeginBinding(ctx, guard1, p.PreparedID, "rev_1"); !errors.Is(err, jobledger.ErrInvalidTransition) {
		t.Fatalf("bind before package: %v", err)
	}
	if _, err := s.RecordPackage(ctx, guard1, p.PreparedID, "sha256:d1", paths); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPackage(ctx, guard1, p.PreparedID, "sha256:d1", paths); err != nil {
		t.Fatalf("same package again: %v", err)
	}
	b, err := s.BeginBinding(ctx, guard1, p.PreparedID, "rev_1")
	if err != nil || b.State != jobledger.BindingIntent || b.DescriptorDigest != "sha256:d1" {
		t.Fatalf("bind = %+v, %v", b, err)
	}
	if u, _ := s.UsableBindings(ctx, "rev_1"); len(u) != 0 {
		t.Fatalf("pending binding selected: %+v", u)
	}
	if _, err := s.MarkBindingReady(ctx, guard1, b.BindingID, 7); !errors.Is(err, jobledger.ErrVersionConflict) {
		t.Fatalf("stale ready: %v", err)
	}
	r, err := s.MarkBindingReady(ctx, guard1, b.BindingID, 1)
	if err != nil || r.State != jobledger.BindingAttachedReady {
		t.Fatalf("ready = %+v, %v", r, err)
	}
	if u, _ := s.UsableBindings(ctx, "rev_1"); len(u) != 1 || u[0].BindingID != b.BindingID {
		t.Fatalf("usable = %+v", u)
	}
}

func TestLedgerTamperDetected(t *testing.T) {
	ctx := context.Background()
	tampers := map[string]string{
		"edited record":     `UPDATE ledger SET record = '{}' WHERE kind = 'namespace'`,
		"renumbered head":   `UPDATE ledger SET seq = 500 WHERE seq = (SELECT MAX(seq) FROM ledger)`,
		"renumbered middle": `UPDATE ledger SET seq = seq + 1000 WHERE seq >= 2`,
		"dropped row":       `DELETE FROM ledger WHERE seq = 2`,
		"truncated tail":    `DELETE FROM ledger WHERE seq = (SELECT MAX(seq) FROM ledger)`,
		"truncated all":     `DELETE FROM ledger`,
	}
	for name, stmt := range tampers {
		s, _ := newStore(t)
		ns := mustNamespace(t, s, guard1)
		if _, err := s.Accept(ctx, guard1, req(ns, "k1", meaningA)); err != nil {
			t.Fatal(err)
		}
		if err := s.VerifyLedger(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.ExecForTest(stmt); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := s.VerifyLedger(ctx); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
			t.Errorf("%s: %v, want ErrLedgerIntegrity", name, err)
		}
		if _, err := s.Snapshot(ctx, filepath.Join(t.TempDir(), "cut.db")); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
			t.Errorf("%s: Snapshot %v, want ErrLedgerIntegrity", name, err)
		}
	}
}

// TestLedgerTailLossRefused covers Codex P2 r4243714642: losing only the highest
// ledger rows leaves a contiguous, correctly chained prefix, so the chain alone
// cannot see it. The recorded head and an independently retained Cut must.
func TestLedgerTailLossRefused(t *testing.T) {
	ctx := context.Background()
	const dropTail = `DELETE FROM ledger WHERE seq = (SELECT MAX(seq) FROM ledger)`

	s, _ := newStore(t)
	ns := mustNamespace(t, s, guard1)
	if _, err := s.Accept(ctx, guard1, req(ns, "k1", meaningA)); err != nil {
		t.Fatal(err)
	}
	if err := s.ExecForTest(dropTail); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LedgerHead(ctx); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
		t.Errorf("LedgerHead after tail loss: %v, want ErrLedgerIntegrity", err)
	}
	if _, err := s.Snapshot(ctx, filepath.Join(t.TempDir(), "cut.db")); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
		t.Errorf("Snapshot after tail loss: %v, want ErrLedgerIntegrity", err)
	}
	if _, err := s.Accept(ctx, guard1, req(ns, "k2", meaningA)); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
		t.Errorf("append after tail loss: %v, want ErrLedgerIntegrity", err)
	}

	// A rewrite that also moves the recorded head back is caught only by evidence
	// kept outside the store, here a Cut taken earlier.
	s, _ = newStore(t)
	ns = mustNamespace(t, s, guard1)
	if _, err := s.Accept(ctx, guard1, req(ns, "k1", meaningA)); err != nil {
		t.Fatal(err)
	}
	cut, err := s.Snapshot(ctx, filepath.Join(t.TempDir(), "cut.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyCut(ctx, cut); err != nil {
		t.Fatalf("intact ledger against its cut: %v", err)
	}
	if _, err := s.Accept(ctx, guard1, req(ns, "k2", meaningA)); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyCut(ctx, cut); err != nil {
		t.Fatalf("grown ledger against an older cut: %v", err)
	}
	stale := cut
	stale.LedgerHeadDigest = "sha256:00"
	if err := s.VerifyCut(ctx, stale); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
		t.Errorf("cut with another digest: %v, want ErrLedgerIntegrity", err)
	}
	if err := s.ExecForTest(`DELETE FROM ledger WHERE seq >= ?`, cut.LastLedgerSeq); err != nil {
		t.Fatal(err)
	}
	var seq int64
	var digest string
	if err := s.QueryRowForTest(`SELECT seq, digest FROM ledger ORDER BY seq DESC LIMIT 1`).Scan(&seq, &digest); err != nil {
		t.Fatal(err)
	}
	if err := s.ExecForTest(`UPDATE ledger_meta SET value = ? WHERE key = 'ledger_head'`, fmt.Sprintf("%d:%s", seq, digest)); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatalf("rewritten head is self-consistent, VerifyLedger: %v", err)
	}
	if err := s.VerifyCut(ctx, cut); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
		t.Errorf("VerifyCut after rewrite below the cut: %v, want ErrLedgerIntegrity", err)
	}
}

// TestLedgerReadsDuringConcurrentAppend covers GR-P2-HEADREAD: a head or chain check
// that reads the last row and the recorded head separately reports a healthy store
// as truncated when an append commits in between. Readers on the writer's handle and
// on a second handle must never see ErrLedgerIntegrity while appends run.
func TestLedgerReadsDuringConcurrentAppend(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	ns := mustNamespace(t, s, guard1)
	if _, err := s.Accept(ctx, guard1, req(ns, "k0", meaningA)); err != nil {
		t.Fatal(err)
	}
	cut, err := s.Snapshot(ctx, filepath.Join(t.TempDir(), "cut.db"))
	if err != nil {
		t.Fatal(err)
	}
	other := open(t, path)

	const appends = 150
	done := make(chan struct{})
	var wg sync.WaitGroup
	var reads, spurious atomic.Int64
	var mu sync.Mutex
	var firstErr error
	read := func(st *jobledger.Store) {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			for _, err := range []error{
				func() error { _, _, err := st.LedgerHead(ctx); return err }(),
				st.VerifyLedger(ctx),
				st.VerifyCut(ctx, cut),
			} {
				reads.Add(1)
				if err != nil {
					spurious.Add(1)
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}
	}
	wg.Add(2)
	go read(s)
	go read(other)
	for i := 1; i <= appends; i++ {
		if _, err := s.Accept(ctx, guard1, req(ns, fmt.Sprintf("k%d", i), meaningA)); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("append %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
	if n := spurious.Load(); n != 0 {
		t.Fatalf("%d of %d reads failed during concurrent appends; first: %v", n, reads.Load(), firstErr)
	}
	if reads.Load() == 0 {
		t.Fatal("no read ran during the appends")
	}
	if err := s.VerifyCut(ctx, cut); err != nil {
		t.Fatalf("quiescent VerifyCut: %v", err)
	}
}

func TestInitAndSchema(t *testing.T) {
	s, path := newStore(t)
	ctx := context.Background()
	if err := s.Init(ctx, installation); !errors.Is(err, jobledger.ErrActivationRefused) {
		t.Fatalf("second init: %v", err)
	}
	if err := s.ExecForTest(`UPDATE ledger_meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := jobledger.Open(path); !errors.Is(err, jobledger.ErrSchemaVersion) {
		t.Fatalf("open other schema: %v", err)
	}
	if _, err := jobledger.Open(":memory:"); !errors.Is(err, jobledger.ErrTransientPath) {
		t.Fatalf("open memory: %v", err)
	}
	u := open(t, filepath.Join(t.TempDir(), "fresh.db"))
	if _, err := u.AllocateNamespace(ctx, guard1, principal); !errors.Is(err, jobledger.ErrNotInitialized) {
		t.Fatalf("uninitialized store: %v", err)
	}
}
