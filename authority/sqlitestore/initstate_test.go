package sqlitestore_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HeaInSeo/sori/authority"
	"github.com/HeaInSeo/sori/authority/sqlitestore"
)

// schemaOnly returns the path of a closed authority database that Open created
// and nothing wrote to.
func schemaOnly(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authority.db")
	s, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// mutated returns the path of a closed schema-only database after stmt ran on it.
func mutated(t *testing.T, stmt string) string {
	t.Helper()
	path := schemaOnly(t)
	db := rawDB(t, path)
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// initInternalObjects are statements that leave SQLite internal objects this
// schema never creates: an AUTOINCREMENT counter table, with and without a row,
// and the planner statistics of ANALYZE.
var initInternalObjects = map[string]string{
	"leftover sqlite_sequence": `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE t`,
	"sqlite_sequence row":      `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE t; INSERT INTO sqlite_sequence (name, seq) VALUES ('ghost', 7)`,
	"analyze statistics":       `ANALYZE`,
}

// initContentRows puts one row in each content table, with foreign keys off so
// that each table is the only one with a row.
var initContentRows = map[string]string{
	"revisions":       `INSERT INTO revisions (seq, revision_id, request_id, asset_id, fingerprint, record) VALUES (1, 'r', 'q', 'a', 'f', '{}')`,
	"bind_events":     `INSERT INTO bind_events (seq, bind_request_id, alias, record) VALUES (1, 'b', 'x', '{}')`,
	"representations": `INSERT INTO representations (seq, representation_id, attach_op_id, revision_id, fingerprint, record) VALUES (1, 'p', 'o', 'r', 'f', '{}')`,
}

// TestInitContentRowsCoverEverySchemaTable keeps initContentRows in step with the
// schema, so a table added later is also tested as content.
func TestInitContentRowsCoverEverySchemaTable(t *testing.T) {
	db := rawDB(t, schemaOnly(t))
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name <> 'meta'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(initContentRows) {
		t.Fatalf("schema has %d content tables, initContentRows covers %d", n, len(initContentRows))
	}
	for table := range initContentRows {
		var ok bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?)`, table).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("initContentRows names %s, which the schema does not have", table)
		}
	}
}

// dirFiles maps each regular file name in dir to its bytes.
func dirFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

// inspectUnchanged runs InspectInitState and fails if the database file or any
// non-empty sidecar changed. An empty -shm/-wal created to read the file is
// bookkeeping, not store content.
func inspectUnchanged(t *testing.T, path string) (sqlitestore.InitState, error) {
	t.Helper()
	dir := filepath.Dir(path)
	before := dirFiles(t, dir)
	got, err := sqlitestore.InspectInitState(context.Background(), path)
	after := dirFiles(t, dir)
	for name, b := range after {
		was, ok := before[name]
		switch {
		case ok && !bytes.Equal(was, b):
			t.Errorf("inspect changed %s", name)
		case !ok && name != filepath.Base(path)+"-shm" && len(b) != 0:
			t.Errorf("inspect created %s with %d bytes", name, len(b))
		case !ok && name == filepath.Base(path):
			t.Errorf("inspect created the database file")
		}
	}
	return got, err
}

func TestInspectInitStatePristine(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "absent.db")
	if got, err := inspectUnchanged(t, absent); err != nil || got != sqlitestore.InitPristine {
		t.Fatalf("absent: %v %v", got, err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect created the absent database: %v", err)
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := inspectUnchanged(t, empty); err != nil || got != sqlitestore.InitPristine {
		t.Fatalf("empty file: %v %v", got, err)
	}
	// A non-empty SQLite file with no schema objects left, as after a table was
	// created and dropped.
	noSchema := filepath.Join(t.TempDir(), "authority.db")
	db := rawDB(t, noSchema)
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER); DROP TABLE t`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(noSchema); err != nil {
		t.Fatal(err)
	} else if fi.Size() == 0 {
		t.Fatal("no-schema file is empty; the case needs a non-empty SQLite file")
	}
	if got, err := inspectUnchanged(t, noSchema); err != nil || got != sqlitestore.InitPristine {
		t.Fatalf("SQLite file with no schema: %v %v", got, err)
	}
	orphan := filepath.Join(dir, "orphan.db")
	if err := os.WriteFile(orphan+"-wal", []byte("frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := inspectUnchanged(t, orphan); !errors.Is(err, sqlitestore.ErrInitRefused) || got != sqlitestore.InitRefused {
		t.Fatalf("wal without database: %v %v", got, err)
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		emptyWithLog := filepath.Join(t.TempDir(), "authority.db")
		if err := os.WriteFile(emptyWithLog, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(emptyWithLog+suffix, []byte("frames"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := inspectUnchanged(t, emptyWithLog); !errors.Is(err, sqlitestore.ErrInitRefused) || got != sqlitestore.InitRefused {
			t.Fatalf("empty database with a non-empty %s: %v %v", suffix, got, err)
		}
	}
	if got, err := sqlitestore.InspectInitState(context.Background(), ":memory:"); !errors.Is(err, sqlitestore.ErrTransientPath) || got != sqlitestore.InitRefused {
		t.Fatalf(":memory: %v %v", got, err)
	}
}

func TestInspectInitStateSchemaOnly(t *testing.T) {
	path := schemaOnly(t)
	for range 2 {
		if got, err := inspectUnchanged(t, path); err != nil || got != sqlitestore.InitSchemaOnly {
			t.Fatalf("schema-only: %v %v", got, err)
		}
	}
	// Inspect leaves the database as Open made it: a later Open still accepts it.
	s, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("reopen after inspect: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectInitStateRefused(t *testing.T) {
	dir := t.TempDir()
	notDB := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(notDB, bytes.Repeat([]byte("not sqlite "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(schemaOnly(t), link); err != nil {
		t.Fatal(err)
	}
	// A schema-only database whose empty bind_events_alias index page reports
	// fragmented bytes it does not have. The page still parses and no content query
	// reads it, so only the integrity check sees it.
	corruptIndex := func() string {
		path := schemaOnly(t)
		db := rawDB(t, path)
		var root, pageSize int64
		if err := db.QueryRow(`SELECT (SELECT rootpage FROM sqlite_schema WHERE name = 'bind_events_alias'), (SELECT page_size FROM pragma_page_size)`).Scan(&root, &pageSize); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte{5}, (root-1)*pageSize+7); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// A database some other program wrote: objects but no meta table.
	foreignOnly := func() string {
		path := filepath.Join(t.TempDir(), "authority.db")
		db := rawDB(t, path)
		if _, err := db.Exec(`CREATE TABLE foreign_data (x INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := map[string]string{
		"corrupt index page": corruptIndex(),
		"not sqlite":         notDB,
		"symlink":            link,
		"directory":          t.TempDir(),
		"foreign only":       foreignOnly(),
		"foreign table":      mutated(t, `CREATE TABLE foreign_data (x INTEGER)`),
		"foreign index":      mutated(t, `CREATE INDEX extra ON revisions (record)`),
		"foreign view":       mutated(t, `CREATE VIEW v AS SELECT 1`),
		"foreign trigger":    mutated(t, `CREATE TRIGGER tr AFTER INSERT ON meta BEGIN SELECT 1; END`),
		"dropped index":      mutated(t, `DROP INDEX representations_revision`),
		"other version":      mutated(t, `UPDATE meta SET value = '99' WHERE key = 'schema_version'`),
		"no version":         mutated(t, `DELETE FROM meta`),
		"extra meta row":     mutated(t, `INSERT INTO meta (key, value) VALUES ('installation_id', 'i')`),
	}
	for name, stmt := range initInternalObjects {
		cases[name] = mutated(t, stmt)
	}
	for table, stmt := range initContentRows {
		cases["row in "+table] = mutated(t, `PRAGMA foreign_keys = OFF; `+stmt)
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := inspectUnchanged(t, path)
			if !errors.Is(err, sqlitestore.ErrInitRefused) || got != sqlitestore.InitRefused {
				t.Fatalf("got %v %v, want REFUSED", got, err)
			}
		})
	}
}

// TestInspectInitStateRefusesStoreWithContent uses the store's own API, not raw
// SQL, to make the content.
func TestInspectInitStateRefusesStoreWithContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.db")
	s, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	req := authority.AcceptRequest{RequestID: "init-req", AssetID: xpAsset, Manifest: xpManifest("d1")}
	if _, err := s.AcceptRevision(context.Background(), req, "fp-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := inspectUnchanged(t, path); !errors.Is(err, sqlitestore.ErrInitRefused) || got != sqlitestore.InitRefused {
		t.Fatalf("got %v %v, want REFUSED", got, err)
	}
}
