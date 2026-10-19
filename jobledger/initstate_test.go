package jobledger_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HeaInSeo/sori/jobledger"
)

// schemaOnly returns the path of a closed store that Open created and Init never
// touched.
func schemaOnly(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := jobledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// mutated returns the path of a closed schema-only store after stmt ran on it.
func mutated(t *testing.T, stmt string) string {
	t.Helper()
	path := schemaOnly(t)
	s := open(t, path)
	if err := s.ExecForTest(stmt); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// internalObjects are statements that leave SQLite internal objects this schema
// never creates: an AUTOINCREMENT counter table, with and without a row, and the
// planner statistics of ANALYZE.
var internalObjects = map[string]string{
	"leftover sqlite_sequence": `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE t`,
	"sqlite_sequence row":      `CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE t; INSERT INTO sqlite_sequence (name, seq) VALUES ('ghost', 7)`,
	"analyze statistics":       `ANALYZE`,
}

// contentRows puts one row in each content table, with foreign keys off so that
// each table is the only one with a row.
var contentRows = map[string]string{
	"ledger":        `INSERT INTO ledger (seq, kind, subject, record, digest) VALUES (1, 'namespace', 'n', '{}', 'd')`,
	"namespaces":    `INSERT INTO namespaces (namespace_id, record) VALUES ('n', '{}')`,
	"idempotency":   `INSERT INTO idempotency (namespace_id, client_key, meaning_digest, job_id, domain_op_key, record) VALUES ('n', 'k', 'm', 'j', 'o', '{}')`,
	"jobs":          `INSERT INTO jobs (job_id, record_version, record) VALUES ('j', 1, '{}')`,
	"policy":        `INSERT INTO policy (resource, revision, record) VALUES ('r', 1, '{}')`,
	"policy_events": `INSERT INTO policy_events (seq, resource, record) VALUES (1, 'r', '{}')`,
	"prepared":      `INSERT INTO prepared (prepared_id, operation_key, record) VALUES ('p', 'o', '{}')`,
	"bindings":      `INSERT INTO bindings (binding_id, prepared_id, revision_id, record_version, record) VALUES ('b', 'p', 'r', 1, '{}')`,
}

// TestContentRowsCoverEverySchemaTable keeps contentRows in step with the schema,
// so a table added later is also tested as content.
func TestContentRowsCoverEverySchemaTable(t *testing.T) {
	s := open(t, schemaOnly(t))
	var n int
	if err := s.QueryRowForTest(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name <> 'ledger_meta'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(contentRows) {
		t.Fatalf("schema has %d content tables, contentRows covers %d", n, len(contentRows))
	}
	for table := range contentRows {
		var ok bool
		if err := s.QueryRowForTest(`SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?)`, table).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("contentRows names %s, which the schema does not have", table)
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
func inspectUnchanged(t *testing.T, path string) (jobledger.InitInspection, error) {
	t.Helper()
	dir := filepath.Dir(path)
	before := dirFiles(t, dir)
	got, err := jobledger.InspectInitState(context.Background(), path)
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
	if got, err := inspectUnchanged(t, absent); err != nil || got.State != jobledger.InitPristine {
		t.Fatalf("absent: %v %v", got.State, err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect created the absent database: %v", err)
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := inspectUnchanged(t, empty); err != nil || got.State != jobledger.InitPristine {
		t.Fatalf("empty file: %v %v", got.State, err)
	}
	orphan := filepath.Join(dir, "orphan.db")
	if err := os.WriteFile(orphan+"-wal", []byte("frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := inspectUnchanged(t, orphan); !errors.Is(err, jobledger.ErrInitRefused) || got.State != jobledger.InitRefused {
		t.Fatalf("wal without database: %v %v", got.State, err)
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		emptyWithLog := filepath.Join(t.TempDir(), "ledger.db")
		if err := os.WriteFile(emptyWithLog, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(emptyWithLog+suffix, []byte("frames"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := inspectUnchanged(t, emptyWithLog); !errors.Is(err, jobledger.ErrInitRefused) || got.State != jobledger.InitRefused {
			t.Fatalf("empty database with a non-empty %s: %v %v", suffix, got.State, err)
		}
	}
	if got, err := jobledger.InspectInitState(context.Background(), ":memory:"); !errors.Is(err, jobledger.ErrTransientPath) || got.State != jobledger.InitRefused {
		t.Fatalf(":memory: %v %v", got.State, err)
	}
}

func TestInspectInitStateSchemaOnlyAndMarkerEmpty(t *testing.T) {
	path := schemaOnly(t)
	if got, err := inspectUnchanged(t, path); err != nil || got.State != jobledger.InitSchemaOnly || got.InstallationID != "" {
		t.Fatalf("schema-only: %+v %v", got, err)
	}

	s := open(t, path)
	if err := s.Init(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A rerun after a lost Init acknowledgement reuses the recorded id.
	for range 2 {
		got, err := inspectUnchanged(t, path)
		if err != nil || got.State != jobledger.InitMarkerEmpty || got.InstallationID != installation {
			t.Fatalf("marker-empty: %+v %v", got, err)
		}
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
	withContent := func() string {
		path := filepath.Join(t.TempDir(), "ledger.db")
		s := open(t, path)
		if err := s.Init(context.Background(), installation); err != nil {
			t.Fatal(err)
		}
		mustNamespace(t, s, guard1)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// A schema-only store whose empty bindings_revision index page reports
	// fragmented bytes it does not have. The page still parses and no content query
	// reads it, so only the integrity check sees it.
	corruptIndex := func() string {
		path := schemaOnly(t)
		s := open(t, path)
		var root, pageSize int64
		if err := s.QueryRowForTest(`SELECT (SELECT rootpage FROM sqlite_schema WHERE name = 'bindings_revision'), (SELECT page_size FROM pragma_page_size)`).Scan(&root, &pageSize); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
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
	cases := map[string]string{
		"corrupt index page":  corruptIndex(),
		"not sqlite":          notDB,
		"symlink":             link,
		"directory":           t.TempDir(),
		"content":             withContent(),
		"foreign table":       mutated(t, `CREATE TABLE foreign_data (x INTEGER)`),
		"foreign index":       mutated(t, `CREATE INDEX extra ON jobs (record)`),
		"foreign view":        mutated(t, `CREATE VIEW v AS SELECT 1`),
		"dropped index":       mutated(t, `DROP INDEX bindings_revision`),
		"other version":       mutated(t, `UPDATE ledger_meta SET value = '99' WHERE key = 'schema_version'`),
		"no version":          mutated(t, `DELETE FROM ledger_meta`),
		"restore nonce":       mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('restore_nonce', 'n')`),
		"ledger head":         mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('ledger_head', '0:')`),
		"unknown marker":      mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('x', 'y')`),
		"partial markers":     mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('installation_id', 'i')`),
		"legacy init ledger":  mutated(t, `INSERT INTO ledger (seq, kind, subject, record, digest) VALUES (1, 'init', 'i', '{}', 'd')`),
		"policy row":          mutated(t, `INSERT INTO policy (resource, revision, record) VALUES ('r', 1, '{}')`),
		"wrong epoch marker":  mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('installation_id', 'i'), ('restore_state', 'LIVE'), ('store_active_epoch', '2'), ('store_cut_seq', '0')`),
		"restored marker":     mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('installation_id', 'i'), ('restore_state', 'RESTORED_INACTIVE'), ('store_active_epoch', '1'), ('store_cut_seq', '0')`),
		"empty installation":  mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('installation_id', ''), ('restore_state', 'LIVE'), ('store_active_epoch', '1'), ('store_cut_seq', '0')`),
		"markers plus a head": mutated(t, `INSERT INTO ledger_meta (key, value) VALUES ('installation_id', 'i'), ('restore_state', 'LIVE'), ('store_active_epoch', '1'), ('store_cut_seq', '0'), ('ledger_head', '0:')`),
	}
	for name, stmt := range internalObjects {
		cases[name] = mutated(t, stmt)
	}
	for table, stmt := range contentRows {
		cases["row in "+table] = mutated(t, `PRAGMA foreign_keys = OFF; `+stmt)
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := inspectUnchanged(t, path)
			if !errors.Is(err, jobledger.ErrInitRefused) || got.State != jobledger.InitRefused || got.InstallationID != "" {
				t.Fatalf("got %+v %v, want REFUSED", got, err)
			}
		})
	}
}

func TestInitIsMarkerOnly(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if seq, digest, err := s.LedgerHead(ctx); err != nil || seq != 0 || digest != "" {
		t.Fatalf("ledger head after Init = %d %q %v, want empty ledger", seq, digest, err)
	}
	var rows, metas int
	if err := s.QueryRowForTest(`SELECT (SELECT COUNT(*) FROM ledger), (SELECT COUNT(*) FROM ledger_meta)`).Scan(&rows, &metas); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || metas != 5 {
		t.Fatalf("after Init: %d ledger rows, %d markers; want 0 and schema_version + 4", rows, metas)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatalf("verify empty ledger: %v", err)
	}
	// The first protected mutation starts the chain at seq 1.
	mustNamespace(t, s, guard1)
	if seq, _, err := s.LedgerHead(ctx); err != nil || seq != 1 {
		t.Fatalf("ledger head after first mutation = %d %v, want 1", seq, err)
	}
	if err := s.VerifyLedger(ctx); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestInitRefusesNonEmptyStoreUnchanged(t *testing.T) {
	cases := map[string]string{
		"foreign table":  `CREATE TABLE foreign_data (x INTEGER)`,
		"dropped index":  `DROP INDEX policy_events_resource`,
		"content row":    `INSERT INTO policy (resource, revision, record) VALUES ('r', 1, '{}')`,
		"legacy ledger":  `INSERT INTO ledger (seq, kind, subject, record, digest) VALUES (1, 'init', 'i', '{}', 'd')`,
		"restore nonce":  `INSERT INTO ledger_meta (key, value) VALUES ('restore_nonce', 'n')`,
		"ledger head":    `INSERT INTO ledger_meta (key, value) VALUES ('ledger_head', '0:')`,
		"unknown marker": `INSERT INTO ledger_meta (key, value) VALUES ('x', 'y')`,
	}
	for name, stmt := range internalObjects {
		cases[name] = stmt
	}
	for table, stmt := range contentRows {
		cases["row in "+table] = `PRAGMA foreign_keys = OFF; ` + stmt
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			path := mutated(t, stmt)
			s := open(t, path)
			ctx := context.Background()
			if err := s.Init(ctx, installation); !errors.Is(err, jobledger.ErrActivationRefused) {
				t.Fatalf("Init: %v, want ErrActivationRefused", err)
			}
			var n int
			if err := s.QueryRowForTest(`SELECT COUNT(*) FROM ledger_meta WHERE key IN ('installation_id', 'restore_state', 'store_active_epoch', 'store_cut_seq')`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("refused Init wrote %d markers", n)
			}
		})
	}
}
