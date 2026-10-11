package jobledger_test

// Cross-process evidence for W01.3 (E022, E023, E024). Every writer below is a
// separate OS process: the test binary re-executes itself into TestChildProcess,
// which opens the given database files, performs one scenario and prints its result
// as JSON. Some scenarios end the process with os.Exit without closing the store, or
// from inside the commit path. A goroutine in this process is never counted as a
// second process.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/authority"
	"github.com/HeaInSeo/sori/authority/sqlitestore"
	"github.com/HeaInSeo/sori/jobledger"
)

const (
	envOp      = "SORI_JOBLEDGER_CHILD_OP"
	envDB      = "SORI_JOBLEDGER_CHILD_DB"
	envAuthDB  = "SORI_JOBLEDGER_CHILD_AUTHDB"
	envGuard   = "SORI_JOBLEDGER_CHILD_GUARD"
	envArgs    = "SORI_JOBLEDGER_CHILD_ARGS"
	envBarrier = "SORI_JOBLEDGER_CHILD_BARRIER"
	resultTag  = "SORI_CHILD_RESULT "
	killCode   = 7
	argSep     = "\x1f" // child arguments may contain commas (namespace JSON)
)

type childResult struct {
	PID       int
	Command   string
	Err       string
	Namespace jobledger.NamespaceRecord
	Decision  jobledger.Decision
	Job       jobledger.Job
	Revision  authority.Revision
	Policy    jobledger.PolicyState
	Prepared  jobledger.PreparedPublication
	Binding   jobledger.Binding
}

func classify(err error) string {
	for _, c := range []struct {
		err  error
		name string
	}{
		{jobledger.ErrFenceStale, "fence_stale"}, {jobledger.ErrReadOnlyHold, "ro_hold"},
		{jobledger.ErrIdempotencyConflict, "conflict"}, {jobledger.ErrReconcileHold, "hold"},
	} {
		if errors.Is(err, c.err) {
			return c.name
		}
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// guardFile is a synthetic admin-control area: one JSON file outside the data
// directory that a restore never writes.
func writeGuard(t *testing.T, path string, g jobledger.Guard) {
	t.Helper()
	b, _ := json.Marshal(g)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func readGuard(t *testing.T, path string) jobledger.Guard {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var g jobledger.Guard
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func e023Manifest() authority.SemanticManifest {
	return authority.SemanticManifest{
		Origin: authority.OriginDerivedAsset,
		Members: []authority.Member{{
			SemanticKey: "m1", Role: "primary", DataFormat: "fastq", Cardinality: authority.CardinalitySingle,
			Proof: authority.ContentProof{Algorithm: "sha256", Digest: "digest-e023"},
		}},
		Provenance:   authority.Provenance{BuilderIdentity: "builder@v1", RuntimeImageIdentity: "img@sha256:beef", FrozenRecipe: "recipe{p=1}", InputLineage: []string{"in-a"}},
		Presentation: map[string]string{"title": "e023"},
	}
}

func waitBarrier(t *testing.T, b string) {
	t.Helper()
	if err := os.WriteFile(fmt.Sprintf("%s.ready.%d", b, os.Getpid()), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := os.Stat(b + ".go"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("child: barrier timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestChildProcess is the child entry point; it does nothing in a normal test run.
func TestChildProcess(t *testing.T) {
	op := os.Getenv(envOp)
	if op == "" {
		t.Skip("child-process helper; driven by the cross-process tests")
	}
	s, err := jobledger.Open(os.Getenv(envDB))
	if err != nil {
		t.Fatalf("child open: %v", err)
	}
	args := strings.Split(os.Getenv(envArgs), argSep)
	res := childResult{PID: os.Getpid(), Command: op + " " + strings.Join(args, " ")}
	err = runChild(t, s, op, args, &res)
	res.Err = classify(err)
	out, _ := json.Marshal(res)
	fmt.Println(resultTag + string(out))
	// Exit without closing the store: the next process sees only what was committed.
	os.Exit(0)
}

func runChild(t *testing.T, s *jobledger.Store, op string, args []string, res *childResult) error {
	ctx := context.Background()
	g := readGuard(t, os.Getenv(envGuard))
	switch op {
	case "e022":
		return childE022(ctx, t, s, g, res)
	case "e023":
		return childE023(ctx, t, s, g, res)
	case "prepare":
		return childPrepare(ctx, s, g, res)
	case "accept", "accept-kill-before", "accept-kill-after", "accept-race": // namespace json, key, meaning
		var ns jobledger.NamespaceRecord
		if err := json.Unmarshal([]byte(args[0]), &ns); err != nil {
			return err
		}
		switch op {
		case "accept-kill-before":
			s.SetCommitHooksForTest(func() error { os.Exit(killCode); return nil }, nil)
		case "accept-kill-after":
			s.SetCommitHooksForTest(nil, func() { os.Exit(killCode) })
		case "accept-race":
			waitBarrier(t, os.Getenv(envBarrier))
		}
		var err error
		res.Decision, err = s.Accept(ctx, g, req(ns, args[1], args[2]))
		return err
	}
	return fmt.Errorf("child: unknown op %q", op)
}

// childE022: accept a Job, run it, record the accepted Revision, request cancel, exit.
func childE022(ctx context.Context, t *testing.T, s *jobledger.Store, g jobledger.Guard, res *childResult) error {
	if err := s.Init(ctx, g.InstallationID); err != nil {
		return err
	}
	ns, err := s.AllocateNamespace(ctx, g, principal)
	if err != nil {
		return err
	}
	res.Namespace = ns
	d, err := s.Accept(ctx, g, req(ns, "k-e022", meaningA))
	if err != nil {
		return err
	}
	job, err := s.Advance(ctx, g, d.Job.JobID, 1, jobledger.Transition{Phase: jobledger.PhaseRunning})
	if err != nil {
		return err
	}
	rev, err := acceptRevision(ctx, t, job.DomainOpKey)
	if err != nil {
		return err
	}
	res.Revision = rev
	if job, err = s.Advance(ctx, g, job.JobID, job.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseRunning, RevisionID: string(rev.RevisionID)}); err != nil {
		return err
	}
	res.Job, err = s.RequestCancel(ctx, g, job.JobID, "operator-bob")
	return err
}

// acceptRevision accepts one Revision in the durable authority store, with the Job's
// domain operation key as its RequestID.
func acceptRevision(ctx context.Context, t *testing.T, requestID string) (authority.Revision, error) {
	t.Helper()
	as, err := sqlitestore.Open(os.Getenv(envAuthDB))
	if err != nil {
		return authority.Revision{}, err
	}
	defer as.Close()
	return authority.New(as).AcceptRevision(ctx, authority.AcceptRequest{
		RequestID: authority.RequestID(requestID), AssetID: "asset-e022", Manifest: e023Manifest(),
	})
}

var e023Changes = []jobledger.PolicyChange{
	{Actor: "alice", Operation: "block", Resource: "asset-e023", Blocked: true, Reason: "quarantine"},
	{Actor: "bob", Operation: "unblock", Resource: "asset-e023", Blocked: false, Reason: "cleared"},
	{Actor: "carol", Operation: "block", Resource: "asset-e023", Blocked: true, Reason: "license"},
}

// childE023: accept a Revision, block, unblock and block it again, exit.
func childE023(ctx context.Context, t *testing.T, s *jobledger.Store, g jobledger.Guard, res *childResult) error {
	if err := s.Init(ctx, g.InstallationID); err != nil {
		return err
	}
	rev, err := acceptRevision(ctx, t, "req-e023")
	if err != nil {
		return err
	}
	res.Revision = rev
	for i, ch := range e023Changes {
		if res.Policy, err = s.SetPolicy(ctx, g, ch, uint64(i)); err != nil {
			return err
		}
	}
	return nil
}

var e024Paths = []jobledger.MemberPath{
	{SemanticKey: "a.fa", CanonicalRelativePath: "data/a.fa", WholeFileSHA256: "sha256:aa"},
	{SemanticKey: "b.fa", CanonicalRelativePath: "data/b.fa", WholeFileSHA256: "sha256:bb"},
}

// childPrepare: prepare, package and record a binding intent, exit.
func childPrepare(ctx context.Context, s *jobledger.Store, g jobledger.Guard, res *childResult) error {
	if err := s.Init(ctx, g.InstallationID); err != nil {
		return err
	}
	p, _, err := s.Prepare(ctx, g, "op-e024", "asset-e024", "basis-1")
	if err != nil {
		return err
	}
	if res.Prepared, err = s.RecordPackage(ctx, g, p.PreparedID, "sha256:d1", e024Paths); err != nil {
		return err
	}
	res.Binding, err = s.BeginBinding(ctx, g, p.PreparedID, "rev_e024")
	return err
}

func startChild(t *testing.T, env map[string]string, op string, args ...string) (*exec.Cmd, *strings.Builder) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), envOp+"="+op, envArgs+"="+strings.Join(args, argSep))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	return cmd, out
}

func parseChild(t *testing.T, cmd *exec.Cmd, out *strings.Builder) childResult {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), resultTag); ok {
			var r childResult
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatalf("child result: %v", err)
			}
			if r.PID == os.Getpid() {
				t.Fatalf("child reported the parent PID %d", r.PID)
			}
			return r
		}
	}
	t.Fatalf("child %d printed no result:\n%s", cmd.Process.Pid, out)
	return childResult{}
}

func runChildOK(t *testing.T, env map[string]string, op string, args ...string) childResult {
	t.Helper()
	cmd, out := startChild(t, env, op, args...)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child %s: %v\n%s", op, err, out)
	}
	r := parseChild(t, cmd, out)
	t.Logf("child pid=%d cmd=%q err=%q (parent pid=%d)", r.PID, r.Command, r.Err, os.Getpid())
	return r
}

// runChildKilled runs a child that must end with killCode from inside the commit
// path, before it can print a result.
func runChildKilled(t *testing.T, env map[string]string, op string, args ...string) {
	t.Helper()
	cmd, out := startChild(t, env, op, args...)
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != killCode {
		t.Fatalf("child %s: exit %v, want code %d\n%s", op, err, killCode, out)
	}
	if strings.Contains(out.String(), resultTag) {
		t.Fatalf("killed child %s printed a result:\n%s", op, out)
	}
	t.Logf("child pid=%d op=%s killed with code %d (parent pid=%d)", cmd.Process.Pid, op, killCode, os.Getpid())
}

type harness struct {
	dir, db, authDB, guard string
	env                    map[string]string
}

func newHarness(t *testing.T, g jobledger.Guard) harness {
	t.Helper()
	dir := t.TempDir()
	h := harness{dir: dir, db: filepath.Join(dir, "data", "ledger.db"), authDB: filepath.Join(dir, "data", "authority.db"),
		guard: filepath.Join(dir, "admin", "guard.json")}
	for _, d := range []string{filepath.Dir(h.db), filepath.Dir(h.guard)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeGuard(t, h.guard, g)
	h.env = map[string]string{envDB: h.db, envAuthDB: h.authDB, envGuard: h.guard}
	return h
}

func nsArg(ns jobledger.NamespaceRecord) string {
	b, _ := json.Marshal(ns)
	return string(b)
}

// E022: a process accepts a Job, runs it, records the accepted Revision, requests
// cancel and exits without closing. After restart the cancel intent, Job ID, domain
// operation key and Revision are all still there; the accepted Revision is not
// deleted; replaying the same key returns the same Job and no new Revision.
func TestE022CancelIntentSurvivesRestart(t *testing.T) {
	h := newHarness(t, guard1)
	a := runChildOK(t, h.env, "e022")
	if a.Err != "" || a.Job.Phase != jobledger.PhaseCancelRequested || !a.Job.CancelRequested {
		t.Fatalf("writer: %+v", a)
	}
	ctx := context.Background()
	s := open(t, h.db)
	got, ok, err := s.GetJob(ctx, a.Job.JobID)
	if err != nil || !ok || !reflect.DeepEqual(got, a.Job) {
		t.Fatalf("job after restart = %+v, %v, %v; want %+v", got, ok, err, a.Job)
	}
	if got.CancelRequestedBy != "operator-bob" || got.RevisionID != string(a.Revision.RevisionID) || got.DomainOpKey == "" || got.Terminal != nil || got.Dispatchable() {
		t.Fatalf("recovered job %+v", got)
	}
	unfinished, err := s.UnfinishedJobs(ctx)
	if err != nil || len(unfinished) != 1 || unfinished[0].JobID != got.JobID {
		t.Fatalf("unfinished after restart = %+v, %v", unfinished, err)
	}

	as, err := sqlitestore.Open(h.authDB)
	if err != nil {
		t.Fatal(err)
	}
	defer as.Close()
	auth := authority.New(as)
	rev, ok, err := auth.GetRevision(ctx, a.Revision.RevisionID)
	if err != nil || !ok || !reflect.DeepEqual(rev, a.Revision) {
		t.Fatalf("accepted revision after cancel = %+v, %v, %v; want %+v", rev, ok, err, a.Revision)
	}

	// Resuming the same Job: same key and meaning → the same Job; the same domain
	// operation re-accepted → the same Revision, never a second one.
	d, err := s.Accept(ctx, guard1, req(a.Namespace, "k-e022", meaningA))
	if err != nil || d.Outcome != jobledger.OutcomeExisting || !reflect.DeepEqual(*d.Job, got) {
		t.Fatalf("replay after restart = %+v, %v", d, err)
	}
	rev2, err := auth.AcceptRevision(ctx, authority.AcceptRequest{RequestID: authority.RequestID(got.DomainOpKey), AssetID: "asset-e022", Manifest: e023Manifest()})
	if err != nil || rev2.RevisionID != rev.RevisionID {
		t.Fatalf("re-accept of the same domain operation = %+v, %v; want revision %s", rev2, err, rev.RevisionID)
	}
	if again, err := s.RequestCancel(ctx, guard1, got.JobID, "someone-else"); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("repeat cancel after restart = %+v, %v", again, err)
	}
	done, err := s.Advance(ctx, guard1, got.JobID, got.RecordVersion, jobledger.Transition{Phase: jobledger.PhaseCancelled})
	if err != nil || done.Terminal == nil || done.Terminal.RevisionID != string(rev.RevisionID) {
		t.Fatalf("terminal receipt = %+v, %v", done, err)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatal(err)
	}
}

// E023: a process stores block → unblock → block with audit entries and exits. After
// restart the current policy and the full actor/operation/resource history are
// recovered, and the accepted Revision's semantic manifest is unchanged.
func TestE023PolicyHistorySurvivesRestart(t *testing.T) {
	h := newHarness(t, guard1)
	a := runChildOK(t, h.env, "e023")
	if a.Err != "" || a.Policy.Revision != 3 {
		t.Fatalf("writer: %+v", a)
	}
	ctx := context.Background()
	s := open(t, h.db)
	cur, err := s.Policy(ctx, "asset-e023")
	if err != nil || !reflect.DeepEqual(cur, a.Policy) || !cur.Blocked || cur.Actor != "carol" {
		t.Fatalf("current policy after restart = %+v, %v; want %+v", cur, err, a.Policy)
	}
	hist, err := s.PolicyHistory(ctx, "asset-e023")
	if err != nil || len(hist) != len(e023Changes) {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	for i, ev := range hist {
		if ev.Change != e023Changes[i] || ev.Revision != uint64(i+1) {
			t.Fatalf("history[%d] = %+v, want %+v at revision %d", i, ev, e023Changes[i], i+1)
		}
	}
	if _, err := s.SetPolicy(ctx, guard1, e023Changes[1], 2); !errors.Is(err, jobledger.ErrVersionConflict) {
		t.Fatalf("stale policy write after restart: %v", err)
	}
	as, err := sqlitestore.Open(h.authDB)
	if err != nil {
		t.Fatal(err)
	}
	defer as.Close()
	rev, ok, err := authority.New(as).GetRevision(ctx, a.Revision.RevisionID)
	if err != nil || !ok || !reflect.DeepEqual(rev.Manifest, e023Manifest()) || rev.Fingerprint != a.Revision.Fingerprint {
		t.Fatalf("revision after policy changes = %+v, %v, %v", rev, ok, err)
	}
}

// E024 (prepared package/binding): a process prepares, packages and records a
// binding intent, then exits. After reopen the descriptor and member mapping are
// recovered, a different descriptor for the same prepared_id is a conflict that
// keeps the recorded one, the same operation never gets a second prepared_id, and the
// pending binding is not usable until it is marked ready.
func TestE024PreparedPackageAndBindingReopen(t *testing.T) {
	h := newHarness(t, guard1)
	a := runChildOK(t, h.env, "prepare")
	if a.Err != "" || a.Binding.State != jobledger.BindingIntent {
		t.Fatalf("writer: %+v", a)
	}
	ctx := context.Background()
	s := open(t, h.db)
	p, err := s.GetPrepared(ctx, a.Prepared.PreparedID)
	if err != nil || !reflect.DeepEqual(p, a.Prepared) || p.DescriptorDigest != "sha256:d1" || !reflect.DeepEqual(p.MemberPaths, e024Paths) {
		t.Fatalf("prepared after reopen = %+v, %v; want %+v", p, err, a.Prepared)
	}
	if _, err := s.RecordPackage(ctx, guard1, p.PreparedID, "sha256:d2", e024Paths); !errors.Is(err, jobledger.ErrDescriptorConflict) {
		t.Fatalf("different digest for the same prepared_id: %v", err)
	}
	if _, err := s.RecordPackage(ctx, guard1, p.PreparedID, "sha256:d1", e024Paths[:1]); !errors.Is(err, jobledger.ErrDescriptorConflict) {
		t.Fatalf("different mapping for the same prepared_id: %v", err)
	}
	if after, _ := s.GetPrepared(ctx, p.PreparedID); !reflect.DeepEqual(after, p) {
		t.Fatalf("conflicting package changed the record: %+v", after)
	}
	same, created, err := s.Prepare(ctx, guard1, "op-e024", "asset-e024", "basis-1")
	if err != nil || created || same.PreparedID != p.PreparedID {
		t.Fatalf("same operation prepared again = %+v, %v, %v", same, created, err)
	}
	if _, _, err := s.Prepare(ctx, guard1, "op-e024", "asset-e024", "basis-2"); !errors.Is(err, jobledger.ErrDescriptorConflict) {
		t.Fatalf("same operation, other basis: %v", err)
	}
	b, ok, err := s.GetBinding(ctx, a.Binding.BindingID)
	if err != nil || !ok || !reflect.DeepEqual(b, a.Binding) {
		t.Fatalf("binding after reopen = %+v, %v, %v", b, ok, err)
	}
	if u, _ := s.UsableBindings(ctx, "rev_e024"); len(u) != 0 {
		t.Fatalf("pending binding usable after reopen: %+v", u)
	}
	if _, err := s.BeginBinding(ctx, guard1, p.PreparedID, "rev_other"); !errors.Is(err, jobledger.ErrDescriptorConflict) {
		t.Fatalf("bind to another revision: %v", err)
	}
	if _, err := s.MarkBindingReady(ctx, guard1, b.BindingID, b.RecordVersion); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UsableBindings(ctx, "rev_e024"); len(u) != 1 || u[0].DescriptorDigest != "sha256:d1" {
		t.Fatalf("usable after ready = %+v", u)
	}
}

func initWithNamespace(t *testing.T, h harness) (*jobledger.Store, jobledger.NamespaceRecord) {
	t.Helper()
	s := open(t, h.db)
	if err := s.Init(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	return s, mustNamespace(t, s, guard1)
}

// E024 (acceptance boundary): a process killed just before the acceptance COMMIT
// leaves no Job, mapping or dispatchable intent; one killed just after it leaves
// exactly one, and the retry returns that Job.
func TestE024AcceptanceBoundaryProcessKill(t *testing.T) {
	h := newHarness(t, guard1)
	s, ns := initWithNamespace(t, h)
	ctx := context.Background()

	runChildKilled(t, h.env, "accept-kill-before", nsArg(ns), "k-before", meaningA)
	d, err := s.Lookup(ctx, guard1, req(ns, "k-before", meaningA))
	if err != nil || d.Outcome != jobledger.OutcomeNew || d.Job != nil {
		t.Fatalf("after kill before commit: %+v, %v; want no mapping (NEW)", d, err)
	}
	if jobs, _ := s.UnfinishedJobs(ctx); len(jobs) != 0 {
		t.Fatalf("kill before commit left jobs: %+v", jobs)
	}

	runChildKilled(t, h.env, "accept-kill-after", nsArg(ns), "k-after", meaningA)
	d, err = s.Lookup(ctx, guard1, req(ns, "k-after", meaningA))
	if err != nil || d.Outcome != jobledger.OutcomeExisting || d.Job == nil {
		t.Fatalf("after kill after commit: %+v, %v; want EXISTING", d, err)
	}
	jobs, _ := s.UnfinishedJobs(ctx)
	if len(jobs) != 1 || jobs[0].JobID != d.Job.JobID || !jobs[0].Dispatchable() || jobs[0].IdempotencyKey != "k-after" {
		t.Fatalf("kill after commit: jobs %+v; want exactly the mapped dispatchable job", jobs)
	}
	retry := runChildOK(t, h.env, "accept", nsArg(ns), "k-after", meaningA)
	if retry.Err != "" || retry.Decision.Outcome != jobledger.OutcomeExisting || retry.Decision.Job.JobID != d.Job.JobID {
		t.Fatalf("retry after lost ACK: %+v", retry)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatal(err)
	}
}

// E024 (concurrent first acceptance): two processes accept the same key at once.
// Same meaning → one NEW and one EXISTING of the same Job; different meaning → one
// NEW and one conflict. A key never has two Jobs.
func TestE024ConcurrentFirstAcceptance(t *testing.T) {
	h := newHarness(t, guard1)
	s, ns := initWithNamespace(t, h)
	ctx := context.Background()
	race := func(key, m1, m2 string) (childResult, childResult) {
		barrier := filepath.Join(t.TempDir(), "barrier")
		env := map[string]string{envBarrier: barrier}
		for k, v := range h.env {
			env[k] = v
		}
		c1, o1 := startChild(t, env, "accept-race", nsArg(ns), key, m1)
		c2, o2 := startChild(t, env, "accept-race", nsArg(ns), key, m2)
		for deadline := time.Now().Add(30 * time.Second); ; {
			if m, _ := filepath.Glob(barrier + ".ready.*"); len(m) == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("children never became ready")
			}
			time.Sleep(time.Millisecond)
		}
		if err := os.WriteFile(barrier+".go", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, c := range []*exec.Cmd{c1, c2} {
			if err := c.Wait(); err != nil {
				t.Fatalf("child: %v\n%s\n%s", err, o1, o2)
			}
		}
		r1, r2 := parseChild(t, c1, o1), parseChild(t, c2, o2)
		t.Logf("pid=%d %q err=%q %s | pid=%d %q err=%q %s", r1.PID, r1.Command, r1.Err, r1.Decision.Outcome, r2.PID, r2.Command, r2.Err, r2.Decision.Outcome)
		return r1, r2
	}

	r1, r2 := race("k-same", meaningA, meaningA)
	outs := map[jobledger.Outcome]int{r1.Decision.Outcome: 1}
	outs[r2.Decision.Outcome]++
	if r1.Err != "" || r2.Err != "" || outs[jobledger.OutcomeNew] != 1 || outs[jobledger.OutcomeExisting] != 1 || r1.Decision.Job.JobID != r2.Decision.Job.JobID {
		t.Fatalf("same-meaning race: %+v / %+v", r1, r2)
	}
	r1, r2 = race("k-diff", meaningA, meaningB)
	errs := map[string]int{r1.Err: 1}
	errs[r2.Err]++
	if errs[""] != 1 || errs["conflict"] != 1 {
		t.Fatalf("different-meaning race: %+v / %+v", r1, r2)
	}
	jobs, _ := s.UnfinishedJobs(ctx)
	if len(jobs) != 2 {
		t.Fatalf("jobs after races = %d, want one per key", len(jobs))
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// E024 (capture cut, restore and the non-restoring guard): a key accepted before the
// cut is old/EXISTING after restore; a key accepted after the cut by another process
// is absent from the restored store and is RECONCILE_HOLD, never NEW, both before and
// after activation; new work needs a namespace of the new epoch. The replaced writer
// process, still holding the old epoch, is fenced. The guard lives outside the data
// directory, so restoring the same backup again cannot reuse the activation.
func TestE024CutRestoreClassification(t *testing.T) {
	h := newHarness(t, guard1)
	s, ns := initWithNamespace(t, h)
	ctx := context.Background()
	pre, err := s.Accept(ctx, guard1, req(ns, "k-pre", meaningA))
	if err != nil {
		t.Fatal(err)
	}

	// Online capture cut: admin advances the counter, the store absorbs it, backup.
	g := guard1
	g.CutCounter = 1
	writeGuard(t, h.guard, g)
	if err := s.AbsorbCut(ctx, g); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(h.dir, "backup.db")
	cut, err := s.Snapshot(ctx, backup)
	if err != nil || cut.CutSeq != 1 || cut.LastLedgerSeq == 0 {
		t.Fatalf("snapshot = %+v, %v", cut, err)
	}

	post := runChildOK(t, h.env, "accept", nsArg(ns), "k-post", meaningA)
	if post.Err != "" || post.Decision.Outcome != jobledger.OutcomeNew {
		t.Fatalf("post-cut writer: %+v", post)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restore the backup into the data directory; admin-control is untouched.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(h.db + suffix)
	}
	copyFile(t, backup, h.db)
	s = open(t, h.db)
	nonce, err := s.MarkRestored(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seq, head, _ := s.LedgerHead(ctx); seq != cut.LastLedgerSeq+1 {
		t.Fatalf("restored ledger head %d %s; want cut %d + restore marker", seq, head, cut.LastLedgerSeq)
	}

	expect := func(when string, gd jobledger.Guard, n jobledger.NamespaceRecord, key string, out jobledger.Outcome, row string) {
		t.Helper()
		d, err := s.Lookup(ctx, gd, req(n, key, meaningA))
		if err != nil || d.Outcome != out || d.Row != row {
			t.Fatalf("%s %s: %+v, %v; want %s/%s", when, key, d, err, out, row)
		}
	}
	expect("restored", g, ns, "k-pre", jobledger.OutcomeExisting, "N2")
	expect("restored", g, ns, "k-post", jobledger.OutcomeHold, "N4H")
	expect("restored", g, ns, "k-new", jobledger.OutcomeHold, "N4H")
	if _, err := s.Accept(ctx, g, req(ns, "k-new", meaningA)); !errors.Is(err, jobledger.ErrReadOnlyHold) {
		t.Fatalf("accept on restored store: %v", err)
	}

	// Activation writes the new epoch to admin-control first, then to the store.
	g2 := jobledger.Guard{InstallationID: installation, ActivationEpoch: 2, CutCounter: 1, ActivationNonce: nonce}
	writeGuard(t, h.guard, g2)
	if err := s.Activate(ctx, g2); err != nil {
		t.Fatal(err)
	}
	expect("activated", g2, ns, "k-pre", jobledger.OutcomeExisting, "N6")
	expect("activated", g2, ns, "k-post", jobledger.OutcomeHold, "N7")
	expect("activated", g2, ns, "k-new", jobledger.OutcomeHold, "N7")
	ns2 := mustNamespace(t, s, g2)
	if d, err := s.Accept(ctx, g2, req(ns2, "k-new", meaningA)); err != nil || d.Outcome != jobledger.OutcomeNew || d.Job.CreatedEpoch != 2 {
		t.Fatalf("new work in the new epoch: %+v, %v", d, err)
	}
	if got, _, _ := s.GetJob(ctx, pre.Job.JobID); got.JobID != pre.Job.JobID {
		t.Fatalf("pre-cut job lost: %+v", got)
	}

	// The replaced writer still reads the old epoch from its own copy of the guard.
	oldGuard := filepath.Join(h.dir, "old-guard.json")
	writeGuard(t, oldGuard, g)
	oldEnv := map[string]string{envDB: h.db, envAuthDB: h.authDB, envGuard: oldGuard}
	old := runChildOK(t, oldEnv, "accept", nsArg(ns), "k-old-writer", meaningA)
	if old.Err != "fence_stale" {
		t.Fatalf("old-epoch writer: %+v, want fence_stale", old)
	}

	// Restoring the same backup again cannot reuse the consumed activation.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(h.db + suffix)
	}
	copyFile(t, backup, h.db)
	s = open(t, h.db)
	if _, err := s.MarkRestored(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(ctx, readGuard(t, h.guard)); !errors.Is(err, jobledger.ErrActivationRefused) {
		t.Fatalf("second activation from the same backup: %v", err)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatal(err)
	}
}
