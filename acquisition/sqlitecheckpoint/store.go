// Package sqlitecheckpoint is the durable SQLite/WAL adapter for
// acquisition.CheckpointStore (W01.2).
//
// It is the J1 durable single-writer local profile (CAD-r1 D01.1-5), with the same
// commit boundary as authority/sqlitestore: the database and its WAL live on
// host-local disk, every mutation runs in one BEGIN IMMEDIATE transaction, and a
// result is returned only after that transaction has committed with
// synchronous=FULL. It reuses the MemoryCheckpointStore semantics through the
// backend-neutral checkpointtest suite: Begin creates or returns the record and
// never overwrites a different fingerprint, and Update is a record-version CAS
// that refuses identity changes.
//
// The version CAS rejects a stale writer's checkpoint update. It is not an
// ownership fence: it does not stop a replaced executor's external effects.
//
// The checkpoint database is its own file; it does not share a file with the
// authority store.
package sqlitecheckpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" database/sql driver
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/HeaInSeo/sori/acquisition"
)

// SchemaVersion is the on-disk schema this adapter reads and writes.
const SchemaVersion = 1

// ErrSchemaVersion is returned by Open when the database was written by a different
// schema version, or is not an empty or checkpoint database. The adapter never
// migrates implicitly.
var ErrSchemaVersion = errors.New("sqlitecheckpoint: unsupported schema version")

const schema = `
CREATE TABLE IF NOT EXISTS checkpoint_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS operations (
	operation_id TEXT PRIMARY KEY,
	fingerprint  TEXT NOT NULL,
	version      INTEGER NOT NULL,
	record       TEXT NOT NULL
) STRICT;
`

// Store is a durable acquisition.CheckpointStore backed by one SQLite database file.
type Store struct {
	db   *sql.DB
	path string

	// beforeCommit, when non-nil, runs after a mutation's writes and just before its
	// COMMIT; a non-nil error rolls the whole transaction back. afterCommit runs just
	// after a successful COMMIT, before the result is returned. Test-only fault hooks.
	beforeCommit func() error
	afterCommit  func()
}

var _ acquisition.CheckpointStore = (*Store)(nil)

// ErrTransientPath is returned by Open for a path SQLite would treat as an
// in-memory or private temporary database, which loses every checkpoint when the
// connection closes.
var ErrTransientPath = errors.New("sqlitecheckpoint: path names a non-durable database")

// openBusyBudget bounds how long Open retries a SQLITE_BUSY from opening and
// initializing the database. It equals the busy timeout.
const openBusyBudget = 10 * time.Second

// Open opens (creating if needed) the store at path and checks its schema version.
//
// While another process is converting a new database file to WAL, SQLite can
// return SQLITE_BUSY without consulting the busy handler. Open therefore retries
// the whole open-and-init step on SQLITE_BUSY within openBusyBudget; any other
// error is returned at once.
func Open(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("%w: %q", ErrTransientPath, path)
	}
	deadline := time.Now().Add(openBusyBudget)
	for {
		s, err := open(path)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return s, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

func open(path string) (*Store, error) {
	q := url.Values{}
	// The driver applies _busy_timeout before any _pragma, so a process that loses
	// the race to convert a new database to WAL waits instead of failing with
	// SQLITE_BUSY.
	q.Set("_busy_timeout", "10000")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", fileURI(path)+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sqlitecheckpoint: open %s: %w", path, err)
	}
	// One connection per process: writes in this process are serialized here, and
	// BEGIN IMMEDIATE plus busy_timeout serializes them against other processes.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

// fileURI returns the SQLite "file:" URI naming exactly path, percent-encoding
// URI-significant characters so they stay part of the file name.
func fileURI(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath()
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database. A later Open of the same path sees every committed write.
func (s *Store) Close() error { return s.db.Close() }

// init checks the stored schema version before touching the schema, and runs this
// version's DDL only for a genuinely empty database. Any other database is refused
// with ErrSchemaVersion and left unchanged.
func (s *Store) init(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var objects, hasMeta int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(type = 'table' AND name = 'checkpoint_meta'), 0)
			FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`).Scan(&objects, &hasMeta)
		if err != nil {
			return fmt.Errorf("sqlitecheckpoint: probe schema: %w", err)
		}
		if objects == 0 {
			if _, err := tx.ExecContext(ctx, schema); err != nil {
				return fmt.Errorf("sqlitecheckpoint: create schema: %w", err)
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO checkpoint_meta (key, value) VALUES ('schema_version', ?)`, fmt.Sprint(SchemaVersion))
			return err
		}
		if hasMeta == 0 {
			return fmt.Errorf("%w: database has no checkpoint schema version", ErrSchemaVersion)
		}
		var v string
		err = tx.QueryRowContext(ctx, `SELECT value FROM checkpoint_meta WHERE key = 'schema_version'`).Scan(&v)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: database has no checkpoint schema version", ErrSchemaVersion)
		case err != nil:
			return fmt.Errorf("sqlitecheckpoint: read schema version: %w", err)
		case v != fmt.Sprint(SchemaVersion):
			return fmt.Errorf("%w: %s (want %d)", ErrSchemaVersion, v, SchemaVersion)
		}
		return nil
	})
}

// inTx runs fn in one IMMEDIATE transaction and commits it. fn's error rolls back
// every write, so callers never observe a partial mutation.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitecheckpoint: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitecheckpoint: commit: %w", err)
	}
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}

// queryRower is satisfied by both *sql.DB and *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func get(ctx context.Context, q queryRower, id acquisition.OperationID) (acquisition.Operation, bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM operations WHERE operation_id = ?`, string(id)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return acquisition.Operation{}, false, nil
	}
	if err != nil {
		return acquisition.Operation{}, false, err
	}
	op, err := decode(raw)
	return op, err == nil, err
}

func decode(raw string) (acquisition.Operation, error) {
	var op acquisition.Operation
	if err := json.Unmarshal([]byte(raw), &op); err != nil {
		return op, fmt.Errorf("sqlitecheckpoint: decode record: %w", err)
	}
	// Match MemoryCheckpointStore's cloneOperation: an empty list reads back as nil.
	if len(op.Members) == 0 {
		op.Members = nil
	}
	if len(op.StagedMembers) == 0 {
		op.StagedMembers = nil
	}
	return op, nil
}

// encode returns the stored JSON and the decoded copy that is handed back, so the
// caller's value and the result share no slices with committed truth.
func encode(op acquisition.Operation) (string, acquisition.Operation, error) {
	b, err := json.Marshal(op)
	if err != nil {
		return "", acquisition.Operation{}, fmt.Errorf("sqlitecheckpoint: encode record: %w", err)
	}
	out, err := decode(string(b))
	return string(b), out, err
}

// Begin implements acquisition.CheckpointStore.
func (s *Store) Begin(ctx context.Context, op acquisition.Operation) (acquisition.Operation, bool, error) {
	var out acquisition.Operation
	var created bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		existing, found, err := get(ctx, tx, op.ID)
		if err != nil {
			return err
		}
		if found {
			if existing.Fingerprint != op.Fingerprint {
				return fmt.Errorf("%w: operation %q", acquisition.ErrOperationConflict, op.ID)
			}
			out = existing
			return nil
		}
		op.Version = 1
		rec, stored, err := encode(op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO operations (operation_id, fingerprint, version, record) VALUES (?, ?, ?, ?)`,
			string(op.ID), op.Fingerprint, op.Version, rec); err != nil {
			return err
		}
		out, created = stored, true
		return nil
	})
	if err != nil {
		return acquisition.Operation{}, false, err
	}
	return out, created, nil
}

// Update implements acquisition.CheckpointStore.
func (s *Store) Update(ctx context.Context, op acquisition.Operation) (acquisition.Operation, error) {
	var out acquisition.Operation
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		existing, found, err := get(ctx, tx, op.ID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: unknown operation %q", acquisition.ErrCheckpointStale, op.ID)
		}
		if existing.Version != op.Version {
			return fmt.Errorf("%w: operation %q at version %d, update from %d", acquisition.ErrCheckpointStale, op.ID, existing.Version, op.Version)
		}
		if existing.Fingerprint != op.Fingerprint || existing.Subject != op.Subject || existing.PublicationRequestID != op.PublicationRequestID {
			return fmt.Errorf("%w: operation %q identity fields are immutable", acquisition.ErrOperationConflict, op.ID)
		}
		op.Version = existing.Version + 1
		rec, stored, err := encode(op)
		if err != nil {
			return err
		}
		// The version predicate repeats the CAS in SQL; inside the IMMEDIATE
		// transaction it always matches, so zero rows would mean a broken invariant.
		res, err := tx.ExecContext(ctx,
			`UPDATE operations SET version = ?, record = ? WHERE operation_id = ? AND version = ?`,
			op.Version, rec, string(op.ID), existing.Version)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.Join(fmt.Errorf("%w: operation %q changed during update", acquisition.ErrCheckpointStale, op.ID), err)
		}
		out = stored
		return nil
	})
	if err != nil {
		return acquisition.Operation{}, err
	}
	return out, nil
}

// Get implements acquisition.CheckpointStore.
func (s *Store) Get(ctx context.Context, id acquisition.OperationID) (acquisition.Operation, bool, error) {
	return get(ctx, s.db, id)
}
