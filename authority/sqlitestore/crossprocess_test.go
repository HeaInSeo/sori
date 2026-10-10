package sqlitestore_test

// Cross-process evidence for W01.1 (E016, E017). Each writer below is a separate OS
// process: the test binary re-executes itself into TestChildProcess, which opens the
// same database file, performs one operation and prints the result as JSON. A
// goroutine in this process is never counted as a second process.

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
	"syscall"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/authority"
	"github.com/HeaInSeo/sori/authority/sqlitestore"
)

const (
	envOp      = "SORI_SQLITESTORE_CHILD_OP"
	envDB      = "SORI_SQLITESTORE_CHILD_DB"
	envArgs    = "SORI_SQLITESTORE_CHILD_ARGS"
	envBarrier = "SORI_SQLITESTORE_CHILD_BARRIER"
	resultTag  = "SORI_CHILD_RESULT "
	xpAsset    = authority.AssetID("xp-asset")
	xpAlias    = "latest"
)

// childResult is what a child process reports. Err is "" on success, "conflict" for
// any of the authority conflict sentinels, otherwise the error text.
type childResult struct {
	PID            int
	Err            string
	Revision       authority.Revision
	History        []authority.BindEvent
	Representation authority.Representation
}

func xpManifest(digest string) authority.SemanticManifest {
	return authority.SemanticManifest{
		Origin: authority.OriginDerivedAsset,
		Members: []authority.Member{{
			SemanticKey: "m1",
			Role:        "primary",
			Proof:       authority.ContentProof{Algorithm: "sha256", Digest: digest},
			DataFormat:  "fastq",
			Cardinality: authority.CardinalitySingle,
		}},
		Provenance: authority.Provenance{
			BuilderIdentity:      "builder@v1",
			RuntimeImageIdentity: "img@sha256:deadbeef",
			FrozenRecipe:         "recipe{p=1}",
			InputLineage:         []string{"input-a"},
		},
		Presentation: map[string]string{"title": "xp"},
	}
}

func xpAttach(op authority.RequestID, rev authority.RevisionID, format string) authority.AttachRequest {
	return authority.AttachRequest{
		AttachOperationID: op,
		AssetID:           xpAsset,
		RevisionID:        rev,
		Format:            format,
		MemberProofs: []authority.Member{{
			SemanticKey: "m1",
			Role:        "primary",
			Proof:       authority.ContentProof{Algorithm: "sha256", Digest: "d1"},
		}},
		Locators: []authority.Locator{{Scheme: "oci", Coordinate: "reg/xp:" + format}},
	}
}

func classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, authority.ErrRequestConflict),
		errors.Is(err, authority.ErrAliasBindingConflict),
		errors.Is(err, authority.ErrAttachConflict):
		return "conflict"
	default:
		return err.Error()
	}
}

// TestChildProcess is the child entry point; it does nothing in a normal test run.
func TestChildProcess(t *testing.T) {
	op := os.Getenv(envOp)
	if op == "" {
		t.Skip("child-process helper; driven by the cross-process tests")
	}
	s, err := sqlitestore.Open(os.Getenv(envDB))
	if err != nil {
		t.Fatalf("child open: %v", err)
	}
	defer s.Close()
	if b := os.Getenv(envBarrier); b != "" {
		// Signal readiness, then wait for the parent to release every child at once.
		if err := os.WriteFile(fmt.Sprintf("%s.ready.%d", b, os.Getpid()), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(30 * time.Second); ; {
			if _, err := os.Stat(b + ".go"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("child: barrier timeout")
			}
			time.Sleep(time.Millisecond)
		}
	}
	ctx := context.Background()
	a := authority.New(s)
	args := strings.Split(os.Getenv(envArgs), ",")
	res := childResult{PID: os.Getpid()}
	// kill never returns: block until the SIGKILL lands so nothing after the hook runs.
	kill := func() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL); time.Sleep(time.Hour) }
	switch op {
	case "accept-kill-before-commit", "accept-kill-after-commit": // request, digest
		if op == "accept-kill-before-commit" {
			s.SetCommitHooksForTest(func() error { kill(); return nil }, nil)
		} else {
			s.SetCommitHooksForTest(nil, kill)
		}
		_, err = a.AcceptRevision(ctx, authority.AcceptRequest{
			RequestID: authority.RequestID(args[0]), AssetID: xpAsset, Manifest: xpManifest(args[1]),
		})
		t.Fatalf("child survived %s: %v", op, err)
	}
	switch op {
	case "accept": // request, digest
		res.Revision, err = a.AcceptRevision(ctx, authority.AcceptRequest{
			RequestID: authority.RequestID(args[0]), AssetID: xpAsset, Manifest: xpManifest(args[1]),
		})
	case "bind": // bind request, revision
		var ev authority.BindEvent
		ev, err = a.BindAlias(ctx, authority.BindRequest{
			BindRequestID: authority.RequestID(args[0]), Alias: xpAlias, AssetID: xpAsset,
			RevisionID: authority.RevisionID(args[1]),
		})
		res.History = []authority.BindEvent{ev}
	case "attach": // attach op, revision, format
		res.Representation, err = a.AttachRepresentation(ctx,
			xpAttach(authority.RequestID(args[0]), authority.RevisionID(args[1]), args[2]))
	case "e016-write":
		err = e016Write(ctx, a, &res)
	default:
		t.Fatalf("child: unknown op %q", op)
	}
	res.Err = classify(err)
	out, _ := json.Marshal(res)
	fmt.Println(resultTag + string(out))
}

// e016Write accepts, binds, attaches, then changes the locator and health.
func e016Write(ctx context.Context, a *authority.Authority, res *childResult) error {
	rev, err := a.AcceptRevision(ctx, authority.AcceptRequest{RequestID: "e016-req", AssetID: xpAsset, Manifest: xpManifest("d1")})
	if err != nil {
		return err
	}
	if _, err := a.BindAlias(ctx, authority.BindRequest{BindRequestID: "e016-bind", Alias: xpAlias, AssetID: xpAsset, RevisionID: rev.RevisionID}); err != nil {
		return err
	}
	rep, err := a.AttachRepresentation(ctx, xpAttach("e016-attach", rev.RevisionID, "fmt-a"))
	if err != nil {
		return err
	}
	if err := a.SetRepresentationLocators(ctx, rep.RepresentationID, []authority.Locator{{Scheme: "oci", Coordinate: "reg/xp:moved"}}); err != nil {
		return err
	}
	if err := a.SetRepresentationHealth(ctx, rep.RepresentationID, false); err != nil {
		return err
	}
	res.Revision = rev
	if res.History, err = a.AliasHistory(ctx, xpAlias); err != nil {
		return err
	}
	rep, _, err = a.GetRepresentation(ctx, rep.RepresentationID)
	res.Representation = rep
	return err
}

// startChild starts one child process for op without waiting for it.
func startChild(t *testing.T, db, barrier, op string, args ...string) (*exec.Cmd, *strings.Builder) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), envOp+"="+op, envDB+"="+db, envArgs+"="+strings.Join(args, ","), envBarrier+"="+barrier)
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	return cmd, out
}

func waitChild(t *testing.T, cmd *exec.Cmd, out *strings.Builder) childResult {
	t.Helper()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child %d: %v\n%s", cmd.Process.Pid, err, out)
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
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

// runChild runs one child process to completion.
func runChild(t *testing.T, db, op string, args ...string) childResult {
	t.Helper()
	cmd, out := startChild(t, db, "", op, args...)
	return waitChild(t, cmd, out)
}

// raceChildren starts two children that open the store, then releases both at once.
func raceChildren(t *testing.T, db string, a, b []string) (childResult, childResult) {
	t.Helper()
	barrier := filepath.Join(t.TempDir(), "barrier")
	c1, o1 := startChild(t, db, barrier, a[0], a[1:]...)
	c2, o2 := startChild(t, db, barrier, b[0], b[1:]...)
	for deadline := time.Now().Add(30 * time.Second); ; {
		m, _ := filepath.Glob(barrier + ".ready.*")
		if len(m) == 2 {
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
	r1, r2 := waitChild(t, c1, o1), waitChild(t, c2, o2)
	t.Logf("store=%s pids=%d,%d errs=%q,%q", db, r1.PID, r2.PID, r1.Err, r2.Err)
	return r1, r2
}

// E016: state written by one OS process is exact after a reopen in another process.
func TestCrossProcessReopen(t *testing.T) {
	db := filepath.Join(t.TempDir(), "authority.db")
	w := runChild(t, db, "e016-write")
	if w.Err != "" {
		t.Fatalf("writer process %d: %s", w.PID, w.Err)
	}
	t.Logf("writer pid=%d reader pid=%d store=%s", w.PID, os.Getpid(), db)
	s := open(t, db)
	ctx := context.Background()
	rev, ok, err := s.GetRevision(ctx, w.Revision.RevisionID)
	if err != nil || !ok || !reflect.DeepEqual(rev, w.Revision) {
		t.Fatalf("revision after reopen = %+v, %v, %v; want %+v", rev, ok, err, w.Revision)
	}
	hist, err := s.AliasHistory(ctx, xpAlias)
	if err != nil || !reflect.DeepEqual(hist, w.History) {
		t.Fatalf("alias history after reopen = %+v, %v; want %+v", hist, err, w.History)
	}
	rep, ok, err := s.GetRepresentation(ctx, w.Representation.RepresentationID)
	if err != nil || !ok || !reflect.DeepEqual(rep, w.Representation) {
		t.Fatalf("representation after reopen = %+v, %v, %v; want %+v", rep, ok, err, w.Representation)
	}
	if rep.Healthy || rep.Locators[0].Coordinate != "reg/xp:moved" {
		t.Fatalf("mutable state not preserved: %+v", rep)
	}
	a := authority.New(s)
	retry, err := a.AcceptRevision(ctx, authority.AcceptRequest{RequestID: "e016-req", AssetID: xpAsset, Manifest: xpManifest("d1")})
	if err != nil || !reflect.DeepEqual(retry, w.Revision) {
		t.Fatalf("same-input retry in reader process = %+v, %v; want %+v", retry, err, w.Revision)
	}
	if _, err := a.AcceptRevision(ctx, authority.AcceptRequest{RequestID: "e016-req", AssetID: xpAsset, Manifest: xpManifest("d2")}); !errors.Is(err, authority.ErrRequestConflict) {
		t.Fatalf("changed-input retry in reader process: err = %v, want ErrRequestConflict", err)
	}
}

// E017: two OS processes racing on one store produce one durable result for the same
// input and exactly one winner plus a conflict for changed input.
func TestCrossProcessConcurrentWriters(t *testing.T) {
	db := filepath.Join(t.TempDir(), "authority.db")
	s := open(t, db) // creates the schema before the race

	r1, r2 := raceChildren(t, db, []string{"accept", "same", "d1"}, []string{"accept", "same", "d1"})
	if r1.Err != "" || r2.Err != "" || !reflect.DeepEqual(r1.Revision, r2.Revision) {
		t.Fatalf("same-input accept: %+v / %+v", r1, r2)
	}
	base := r1.Revision.RevisionID

	r1, r2 = raceChildren(t, db, []string{"accept", "changed", "d1"}, []string{"accept", "changed", "d2"})
	winner := oneWinner(t, "changed-input accept", r1, r2)
	got, _, err := s.GetRevision(context.Background(), winner.Revision.RevisionID)
	if err != nil || !reflect.DeepEqual(got, winner.Revision) {
		t.Fatalf("stored revision %+v, %v; want the winner %+v", got, err, winner.Revision)
	}
	other := r1.Revision.RevisionID
	if other == "" {
		other = r2.Revision.RevisionID
	}

	r1, r2 = raceChildren(t, db, []string{"bind", "bind-x", string(base)}, []string{"bind", "bind-x", string(other)})
	oneWinner(t, "changed-input bind", r1, r2)

	r1, r2 = raceChildren(t, db, []string{"attach", "att-x", string(base), "fmt-a"}, []string{"attach", "att-x", string(base), "fmt-b"})
	winner = oneWinner(t, "changed-input attach (conflict before member check)", r1, r2)
	reps, err := s.ListRepresentations(context.Background(), base)
	if err != nil || len(reps) != 1 || !reflect.DeepEqual(reps[0], winner.Representation) {
		t.Fatalf("stored representations %+v, %v; want only the winner %+v", reps, err, winner.Representation)
	}
}

// runKilledChild runs a child that SIGKILLs itself at a commit boundary.
func runKilledChild(t *testing.T, db, op string, args ...string) {
	t.Helper()
	cmd, out := startChild(t, db, "", op, args...)
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("child %s: want death by SIGKILL, got %v\n%s", op, err, out)
	}
	t.Logf("child pid=%d killed (%s) store=%s", cmd.Process.Pid, op, db)
}

// E018: a process killed after its writes but before COMMIT leaves no acceptance; a
// process killed after COMMIT but before the ACK leaves exactly the complete one, and
// the same request retried later gets it back; an in-process failure at the commit
// boundary rolls back every write. Caller mutation of inputs/results is covered by the
// DeepCopy cases of the conformance suite.
func TestCommitBoundaryFaults(t *testing.T) {
	db := filepath.Join(t.TempDir(), "authority.db")
	ctx := context.Background()

	runKilledChild(t, db, "accept-kill-before-commit", "e018-a", "d1")
	s := open(t, db)
	if _, ok, err := s.GetRevision(ctx, "sori-rev-1"); err != nil || ok {
		t.Fatalf("killed-before-commit acceptance is visible: ok=%v err=%v", ok, err)
	}
	a := authority.New(s)
	rev, err := a.AcceptRevision(ctx, authority.AcceptRequest{RequestID: "e018-a", AssetID: xpAsset, Manifest: xpManifest("d1")})
	if err != nil || rev.RevisionID != "sori-rev-1" {
		t.Fatalf("retry after killed-before-commit = %+v, %v; want a fresh sori-rev-1", rev, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	runKilledChild(t, db, "accept-kill-after-commit", "e018-b", "d1")
	s = open(t, db)
	a = authority.New(s)
	stored, ok, err := s.GetRevision(ctx, "sori-rev-2")
	if err != nil || !ok || stored.RequestID != "e018-b" {
		t.Fatalf("killed-after-commit acceptance missing: %+v ok=%v err=%v", stored, ok, err)
	}
	retry, err := a.AcceptRevision(ctx, authority.AcceptRequest{RequestID: "e018-b", AssetID: xpAsset, Manifest: xpManifest("d1")})
	if err != nil || !reflect.DeepEqual(retry, stored) {
		t.Fatalf("retry after lost ACK = %+v, %v; want the committed %+v", retry, err, stored)
	}

	boom := errors.New("injected commit-boundary failure")
	s.SetCommitHooksForTest(func() error { return boom }, nil)
	if _, err := a.BindAlias(ctx, authority.BindRequest{BindRequestID: "e018-bind", Alias: xpAlias, AssetID: xpAsset, RevisionID: rev.RevisionID}); !errors.Is(err, boom) {
		t.Fatalf("bind under failure: err = %v", err)
	}
	if _, err := a.AttachRepresentation(ctx, xpAttach("e018-attach", rev.RevisionID, "fmt-a")); !errors.Is(err, boom) {
		t.Fatalf("attach under failure: err = %v", err)
	}
	s.SetCommitHooksForTest(nil, nil)
	if hist, err := s.AliasHistory(ctx, xpAlias); err != nil || len(hist) != 0 {
		t.Fatalf("rolled-back bind is visible: %+v, %v", hist, err)
	}
	if reps, err := s.ListRepresentations(ctx, rev.RevisionID); err != nil || len(reps) != 0 {
		t.Fatalf("rolled-back attach is visible: %+v, %v", reps, err)
	}
	ev, err := a.BindAlias(ctx, authority.BindRequest{BindRequestID: "e018-bind", Alias: xpAlias, AssetID: xpAsset, RevisionID: rev.RevisionID})
	if err != nil || ev.Sequence != 1 {
		t.Fatalf("bind retry after rollback = %+v, %v; want sequence 1", ev, err)
	}
	rep, err := a.AttachRepresentation(ctx, xpAttach("e018-attach", rev.RevisionID, "fmt-a"))
	if err != nil || rep.RepresentationID != "sori-rep-1" {
		t.Fatalf("attach retry after rollback = %+v, %v; want sori-rep-1", rep, err)
	}
}

func oneWinner(t *testing.T, what string, r1, r2 childResult) childResult {
	t.Helper()
	switch {
	case r1.Err == "" && r2.Err == "conflict":
		return r1
	case r2.Err == "" && r1.Err == "conflict":
		return r2
	}
	t.Fatalf("%s: want one success and one conflict, got %q / %q", what, r1.Err, r2.Err)
	return childResult{}
}
