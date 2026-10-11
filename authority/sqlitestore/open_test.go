package sqlitestore_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HeaInSeo/sori/authority/sqlitestore"
)

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
