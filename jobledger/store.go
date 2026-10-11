// Package jobledger is the durable Job ledger and policy history for the J1 local
// profile (W01.3).
//
// It records, in one SQLite/WAL database on host-local disk, the atomic acceptance
// boundary of D01.2: a caller-scoped idempotency key maps to exactly one Job and one
// domain operation key, and the Job is dispatchable only once that mapping is
// durable. It also keeps the cancel intent and terminal receipt of each Job, the
// eligibility/block history of resources, and the PreparedPublication and
// PhysicalBinding records of D01.1/D01.3. Every mutation runs in one BEGIN IMMEDIATE
// transaction committed with synchronous=FULL, and appends one record to a
// hash-chained ledger in the same transaction.
//
// Protected mutations are fenced by a Guard: the installation, activation epoch and
// capture-cut counter read from the admin-control area, which is outside this
// database and outside any backup of it (D01.3). The store keeps its own
// restore_state, store_active_epoch and store_cut_seq; a mutation is refused unless
// they match the Guard. Each Job, policy and binding record also carries a record
// version for compare-and-set updates.
//
// The ledger is metadata only. Transfer chunks, packages and registry effects live
// elsewhere and are reconciled by digest; an external OCI effect is never claimed to
// be atomic with a ledger row.
//
// Out of scope here: the admin-control area itself, namespace token MAC
// verification, the gRPC surface, key tombstones and namespace retirement (W02.A,
// W09).
package jobledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" database/sql driver
	sqlite3 "modernc.org/sqlite/lib"
)

// SchemaVersion is the on-disk schema this package reads and writes.
const SchemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS ledger_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS ledger (
	seq     INTEGER PRIMARY KEY,
	kind    TEXT NOT NULL,
	subject TEXT NOT NULL,
	record  TEXT NOT NULL,
	digest  TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS namespaces (
	namespace_id TEXT PRIMARY KEY,
	record       TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS idempotency (
	namespace_id   TEXT NOT NULL REFERENCES namespaces (namespace_id),
	client_key     TEXT NOT NULL,
	meaning_digest TEXT NOT NULL,
	job_id         TEXT NOT NULL UNIQUE,
	domain_op_key  TEXT NOT NULL UNIQUE,
	record         TEXT NOT NULL,
	PRIMARY KEY (namespace_id, client_key)
) STRICT;
CREATE TABLE IF NOT EXISTS jobs (
	job_id         TEXT PRIMARY KEY REFERENCES idempotency (job_id),
	record_version INTEGER NOT NULL,
	record         TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS policy (
	resource TEXT PRIMARY KEY,
	revision INTEGER NOT NULL,
	record   TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS policy_events (
	seq      INTEGER PRIMARY KEY,
	resource TEXT NOT NULL,
	record   TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS policy_events_resource ON policy_events (resource, seq);
CREATE TABLE IF NOT EXISTS prepared (
	prepared_id   TEXT PRIMARY KEY,
	operation_key TEXT NOT NULL UNIQUE,
	record        TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS bindings (
	binding_id     TEXT PRIMARY KEY,
	prepared_id    TEXT NOT NULL UNIQUE REFERENCES prepared (prepared_id),
	revision_id    TEXT NOT NULL,
	record_version INTEGER NOT NULL,
	record         TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS bindings_revision ON bindings (revision_id);
`

// Store markers kept in ledger_meta. They are store content only in the sense that
// they travel with a backup; the admin-control Guard is the authority they are
// compared with.
const (
	metaSchemaVersion  = "schema_version"
	metaInstallationID = "installation_id"
	metaRestoreState   = "restore_state"
	metaActiveEpoch    = "store_active_epoch"
	metaCutSeq         = "store_cut_seq"
	metaRestoreNonce   = "restore_nonce"
)

// Restore states of the store marker (D01.3).
const (
	RestoreLive             = "LIVE"
	RestoreRestoredInactive = "RESTORED_INACTIVE"
)

// Store is a durable Job ledger backed by one SQLite database file.
type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time

	// beforeCommit, when non-nil, runs after a mutation's writes and just before its
	// COMMIT; a non-nil error rolls the whole transaction back. afterCommit runs just
	// after a successful COMMIT, before the result is returned. Test-only fault hooks.
	beforeCommit func() error
	afterCommit  func()
}

// busyTimeout is how long a store connection waits for another process's lock.
const busyTimeout = 10 * time.Second

// openBusyBudget bounds how long Open spends on SQLITE_BUSY while opening and
// initializing the database.
var openBusyBudget = busyTimeout

var errOpenBudgetSpent = errors.New("jobledger: open busy budget spent")

// Open opens (creating if needed) the ledger at path and checks its schema version.
// A new database has no installation marker until Init. Like the W01.1/W01.2
// adapters, all open attempts share one deadline and the busy timeout is recomputed
// from it before each blocking step.
func Open(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("%w: %q", ErrTransientPath, path)
	}
	deadline := time.Now().Add(openBusyBudget)
	var err error
	for time.Until(deadline) > 0 {
		var s *Store
		if s, err = open(path, deadline); err == nil || !isBusy(err) {
			return s, err
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
	return nil, err
}

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

func open(path string, deadline time.Time) (*Store, error) {
	left := time.Until(deadline)
	if left <= 0 {
		return nil, errOpenBudgetSpent
	}
	q := url.Values{}
	q.Set("_busy_timeout", strconv.FormatInt(max(left.Milliseconds(), 1), 10))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", fileURI(path)+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("jobledger: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("jobledger: open %s: %w", path, err), db.Close())
	}
	if err := setBusyTimeout(ctx, db, time.Until(deadline)); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	s := &Store{db: db, path: path, now: time.Now}
	if err := s.initSchema(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := setBusyTimeout(context.Background(), db, busyTimeout); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

func setBusyTimeout(ctx context.Context, db *sql.DB, d time.Duration) error {
	if d <= 0 {
		return errOpenBudgetSpent
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", max(d.Milliseconds(), 1))); err != nil {
		return fmt.Errorf("jobledger: set busy timeout: %w", err)
	}
	return nil
}

// fileURI returns the SQLite "file:" URI naming exactly path.
func fileURI(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath()
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database. A later Open of the same path sees every committed write.
func (s *Store) Close() error { return s.db.Close() }

// initSchema creates this version's schema in a genuinely empty database and
// otherwise checks the stored schema version without changing anything.
func (s *Store) initSchema(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var objects, hasMeta int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(type = 'table' AND name = 'ledger_meta'), 0)
			FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`).Scan(&objects, &hasMeta)
		if err != nil {
			return fmt.Errorf("jobledger: probe schema: %w", err)
		}
		if objects == 0 {
			if _, err := tx.ExecContext(ctx, schema); err != nil {
				return fmt.Errorf("jobledger: create schema: %w", err)
			}
			return setMeta(ctx, tx, metaSchemaVersion, strconv.Itoa(SchemaVersion))
		}
		if hasMeta == 0 {
			return fmt.Errorf("%w: database has no ledger schema version", ErrSchemaVersion)
		}
		v, ok, err := getMeta(ctx, tx, metaSchemaVersion)
		switch {
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("%w: database has no ledger schema version", ErrSchemaVersion)
		case v != strconv.Itoa(SchemaVersion):
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
		return fmt.Errorf("jobledger: begin: %w", err)
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
		return fmt.Errorf("jobledger: commit: %w", err)
	}
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}

// queryRower is satisfied by *sql.DB and *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func getMeta(ctx context.Context, q queryRower, key string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM ledger_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("jobledger: read %s: %w", key, err)
	}
	return v, true, nil
}

func setMeta(ctx context.Context, e execer, key, value string) error {
	_, err := e.ExecContext(ctx, `INSERT INTO ledger_meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("jobledger: write %s: %w", key, err)
	}
	return nil
}

// appendLedger appends one record to the hash-chained ledger inside tx. Each digest
// is SHA-256 over the previous digest, kind, subject and record, so a later reader
// can detect a lost, reordered or edited row (VerifyLedger).
func appendLedger(ctx context.Context, tx *sql.Tx, kind, subject string, record any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("jobledger: encode ledger record: %w", err)
	}
	head, prev, err := ledgerHead(ctx, tx)
	if err != nil {
		return err
	}
	seq := head + 1
	digest := chainDigest(seq, prev, kind, subject, string(raw))
	_, err = tx.ExecContext(ctx, `INSERT INTO ledger (seq, kind, subject, record, digest) VALUES (?, ?, ?, ?, ?)`,
		seq, kind, subject, string(raw), digest)
	if err != nil {
		return fmt.Errorf("jobledger: append ledger: %w", err)
	}
	return nil
}

// chainDigest covers the row's own sequence number, so renumbering a row breaks
// the chain even when the relative order is kept.
func chainDigest(seq int64, prev, kind, subject, record string) string {
	h := sha256.New()
	for _, part := range []string{strconv.FormatInt(seq, 10), prev, kind, subject, record} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{':'})
		h.Write([]byte(part))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func ledgerHead(ctx context.Context, q queryRower) (int64, string, error) {
	var seq int64
	var digest string
	err := q.QueryRowContext(ctx, `SELECT seq, digest FROM ledger ORDER BY seq DESC LIMIT 1`).Scan(&seq, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("jobledger: read ledger head: %w", err)
	}
	return seq, digest, nil
}

// LedgerHead returns the last ledger sequence number and its chained digest; an
// empty ledger is (0, "").
func (s *Store) LedgerHead(ctx context.Context) (seq int64, digest string, err error) {
	return ledgerHead(ctx, s.db)
}

// VerifyLedger recomputes the whole chain and returns ErrLedgerIntegrity at the
// first row that is not the next sequence number or whose digest does not follow
// from its predecessor.
func (s *Store) VerifyLedger(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, kind, subject, record, digest FROM ledger ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("jobledger: read ledger: %w", err)
	}
	defer rows.Close()
	prev, want := "", int64(1)
	for rows.Next() {
		var seq int64
		var kind, subject, record, digest string
		if err := rows.Scan(&seq, &kind, &subject, &record, &digest); err != nil {
			return fmt.Errorf("jobledger: read ledger: %w", err)
		}
		if seq != want {
			return fmt.Errorf("%w: ledger seq %d, want %d", ErrLedgerIntegrity, seq, want)
		}
		if chainDigest(seq, prev, kind, subject, record) != digest {
			return fmt.Errorf("%w: ledger seq %d", ErrLedgerIntegrity, seq)
		}
		prev, want = digest, want+1
	}
	return rows.Err()
}
