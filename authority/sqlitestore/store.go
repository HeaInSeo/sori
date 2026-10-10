// Package sqlitestore is the durable SQLite/WAL adapter for authority.Store (W01.1).
//
// It is the J1 durable single-writer local profile (CAD-r1 D01.1-5): the database
// and its WAL live on host-local disk, never on NFS. It reuses the MemoryStore
// semantics through the backend-neutral authoritytest suite; only the commit
// boundary changes. Every mutation runs in one BEGIN IMMEDIATE transaction, and a
// successful result is returned only after that transaction has committed with
// synchronous=FULL.
package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/HeaInSeo/sori/authority"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// SchemaVersion is the on-disk schema this adapter reads and writes.
const SchemaVersion = 1

// ErrSchemaVersion is returned by Open when the database was written by a different
// schema version. The adapter never migrates implicitly.
var ErrSchemaVersion = errors.New("sqlitestore: unsupported schema version")

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS revisions (
	seq         INTEGER PRIMARY KEY,
	revision_id TEXT NOT NULL UNIQUE,
	request_id  TEXT NOT NULL UNIQUE,
	asset_id    TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	record      TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS bind_events (
	seq             INTEGER PRIMARY KEY,
	bind_request_id TEXT NOT NULL UNIQUE,
	alias           TEXT NOT NULL,
	record          TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS bind_events_alias ON bind_events (alias, seq);
CREATE TABLE IF NOT EXISTS representations (
	seq               INTEGER PRIMARY KEY,
	representation_id TEXT NOT NULL UNIQUE,
	attach_op_id      TEXT NOT NULL UNIQUE,
	revision_id       TEXT NOT NULL REFERENCES revisions (revision_id),
	fingerprint       TEXT NOT NULL,
	record            TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS representations_revision ON representations (revision_id, seq);
`

// Store is a durable authority.Store backed by one SQLite database file.
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

var _ authority.Store = (*Store)(nil)

// Open opens (creating if needed) the store at path and checks its schema version.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open %s: %w", path, err)
	}
	// One connection per process: writes in this process are serialized here, and
	// BEGIN IMMEDIATE plus busy_timeout serializes them against other processes.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, now: func() time.Time { return time.Now().UTC() }}
	if err := s.init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database. A later Open of the same path sees every committed write.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("sqlitestore: create schema: %w", err)
		}
		var v string
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, fmt.Sprint(SchemaVersion))
			return err
		case err != nil:
			return err
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
		return fmt.Errorf("sqlitestore: begin: %w", err)
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
		return fmt.Errorf("sqlitestore: commit: %w", err)
	}
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}

func nextSeq(ctx context.Context, tx *sql.Tx, table string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM `+table).Scan(&n)
	return n, err
}

func decode[T any](raw string) (T, error) {
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return v, fmt.Errorf("sqlitestore: decode record: %w", err)
	}
	return v, nil
}

func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlitestore: encode record: %w", err)
	}
	return string(b), nil
}

// AcceptRevision implements authority.Store.
func (s *Store) AcceptRevision(ctx context.Context, req authority.AcceptRequest, fingerprint string) (authority.Revision, error) {
	var out authority.Revision
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT record FROM revisions WHERE request_id = ?`, string(req.RequestID)).Scan(&raw)
		if err == nil {
			existing, err := decode[authority.Revision](raw)
			if err != nil {
				return err
			}
			if existing.AssetID == req.AssetID && existing.Fingerprint == fingerprint {
				out = existing // idempotent reconcile: same request, same content
				return nil
			}
			return fmt.Errorf("%w: request %q", authority.ErrRequestConflict, req.RequestID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		seq, err := nextSeq(ctx, tx, "revisions")
		if err != nil {
			return err
		}
		rev := authority.Revision{
			RevisionID:  authority.RevisionID(fmt.Sprintf("sori-rev-%d", seq)),
			AssetID:     req.AssetID,
			RequestID:   req.RequestID,
			Fingerprint: fingerprint,
			Manifest:    req.Manifest,
			AcceptedAt:  s.now(),
		}
		// Encoding the record copies the manifest, so the caller's request and the
		// returned value share no storage with committed truth.
		rec, err := encode(rev)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO revisions (seq, revision_id, request_id, asset_id, fingerprint, record) VALUES (?, ?, ?, ?, ?, ?)`,
			seq, string(rev.RevisionID), string(req.RequestID), string(req.AssetID), fingerprint, rec); err != nil {
			return err
		}
		out, err = decode[authority.Revision](rec)
		return err
	})
	if err != nil {
		return authority.Revision{}, err
	}
	return out, nil
}

// BindAlias implements authority.Store.
func (s *Store) BindAlias(ctx context.Context, req authority.BindRequest) (authority.BindEvent, error) {
	var out authority.BindEvent
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT record FROM bind_events WHERE bind_request_id = ?`, string(req.BindRequestID)).Scan(&raw)
		if err == nil {
			prior, err := decode[authority.BindEvent](raw)
			if err != nil {
				return err
			}
			if prior.Alias == req.Alias && prior.AssetID == req.AssetID && prior.RevisionID == req.RevisionID {
				out = prior // idempotent: same operation id, same binding, no dup
				return nil
			}
			return fmt.Errorf("%w: bind request %q", authority.ErrAliasBindingConflict, req.BindRequestID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var assetID string
		err = tx.QueryRowContext(ctx, `SELECT asset_id FROM revisions WHERE revision_id = ?`, string(req.RevisionID)).Scan(&assetID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %q", authority.ErrRevisionNotFound, req.RevisionID)
		}
		if err != nil {
			return err
		}
		if authority.AssetID(assetID) != req.AssetID {
			return fmt.Errorf("%w: revision %q belongs to a different asset", authority.ErrAliasBindingConflict, req.RevisionID)
		}
		seq, err := nextSeq(ctx, tx, "bind_events")
		if err != nil {
			return err
		}
		ev := authority.BindEvent{
			BindRequestID: req.BindRequestID,
			Alias:         req.Alias,
			AssetID:       req.AssetID,
			RevisionID:    req.RevisionID,
			Sequence:      seq,
			BoundAt:       s.now(),
		}
		rec, err := encode(ev)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO bind_events (seq, bind_request_id, alias, record) VALUES (?, ?, ?, ?)`,
			seq, string(req.BindRequestID), req.Alias, rec); err != nil {
			return err
		}
		out, err = decode[authority.BindEvent](rec)
		return err
	})
	if err != nil {
		return authority.BindEvent{}, err
	}
	return out, nil
}

// GetRevision implements authority.Store.
func (s *Store) GetRevision(ctx context.Context, id authority.RevisionID) (authority.Revision, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT record FROM revisions WHERE revision_id = ?`, string(id)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return authority.Revision{}, false, nil
	}
	if err != nil {
		return authority.Revision{}, false, err
	}
	rev, err := decode[authority.Revision](raw)
	if err != nil {
		return authority.Revision{}, false, err
	}
	return rev, true, nil
}

// AliasHistory implements authority.Store.
func (s *Store) AliasHistory(ctx context.Context, alias string) ([]authority.BindEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM bind_events WHERE alias = ? ORDER BY seq`, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []authority.BindEvent{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		ev, err := decode[authority.BindEvent](raw)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// AttachRepresentation implements authority.Store.
func (s *Store) AttachRepresentation(ctx context.Context, req authority.AttachRequest, fingerprint string, revMembers []authority.Member) (authority.Representation, error) {
	var out authority.Representation
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Reconcile the attach operation id FIRST, before member equivalence.
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT record FROM representations WHERE attach_op_id = ?`, string(req.AttachOperationID)).Scan(&raw)
		if err == nil {
			existing, err := decode[authority.Representation](raw)
			if err != nil {
				return err
			}
			if existing.RevisionID == req.RevisionID && existing.Fingerprint == fingerprint {
				out = existing // idempotent: same op, same relation
				return nil
			}
			return fmt.Errorf("%w: attach operation %q", authority.ErrAttachConflict, req.AttachOperationID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !authority.MembersEquivalent(req.MemberProofs, revMembers) {
			return authority.ErrMemberEquivalence
		}
		seq, err := nextSeq(ctx, tx, "representations")
		if err != nil {
			return err
		}
		rep := authority.Representation{
			RepresentationID:  authority.RepresentationID(fmt.Sprintf("sori-rep-%d", seq)),
			RevisionID:        req.RevisionID,
			AssetID:           req.AssetID,
			Format:            req.Format,
			Fingerprint:       fingerprint,
			MemberProofs:      req.MemberProofs,
			Locators:          req.Locators,
			Healthy:           true,
			AttachOperationID: req.AttachOperationID,
			AttachedAt:        s.now(),
		}
		if len(rep.MemberProofs) == 0 {
			rep.MemberProofs = nil // match MemoryStore's cloneMembers
		}
		rec, err := encode(rep)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO representations (seq, representation_id, attach_op_id, revision_id, fingerprint, record) VALUES (?, ?, ?, ?, ?, ?)`,
			seq, string(rep.RepresentationID), string(req.AttachOperationID), string(req.RevisionID), fingerprint, rec); err != nil {
			return err
		}
		out, err = decode[authority.Representation](rec)
		return err
	})
	if err != nil {
		return authority.Representation{}, err
	}
	return out, nil
}

// updateRepresentation rewrites only the mutable availability fields of one
// Representation; its identity, fingerprint and Revision are never changed.
func (s *Store) updateRepresentation(ctx context.Context, id authority.RepresentationID, mutate func(*authority.Representation)) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT record FROM representations WHERE representation_id = ?`, string(id)).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %q", authority.ErrRepresentationNotFound, id)
		}
		if err != nil {
			return err
		}
		rep, err := decode[authority.Representation](raw)
		if err != nil {
			return err
		}
		mutate(&rep)
		rec, err := encode(rep)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE representations SET record = ? WHERE representation_id = ?`, rec, string(id))
		return err
	})
}

// SetRepresentationLocators implements authority.Store.
func (s *Store) SetRepresentationLocators(ctx context.Context, id authority.RepresentationID, locators []authority.Locator) error {
	return s.updateRepresentation(ctx, id, func(r *authority.Representation) { r.Locators = locators })
}

// SetRepresentationHealth implements authority.Store.
func (s *Store) SetRepresentationHealth(ctx context.Context, id authority.RepresentationID, healthy bool) error {
	return s.updateRepresentation(ctx, id, func(r *authority.Representation) { r.Healthy = healthy })
}

// GetRepresentation implements authority.Store.
func (s *Store) GetRepresentation(ctx context.Context, id authority.RepresentationID) (authority.Representation, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT record FROM representations WHERE representation_id = ?`, string(id)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return authority.Representation{}, false, nil
	}
	if err != nil {
		return authority.Representation{}, false, err
	}
	rep, err := decode[authority.Representation](raw)
	if err != nil {
		return authority.Representation{}, false, err
	}
	return rep, true, nil
}

// ListRepresentations implements authority.Store.
func (s *Store) ListRepresentations(ctx context.Context, revID authority.RevisionID) ([]authority.Representation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM representations WHERE revision_id = ? ORDER BY seq`, string(revID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []authority.Representation{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		rep, err := decode[authority.Representation](raw)
		if err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	return out, rows.Err()
}
