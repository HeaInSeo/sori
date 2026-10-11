package sqlitecheckpoint_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/acquisition/sqlitecheckpoint"
)

// holdWriteLock opens a second connection to the checkpoint database at path. The
// returned conn can take the write lock with BEGIN IMMEDIATE; cleanup rolls it back.
func holdWriteLock(t *testing.T, path string) *sql.Conn {
	t.Helper()
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		_ = conn.Close()
	})
	return conn
}

// newCheckpointDB creates an initialized checkpoint database and closes it.
func newCheckpointDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := open(t, path).Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// While another connection holds the write lock, Open must give up within its busy
// budget: no attempt may start after the budget is spent or wait past it (sori#36).
// The budget is shortened here; the store's busy timeout stays 10 s.
func TestOpenReturnsWithinBusyBudget(t *testing.T) {
	path := newCheckpointDB(t)
	conn := holdWriteLock(t, path)
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	const budget = 300 * time.Millisecond
	defer sqlitecheckpoint.SetOpenBusyBudgetForTest(budget)()
	start := time.Now()
	s, err := sqlitecheckpoint.Open(path)
	elapsed := time.Since(start)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded while another connection held the write lock")
	}
	// Without the cap, the attempt waits the full 10 s busy timeout.
	if elapsed > budget+2*time.Second {
		t.Fatalf("Open took %v with a %v budget: %v", elapsed, budget, err)
	}
}

// The budget is one wall-clock deadline for the whole attempt, not a busy timeout
// granted again to each lock wait (sori#36). Here connecting spends most of the
// budget, as a slow WAL conversion would, and then another connection takes the
// write lock before init's BEGIN IMMEDIATE. Open must still give up at the
// deadline instead of waiting another full budget in init.
func TestOpenDeadlineSpansConnectAndInit(t *testing.T) {
	path := newCheckpointDB(t)
	conn := holdWriteLock(t, path)

	const budget = time.Second
	const connectCost = 800 * time.Millisecond
	defer sqlitecheckpoint.SetOpenBusyBudgetForTest(budget)()
	var locked bool
	defer sqlitecheckpoint.SetOpenAfterConnectForTest(func() {
		if !locked {
			time.Sleep(connectCost)
			if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
				t.Error(err)
			}
			locked = true
		}
	})()
	start := time.Now()
	s, err := sqlitecheckpoint.Open(path)
	elapsed := time.Since(start)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded while another connection held the write lock")
	}
	// Without one deadline, init waits a further full budget: about 1.8 s here.
	if limit := budget + 400*time.Millisecond; elapsed > limit {
		t.Fatalf("Open took %v with a %v budget (limit %v): %v", elapsed, budget, limit, err)
	}
}

// Connecting can itself wait for a lock: converting the file to WAL needs one. The
// busy timeout applied while connecting must be capped at the budget too, not the
// full 10 s (sori#36). Here another connection holds an exclusive lock on a
// rollback-journal file, so the connection's journal_mode pragma waits.
func TestOpenCapsBusyWaitWhileConnecting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	conn := holdWriteLock(t, path)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE lock_probe (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO lock_probe VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	const budget = 300 * time.Millisecond
	defer sqlitecheckpoint.SetOpenBusyBudgetForTest(budget)()
	start := time.Now()
	s, err := sqlitecheckpoint.Open(path)
	elapsed := time.Since(start)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded while another connection held an exclusive lock")
	}
	if elapsed > budget+2*time.Second {
		t.Fatalf("Open took %v with a %v budget: %v", elapsed, budget, err)
	}
}

// An attempt made with less than the full budget left runs with a shorter busy
// timeout; once Open succeeds the store's connection is back to the full 10 s.
func TestOpenRestoresFullBusyTimeout(t *testing.T) {
	defer sqlitecheckpoint.SetOpenBusyBudgetForTest(2 * time.Second)()
	s := open(t, filepath.Join(t.TempDir(), "checkpoint.db"))
	ms, err := s.BusyTimeoutForTest()
	if err != nil || ms != 10000 {
		t.Fatalf("busy_timeout = %d, %v; want 10000", ms, err)
	}
}
