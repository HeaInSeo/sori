package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/authority/sqlitestore"
)

// An empty path or ":memory:" would give an in-memory or private temporary database
// that loses every acceptance on close, so Open refuses it (sori#34).
func TestOpenRejectsTransientPaths(t *testing.T) {
	for _, path := range []string{"", ":memory:"} {
		s, err := sqlitestore.Open(path)
		if !errors.Is(err, sqlitestore.ErrTransientPath) {
			if s != nil {
				_ = s.Close()
			}
			t.Fatalf("Open(%q): err = %v, want ErrTransientPath", path, err)
		}
	}
}

// While another connection holds the write lock, Open must give up within its busy
// budget: no attempt may start after the budget is spent or wait past it (PR #35
// review). The budget is shortened here; the store's busy timeout stays 10 s.
func TestOpenReturnsWithinBusyBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.db")
	s, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `ROLLBACK`) }()

	const budget = 300 * time.Millisecond
	defer sqlitestore.SetOpenBusyBudgetForTest(budget)()
	start := time.Now()
	s, err = sqlitestore.Open(path)
	elapsed := time.Since(start)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded while another connection held the write lock")
	}
	if elapsed > budget+2*time.Second {
		t.Fatalf("Open took %v with a %v budget: %v", elapsed, budget, err)
	}
}

// The budget is one wall-clock deadline for the whole attempt, not a busy timeout
// granted again to each lock wait (PR #35 review r4242961909). Here connecting
// spends most of the budget, as a slow WAL conversion would, and then another
// connection takes the write lock before init's BEGIN IMMEDIATE. Open must still
// give up at the deadline instead of waiting another full budget in init.
func TestOpenDeadlineSpansConnectAndInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.db")
	s, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer func() { _, _ = conn.ExecContext(ctx, `ROLLBACK`) }()

	const budget = time.Second
	const connectCost = 800 * time.Millisecond
	defer sqlitestore.SetOpenBusyBudgetForTest(budget)()
	var locked bool
	defer sqlitestore.SetOpenAfterConnectForTest(func() {
		if !locked {
			time.Sleep(connectCost)
			if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
				t.Error(err)
			}
			locked = true
		}
	})()
	start := time.Now()
	s, err = sqlitestore.Open(path)
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

// An attempt made with less than the full budget left runs with a shorter busy
// timeout; once Open succeeds the store's connection is back to the full 10 s.
func TestOpenRestoresFullBusyTimeout(t *testing.T) {
	defer sqlitestore.SetOpenBusyBudgetForTest(2 * time.Second)()
	s, err := sqlitestore.Open(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ms, err := s.BusyTimeoutForTest()
	if err != nil || ms != 10000 {
		t.Fatalf("busy_timeout = %d, %v; want 10000", ms, err)
	}
}

// A file name may contain characters that are significant in a URI. Open must
// still create and reopen exactly that file, with the durability pragmas applied,
// instead of parsing part of the name as DSN parameters, a fragment or an escape.
func TestOpenPathWithURICharacters(t *testing.T) {
	for _, name := range []string{
		"q?mode=memory&cache=shared",
		"frag#ment",
		"pct%41%2Fname",
		"space and ;semi",
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "authority.db")
			s := open(t, path)
			journal, sync, err := s.JournalModeForTest()
			if err != nil {
				t.Fatal(err)
			}
			if journal != "wal" || sync != 2 {
				t.Fatalf("journal_mode=%q synchronous=%d, want wal/2 (FULL)", journal, sync)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not created at exact path %q: %v", path, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if n := e.Name(); n != "authority.db" && n != "authority.db-wal" && n != "authority.db-shm" {
					t.Fatalf("unexpected file %q next to the database", n)
				}
			}
			// The reopened file carries this adapter's schema version.
			_ = open(t, path)
		})
	}
}

// rawDB opens path with the driver directly, outside the adapter.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func schemaObjects(t *testing.T, path string) []string {
	t.Helper()
	rows, err := rawDB(t, path).Query(`SELECT type || ' ' || name FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Open must read the stored version before running this version's DDL. A future
// schema whose tables lack the current columns, an unversioned database, and a
// meta table without a version are all refused with ErrSchemaVersion and left
// unchanged.
func TestOpenRefusesIncompatibleSchemaBeforeDDL(t *testing.T) {
	for name, ddl := range map[string]string{
		// A later version renamed bind_events.alias; this version's
		// CREATE INDEX ... (alias, seq) cannot run against it.
		"future version with renamed column": `
			CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
			INSERT INTO meta VALUES ('schema_version', '2');
			CREATE TABLE bind_events (seq INTEGER PRIMARY KEY, bind_request_id TEXT, alias_name TEXT, record TEXT);`,
		"unversioned foreign database": `
			CREATE TABLE bind_events (seq INTEGER PRIMARY KEY, alias_name TEXT);`,
		"meta without version": `
			CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "authority.db")
			if _, err := rawDB(t, path).Exec(ddl); err != nil {
				t.Fatal(err)
			}
			before := schemaObjects(t, path)
			if s, err := sqlitestore.Open(path); !errors.Is(err, sqlitestore.ErrSchemaVersion) {
				if s != nil {
					_ = s.Close()
				}
				t.Fatalf("Open: err = %v, want ErrSchemaVersion", err)
			}
			after := schemaObjects(t, path)
			if len(before) != len(after) {
				t.Fatalf("refused Open changed the schema: before %v, after %v", before, after)
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("refused Open changed the schema: before %v, after %v", before, after)
				}
			}
		})
	}
}
