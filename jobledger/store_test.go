package jobledger_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
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
	s, _ := newStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, guard1)
	if _, err := s.Accept(ctx, guard1, req(ns, "k1", meaningA)); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ExecForTest(`UPDATE ledger SET record = '{}' WHERE kind = 'namespace'`); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyLedger(ctx); !errors.Is(err, jobledger.ErrLedgerIntegrity) {
		t.Fatalf("tampered ledger: %v, want ErrLedgerIntegrity", err)
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
