package sqlitecheckpoint_test

// Cross-process evidence for W01.2 (E019, E020). Each writer below is a separate OS
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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/HeaInSeo/sori/acquisition"
	"github.com/HeaInSeo/sori/acquisition/checkpointtest"
	"github.com/HeaInSeo/sori/acquisition/sqlitecheckpoint"
	"github.com/HeaInSeo/sori/authority"
)

const (
	envOp      = "SORI_SQLITECHECKPOINT_CHILD_OP"
	envDB      = "SORI_SQLITECHECKPOINT_CHILD_DB"
	envArgs    = "SORI_SQLITECHECKPOINT_CHILD_ARGS"
	envBarrier = "SORI_SQLITECHECKPOINT_CHILD_BARRIER"
	resultTag  = "SORI_CHILD_RESULT "
)

// childResult is what a child process reports. Err is "" on success, "conflict" for
// ErrOperationConflict, "stale" for ErrCheckpointStale, otherwise the error text.
type childResult struct {
	PID     int
	Command string
	Err     string
	Created bool
	Op      acquisition.Operation
}

func classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, acquisition.ErrOperationConflict):
		return "conflict"
	case errors.Is(err, acquisition.ErrCheckpointStale):
		return "stale"
	default:
		return err.Error()
	}
}

// stagedOp is the fully populated STAGED record a writer process checkpoints.
func stagedOp(op acquisition.Operation, path string) acquisition.Operation {
	op.Phase = acquisition.PhaseStaged
	op.StagedPath = path
	op.StagedMembers = []authority.Member{{
		SemanticKey: "a.fa", Role: "primary", DataFormat: "fasta", Cardinality: authority.CardinalitySingle,
		Proof: authority.ContentProof{Algorithm: "sha256", Digest: digest.FromString("a.fa").String()},
	}}
	op.Attempts = 2
	op.LastError = "previous attempt failed"
	return op
}

func waitBarrier(t *testing.T, b string) {
	t.Helper()
	// Signal readiness, then wait for the parent to release every child at once.
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
	barrier := os.Getenv(envBarrier)
	if op == "open-race" && barrier != "" {
		// Release before Open: both processes race to create and WAL-convert a
		// database file that does not exist yet.
		waitBarrier(t, barrier)
		barrier = ""
	}
	s, err := sqlitecheckpoint.Open(os.Getenv(envDB))
	if err != nil {
		t.Fatalf("child open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	args := strings.Split(os.Getenv(envArgs), ",")
	res := childResult{PID: os.Getpid(), Command: op + " " + strings.Join(args, ",")}
	switch op {
	case "open-race": // id; Open already happened after the barrier
		res.Op, res.Created, err = s.Begin(ctx, checkpointtest.Operation(acquisition.OperationID(args[0])))
	case "begin": // id, fingerprint
		want := checkpointtest.Operation(acquisition.OperationID(args[0]))
		want.Fingerprint = args[1]
		if barrier != "" {
			waitBarrier(t, barrier)
		}
		res.Op, res.Created, err = s.Begin(ctx, want)
	case "write-staged": // id, staged path
		var begun acquisition.Operation
		begun, res.Created, err = s.Begin(ctx, checkpointtest.Operation(acquisition.OperationID(args[0])))
		if err == nil {
			res.Op, err = s.Update(ctx, stagedOp(begun, args[1]))
		}
	case "update": // id, version to update from, last error
		var cur acquisition.Operation
		cur, _, err = s.Get(ctx, acquisition.OperationID(args[0]))
		if err != nil {
			break
		}
		// The handle is read before the barrier, so both racing processes hold the
		// same version when they are released.
		cur.Version, err = strconv.Atoi(args[1])
		if err != nil {
			break
		}
		cur.LastError = args[2]
		if barrier != "" {
			waitBarrier(t, barrier)
		}
		res.Op, err = s.Update(ctx, cur)
	default:
		t.Fatalf("child: unknown op %q", op)
	}
	res.Err = classify(err)
	out, _ := json.Marshal(res)
	fmt.Println(resultTag + string(out))
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
	t.Logf("store=%s pid=%d cmd=%q err=%q created=%v | pid=%d cmd=%q err=%q created=%v",
		db, r1.PID, r1.Command, r1.Err, r1.Created, r2.PID, r2.Command, r2.Err, r2.Created)
	if r1.PID == r2.PID {
		t.Fatalf("both results came from pid %d", r1.PID)
	}
	return r1, r2
}

func oneWinner(t *testing.T, what, loserErr string, r1, r2 childResult) childResult {
	t.Helper()
	switch {
	case r1.Err == "" && r2.Err == loserErr:
		return r1
	case r2.Err == "" && r1.Err == loserErr:
		return r2
	}
	t.Fatalf("%s: want one success and one %q, got %q / %q", what, loserErr, r1.Err, r2.Err)
	return childResult{}
}

// E019: two OS processes racing Begin on one store. The same operation input yields
// one record (exactly one creator, both see it); a different fingerprint for the same
// operation ID is a conflict for exactly one of them, and the stored record is the
// winner's, never overwritten.
func TestCrossProcessConcurrentBegin(t *testing.T) {
	db := filepath.Join(t.TempDir(), "checkpoint.db")
	s := open(t, db) // creates the schema before the race
	ctx := context.Background()

	r1, r2 := raceChildren(t, db, []string{"begin", "op-same", "fp-op-same"}, []string{"begin", "op-same", "fp-op-same"})
	if r1.Err != "" || r2.Err != "" || r1.Created == r2.Created || !reflect.DeepEqual(r1.Op, r2.Op) || r1.Op.Version != 1 {
		t.Fatalf("same-input Begin: %+v / %+v; want one creator and one identical existing record", r1, r2)
	}

	r1, r2 = raceChildren(t, db, []string{"begin", "op-changed", "fp-a"}, []string{"begin", "op-changed", "fp-b"})
	winner := oneWinner(t, "conflicting Begin", "conflict", r1, r2)
	if !winner.Created {
		t.Fatalf("conflicting Begin winner did not create the record: %+v", winner)
	}
	got, ok, err := s.Get(ctx, "op-changed")
	if err != nil || !ok || !reflect.DeepEqual(got, winner.Op) {
		t.Fatalf("stored record %+v, %v, %v; want the winner %+v", got, ok, err, winner.Op)
	}
}

// Two OS processes Open a database file that does not exist yet at the same moment.
// Both must wait out the other's WAL conversion and schema creation instead of
// failing with SQLITE_BUSY, and the same operation then has exactly one creator.
// Several fresh files make the race likely.
func TestCrossProcessConcurrentOpenOfNewDatabase(t *testing.T) {
	for round := range 8 {
		db := filepath.Join(t.TempDir(), fmt.Sprintf("fresh-%d.db", round))
		r1, r2 := raceChildren(t, db, []string{"open-race", "op-open"}, []string{"open-race", "op-open"})
		if r1.Err != "" || r2.Err != "" || r1.Created == r2.Created || !reflect.DeepEqual(r1.Op, r2.Op) {
			t.Fatalf("round %d: concurrent first Open + Begin: %+v / %+v", round, r1, r2)
		}
	}
}

// E020: process A begins and checkpoints STAGED, then exits. Process B (this test
// process) reopens the file and recovers identity, phase, proof, version, attempts and
// lastError exactly; a stale update carrying A's pre-STAGED version is rejected. Two
// processes holding the same current version race an Update: one CAS wins, the other
// is stale, and the stored record is the winner's.
func TestCrossProcessReopenAndStaleUpdate(t *testing.T) {
	db := filepath.Join(t.TempDir(), "checkpoint.db")
	ctx := context.Background()

	cmd, out := startChild(t, db, "", "write-staged", "op-e020", "/staging/op-e020")
	a := waitChild(t, cmd, out)
	if a.Err != "" || !a.Created || a.Op.Phase != acquisition.PhaseStaged || a.Op.Version != 2 {
		t.Fatalf("writer process %d: %+v", a.PID, a)
	}
	t.Logf("writer pid=%d cmd=%q reader pid=%d store=%s", a.PID, a.Command, os.Getpid(), db)

	s := open(t, db)
	got, ok, err := s.Get(ctx, "op-e020")
	if err != nil || !ok || !reflect.DeepEqual(got, a.Op) {
		t.Fatalf("record after reopen in another process = %+v, %v, %v; want %+v", got, ok, err, a.Op)
	}
	want := stagedOp(checkpointtest.Operation("op-e020"), "/staging/op-e020")
	want.Version = 2
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered fields differ from what the writer checkpointed:\n got  %+v\n want %+v", got, want)
	}

	stale := checkpointtest.Operation("op-e020")
	stale.Version = 1 // A's pre-STAGED handle
	stale.LastError = "stale writer"
	if _, err := s.Update(ctx, stale); !errors.Is(err, acquisition.ErrCheckpointStale) {
		t.Fatalf("stale pre-reopen Update: err = %v, want ErrCheckpointStale", err)
	}
	if after, _, _ := s.Get(ctx, "op-e020"); !reflect.DeepEqual(after, got) {
		t.Fatalf("stale Update changed the record: %+v", after)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r1, r2 := raceChildren(t, db, []string{"update", "op-e020", "2", "writer-x"}, []string{"update", "op-e020", "2", "writer-y"})
	winner := oneWinner(t, "concurrent CAS Update", "stale", r1, r2)
	s = open(t, db)
	final, ok, err := s.Get(ctx, "op-e020")
	if err != nil || !ok || !reflect.DeepEqual(final, winner.Op) || final.Version != 3 {
		t.Fatalf("stored record %+v, %v, %v; want the winner %+v at version 3", final, ok, err, winner.Op)
	}
}
