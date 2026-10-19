package jobledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"sync"
)

// InitState is the read-only classification of a store before offline init
// (RW-C1 §5.1a).
type InitState int

const (
	// InitRefused: the store holds content, foreign objects, other markers or cannot
	// be read with certainty. Init must not write to it.
	InitRefused InitState = iota
	// InitPristine: no database file, or an SQLite file with no schema.
	InitPristine
	// InitSchemaOnly: exactly this version's schema, no content, only schema_version.
	InitSchemaOnly
	// InitMarkerEmpty: InitSchemaOnly plus exactly the four Init markers. Init
	// already ran; a rerun reuses the recorded installation id and skips Init.
	InitMarkerEmpty
)

func (s InitState) String() string {
	switch s {
	case InitPristine:
		return "PRISTINE"
	case InitSchemaOnly:
		return "SCHEMA_ONLY"
	case InitMarkerEmpty:
		return "INIT_MARKER_EMPTY"
	default:
		return "REFUSED"
	}
}

// InitInspection is the result of InspectInitState. InstallationID is set only for
// InitMarkerEmpty.
type InitInspection struct {
	State          InitState
	InstallationID string
}

// sqliteSidecars are the files SQLite keeps next to a database.
var sqliteSidecars = []string{"-wal", "-shm", "-journal"}

// InspectInitState classifies the store at path without changing it. It never
// uses Open, which creates a schema in an empty database. The caller holds the
// admin and server locks, so no writer runs meanwhile.
//
// The schema is compared object by object with the schema this version creates,
// not trusted from schema_version. A database file is read through a read-only
// connection; SQLite may still create its shared-memory index (-shm) to read a
// write-ahead log, which is bookkeeping, not store content. Anything that is not
// PRISTINE, SCHEMA_ONLY or INIT_MARKER_EMPTY is InitRefused with an error that
// wraps ErrInitRefused.
func InspectInitState(ctx context.Context, path string) (InitInspection, error) {
	if path == "" || path == ":memory:" {
		return InitInspection{State: InitRefused}, fmt.Errorf("%w: %q", ErrTransientPath, path)
	}
	state, id, err := inspectInit(ctx, path)
	if err != nil {
		return InitInspection{State: InitRefused}, fmt.Errorf("%w: %w", ErrInitRefused, err)
	}
	return InitInspection{State: state, InstallationID: id}, nil
}

func inspectInit(ctx context.Context, path string) (InitState, string, error) {
	pristine, err := inspectFile(path)
	switch {
	case err != nil:
		return InitRefused, "", err
	case pristine:
		return InitPristine, "", nil
	}
	return inspectDatabase(ctx, path)
}

// inspectFile reports whether path is pristine before any SQLite open: absent with
// no sidecar, or an empty regular file with no non-empty sidecar. A non-empty
// regular file is left to inspectDatabase.
func inspectFile(path string) (bool, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		for _, suffix := range sqliteSidecars {
			if _, err := os.Lstat(path + suffix); !errors.Is(err, fs.ErrNotExist) {
				return false, fmt.Errorf("%s exists without its database", path+suffix)
			}
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("stat %s: %w", path, err)
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("%s is not a regular file (%s)", path, fi.Mode().Type())
	case fi.Size() != 0:
		return false, nil
	}
	for _, suffix := range sqliteSidecars {
		sfi, err := os.Lstat(path + suffix)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return false, fmt.Errorf("stat %s: %w", path+suffix, err)
		case sfi.Size() > 0:
			return false, fmt.Errorf("empty %s has a non-empty %s", path, suffix)
		}
	}
	return true, nil
}

// inspectDatabase classifies a non-empty database file through a read-only
// connection.
func inspectDatabase(ctx context.Context, path string) (InitState, string, error) {
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return InitRefused, "", fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return InitRefused, "", fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()

	var check string
	if err := tx.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return InitRefused, "", fmt.Errorf("check %s: %w", path, err)
	}
	if check != "ok" {
		return InitRefused, "", fmt.Errorf("check %s: %s", path, check)
	}
	objects, err := schemaObjects(ctx, tx)
	if err != nil {
		return InitRefused, "", err
	}
	if len(objects) == 0 {
		return InitPristine, "", nil
	}
	if err := checkSchemaObjects(ctx, tx); err != nil {
		return InitRefused, "", err
	}
	if err := checkNoContent(ctx, tx); err != nil {
		return InitRefused, "", err
	}
	meta, err := readMeta(ctx, tx)
	if err != nil {
		return InitRefused, "", err
	}
	return classifyMarkers(meta)
}

// classifyMarkers accepts exactly schema_version, or schema_version plus exactly
// the four Init markers.
func classifyMarkers(meta map[string]string) (InitState, string, error) {
	if meta[metaSchemaVersion] != strconv.Itoa(SchemaVersion) {
		return InitRefused, "", fmt.Errorf("schema_version %q (want %d)", meta[metaSchemaVersion], SchemaVersion)
	}
	if len(meta) == 1 {
		return InitSchemaOnly, "", nil
	}
	id := meta[metaInstallationID]
	if id == "" || len(meta) != 1+len(initMarkers(id)) {
		return InitRefused, "", errors.New("markers are not exactly the init markers")
	}
	for k, v := range initMarkers(id) {
		if meta[k] != v {
			return InitRefused, "", fmt.Errorf("marker %s = %q (want %q)", k, meta[k], v)
		}
	}
	return InitMarkerEmpty, id, nil
}

// schemaObject is one row of sqlite_schema.
type schemaObject struct{ typ, name, table, sql string }

// schemaObjects lists every sqlite_schema row except the automatic indexes behind
// PRIMARY KEY and UNIQUE, whose names SQLite assigns. Other internal objects such
// as sqlite_sequence or sqlite_stat1 are listed: this schema creates none, so one
// present means the file was used for something else.
func schemaObjects(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]schemaObject, error) {
	rows, err := q.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite\_autoindex\_%' ESCAPE '\' ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("jobledger: read schema: %w", err)
	}
	var out []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.sql); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("jobledger: read schema: %w", err)
		}
		out = append(out, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("jobledger: read schema: %w", err)
	}
	return out, nil
}

// expectedSchema is the object list this version's schema creates, read from a
// private in-memory database once.
var expectedSchema = sync.OnceValues(func() ([]schemaObject, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("jobledger: reference schema: %w", err)
	}
	return schemaObjects(ctx, db)
})

// checkSchemaObjects refuses a database whose objects are not exactly this
// version's schema: a missing, extra or altered table, index, view or trigger.
func checkSchemaObjects(ctx context.Context, tx *sql.Tx) error {
	want, err := expectedSchema()
	if err != nil {
		return err
	}
	got, err := schemaObjects(ctx, tx)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("%w: schema objects differ from version %d", ErrSchemaVersion, SchemaVersion)
	}
	return nil
}

// contentTables are the tables whose rows are store content: every table of this
// version's schema except ledger_meta, which alone may hold rows in a store that
// offline init may touch.
func contentTables() ([]string, error) {
	objects, err := expectedSchema()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, o := range objects {
		if o.typ == "table" && o.name != "ledger_meta" {
			out = append(out, o.name)
		}
	}
	return out, nil
}

// checkNoContent refuses a store with a row in any content table.
func checkNoContent(ctx context.Context, tx *sql.Tx) error {
	tables, err := contentTables()
	if err != nil {
		return err
	}
	for _, table := range tables {
		var n int
		// table comes from the reference schema, never from input.
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+`)`).Scan(&n); err != nil {
			return fmt.Errorf("jobledger: read %s: %w", table, err)
		}
		if n != 0 {
			return fmt.Errorf("store has content in %s", table)
		}
	}
	return nil
}

// readMeta returns every ledger_meta row.
func readMeta(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM ledger_meta`)
	if err != nil {
		return nil, fmt.Errorf("jobledger: read markers: %w", err)
	}
	meta := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("jobledger: read markers: %w", err)
		}
		meta[k] = v
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("jobledger: read markers: %w", err)
	}
	return meta, nil
}
