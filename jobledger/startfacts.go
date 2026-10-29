package jobledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

// memoryPath is SQLite's name for a private in-memory database.
const memoryPath = ":memory:"

// StartFacts are the read-only facts a server start classifier needs about one
// job ledger (SORI-D01-W021B-FMT-PREP-r2 §9.1 P-b). They are reported as facts,
// not as the three offline-init outcomes of InspectInitState, so that "not
// content-empty", "RESTORED_INACTIVE marker" and "unknown schema" stay apart.
type StartFacts struct {
	// Pristine: no database file (and no sidecar), an empty file, or an SQLite
	// file with no schema object. A pristine path is reported, never created.
	Pristine bool
	// SchemaKnown: the schema objects are exactly this version's schema and
	// schema_version is this version. False for a pristine store.
	SchemaKnown bool
	// ContentEmpty: no row in any content table (every table but ledger_meta).
	// Meaningful only when SchemaKnown.
	ContentEmpty bool
	// Marked: installation_id is recorded. The remaining fields are meaningful
	// only when Marked.
	Marked         bool
	InstallationID string
	RestoreState   string
	ActiveEpoch    uint64
	CutSeq         uint64
	RestoreNonce   string
	// InitMarkerOnly: the markers are exactly the four Init markers for
	// InstallationID (LIVE, epoch 1, cut 0) and nothing else besides
	// schema_version.
	InitMarkerOnly bool
}

// InspectStartFacts reads the start facts of the store at path without
// writing anything: no file or sidecar is created, changed or removed. Like
// InspectInitState it never uses Open, which would create a schema or switch
// the journal mode. A non-empty file is read only through an immutable,
// read-only, query-only connection, which never creates or touches a -wal,
// -shm or -journal file. A store whose committed state is not all in the main
// file (a non-empty -wal or any -journal, as after a crash) cannot be read that
// way: reading it needs SQLite's WAL or journal recovery, which creates or
// rewrites the -shm index, so it returns an error (ErrNeedsRecovery) instead.
// A store that cannot be read with certainty (a non-regular file, a stray
// sidecar, a failed integrity check, an unreadable or malformed marker) also
// returns an error and no facts.
func InspectStartFacts(ctx context.Context, path string) (StartFacts, error) {
	if path == "" || path == memoryPath {
		return StartFacts{}, fmt.Errorf("%w: %q", ErrTransientPath, path)
	}
	pristine, err := inspectFile(path)
	switch {
	case err != nil:
		return StartFacts{}, fmt.Errorf("jobledger: start facts: %w", err)
	case pristine:
		return StartFacts{Pristine: true}, nil
	}
	if err := mainFileComplete(path); err != nil {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: %w", err)
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&immutable=1&_pragma=query_only(1)")
	if err != nil {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: read %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()
	return startFacts(ctx, tx, path)
}

// ErrNeedsRecovery reports a database whose committed state is not all in its
// main file: it has a non-empty -wal or a -journal. Reading it needs SQLite's
// WAL or journal recovery, which writes (it creates or rewrites the -shm
// index), so a reader that must not write cannot inspect it.
var ErrNeedsRecovery = errors.New("database needs WAL or journal recovery to be read")

// mainFileComplete returns nil when every committed page of the database at
// path is in its main file: there is no -journal and the -wal is absent or an
// empty regular file. Only then may the file be opened immutable: SQLite reads
// an immutable file without looking at, creating or touching a -wal, -shm or
// -journal, so a committed page that lived only in the -wal would be missed.
func mainFileComplete(path string) error {
	fi, err := os.Lstat(path + "-wal")
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("stat %s-wal: %w", path, err)
	case !fi.Mode().IsRegular() || fi.Size() != 0:
		return fmt.Errorf("%w: %s-wal is non-empty or not a regular file", ErrNeedsRecovery, path)
	}
	if _, err := os.Lstat(path + "-journal"); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s-journal exists", ErrNeedsRecovery, path)
	}
	return nil
}

func startFacts(ctx context.Context, tx *sql.Tx, path string) (StartFacts, error) {
	var check string
	if err := tx.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: check %s: %w", path, err)
	}
	if check != "ok" {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: check %s: %s", path, check)
	}
	objects, err := schemaObjects(ctx, tx)
	if err != nil {
		return StartFacts{}, err
	}
	if len(objects) == 0 {
		return StartFacts{Pristine: true}, nil
	}
	if err := checkSchemaObjects(ctx, tx); err != nil {
		if errors.Is(err, ErrSchemaVersion) {
			return StartFacts{}, nil
		}
		return StartFacts{}, err
	}
	meta, err := readMeta(ctx, tx)
	if err != nil {
		return StartFacts{}, err
	}
	if meta[metaSchemaVersion] != strconv.Itoa(SchemaVersion) {
		return StartFacts{}, nil
	}
	f := StartFacts{SchemaKnown: true}
	if f.ContentEmpty, err = contentEmpty(ctx, tx); err != nil {
		return StartFacts{}, err
	}
	id, ok := meta[metaInstallationID]
	if !ok {
		return f, nil
	}
	if id == "" {
		return StartFacts{}, errors.New("jobledger: start facts: empty installation_id marker")
	}
	f.Marked, f.InstallationID = true, id
	f.RestoreState, f.RestoreNonce = meta[metaRestoreState], meta[metaRestoreNonce]
	for key, dst := range map[string]*uint64{metaActiveEpoch: &f.ActiveEpoch, metaCutSeq: &f.CutSeq} {
		v, ok := meta[key]
		if !ok {
			return StartFacts{}, fmt.Errorf("jobledger: start facts: marker %s missing", key)
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != v {
			return StartFacts{}, fmt.Errorf("jobledger: start facts: marker %s = %q", key, v)
		}
		*dst = n
	}
	_, _, cerr := classifyMarkers(meta)
	f.InitMarkerOnly = cerr == nil
	return f, nil
}

// contentEmpty reports whether every content table is empty.
func contentEmpty(ctx context.Context, tx *sql.Tx) (bool, error) {
	err := checkNoContent(ctx, tx)
	switch {
	case err == nil:
		return true, nil
	case isContentFound(err):
		return false, nil
	}
	return false, err
}

// isContentFound reports whether err is checkNoContent's refusal of a
// non-empty content table; any other error is a read failure.
func isContentFound(err error) bool {
	var ce *contentError
	return errors.As(err, &ce)
}
