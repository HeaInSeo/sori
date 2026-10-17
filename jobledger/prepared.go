package jobledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Prepared publication states (D01.3 ledger phases this unit records).
const (
	PreparedFrozen   = "FROZEN"
	PreparedPackaged = "PACKAGED"
)

// Binding states. An INTENT binding is never usable; only ATTACHED_READY is.
const (
	BindingIntent        = "INTENT"
	BindingAttachedReady = "ATTACHED_READY"
)

// MemberPath maps one semantic member to its path in the package (PhysicalBinding v1).
type MemberPath struct {
	SemanticKey           string
	CanonicalRelativePath string
	WholeFileSHA256       string
}

// PreparedPublication is the immutable prepared package record (D01.1 A.6). Its
// basis is fixed when it is created; its descriptor and member mapping are fixed
// once, when it is packaged.
type PreparedPublication struct {
	PreparedID       string
	OperationKey     string
	AssetID          string
	PackageBasis     string
	State            string
	DescriptorDigest string
	MemberPaths      []MemberPath
	CreatedAt        time.Time
}

// Binding is a PhysicalBinding of a packaged publication to an accepted Revision.
type Binding struct {
	BindingID        string
	PreparedID       string
	RevisionID       string
	DescriptorDigest string
	MemberPaths      []MemberPath
	State            string
	RecordVersion    uint64
}

// Prepare creates the PreparedPublication for operationKey, or returns the existing
// one. The same key with a different asset or basis is ErrDescriptorConflict; a new
// prepared_id is never minted for a key that already has one.
func (s *Store) Prepare(ctx context.Context, g Guard, operationKey, assetID, basis string) (PreparedPublication, bool, error) {
	var out PreparedPublication
	var created bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := fence(ctx, tx, g); err != nil {
			return err
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT record FROM prepared WHERE operation_key = ?`, operationKey).Scan(&raw)
		if err == nil {
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return fmt.Errorf("jobledger: decode prepared: %w", err)
			}
			if out.AssetID != assetID || out.PackageBasis != basis {
				return fmt.Errorf("%w: operation %q already prepared with another basis", ErrDescriptorConflict, operationKey)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("jobledger: read prepared: %w", err)
		}
		id, err := newID("prep")
		if err != nil {
			return err
		}
		out = PreparedPublication{PreparedID: id, OperationKey: operationKey, AssetID: assetID, PackageBasis: basis,
			State: PreparedFrozen, CreatedAt: s.now().UTC()}
		created = true
		return writePrepared(ctx, tx, out, true)
	})
	if err != nil {
		return PreparedPublication{}, false, err
	}
	return out, created, nil
}

func writePrepared(ctx context.Context, tx *sql.Tx, p PreparedPublication, insert bool) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if insert {
		_, err = tx.ExecContext(ctx, `INSERT INTO prepared (prepared_id, operation_key, record) VALUES (?, ?, ?)`, p.PreparedID, p.OperationKey, string(raw))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE prepared SET record = ? WHERE prepared_id = ?`, string(raw), p.PreparedID)
	}
	if err != nil {
		return fmt.Errorf("jobledger: write prepared: %w", err)
	}
	return appendLedger(ctx, tx, "prepared", p.PreparedID, p)
}

func preparedByID(ctx context.Context, q queryRower, id string) (PreparedPublication, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM prepared WHERE prepared_id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return PreparedPublication{}, fmt.Errorf("%w: prepared %s", ErrNotFound, id)
	}
	if err != nil {
		return PreparedPublication{}, fmt.Errorf("jobledger: read prepared: %w", err)
	}
	var p PreparedPublication
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return PreparedPublication{}, fmt.Errorf("jobledger: decode prepared: %w", err)
	}
	return p, nil
}

// GetPrepared returns a PreparedPublication by id.
func (s *Store) GetPrepared(ctx context.Context, id string) (PreparedPublication, error) {
	return preparedByID(ctx, s.db, id)
}

// RecordPackage fixes the completed package descriptor and member mapping of a
// prepared publication (at most one PACKAGED descriptor per prepared_id). Recording
// the same descriptor and mapping again is a no-op; a different one is
// ErrDescriptorConflict and the recorded one is kept.
func (s *Store) RecordPackage(ctx context.Context, g Guard, preparedID, descriptorDigest string, paths []MemberPath) (PreparedPublication, error) {
	var out PreparedPublication
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := fence(ctx, tx, g); err != nil {
			return err
		}
		p, err := preparedByID(ctx, tx, preparedID)
		if err != nil {
			return err
		}
		if p.State == PreparedPackaged {
			if p.DescriptorDigest != descriptorDigest || !slices.Equal(p.MemberPaths, paths) {
				return fmt.Errorf("%w: prepared %s is packaged as %s", ErrDescriptorConflict, preparedID, p.DescriptorDigest)
			}
			out = p
			return nil
		}
		p.State, p.DescriptorDigest, p.MemberPaths = PreparedPackaged, descriptorDigest, slices.Clone(paths)
		out = p
		return writePrepared(ctx, tx, p, false)
	})
	if err != nil {
		return PreparedPublication{}, err
	}
	return out, nil
}

// BeginBinding records the binding intent of a packaged publication to revisionID.
// The binding copies the prepared descriptor and mapping and starts as INTENT, which
// UsableBindings excludes. One prepared publication has at most one binding; a
// repeat for the same revision returns it, another revision is ErrDescriptorConflict.
func (s *Store) BeginBinding(ctx context.Context, g Guard, preparedID, revisionID string) (Binding, error) {
	var out Binding
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := fence(ctx, tx, g); err != nil {
			return err
		}
		p, err := preparedByID(ctx, tx, preparedID)
		if err != nil {
			return err
		}
		if p.State != PreparedPackaged {
			return fmt.Errorf("%w: prepared %s is %s, not packaged", ErrInvalidTransition, preparedID, p.State)
		}
		existing, ok, err := bindingWhere(ctx, tx, "prepared_id", preparedID)
		if err != nil {
			return err
		}
		if ok {
			if existing.RevisionID != revisionID {
				return fmt.Errorf("%w: prepared %s is bound to %s", ErrDescriptorConflict, preparedID, existing.RevisionID)
			}
			out = existing
			return nil
		}
		id, err := newID("bind")
		if err != nil {
			return err
		}
		out = Binding{BindingID: id, PreparedID: preparedID, RevisionID: revisionID, DescriptorDigest: p.DescriptorDigest,
			MemberPaths: slices.Clone(p.MemberPaths), State: BindingIntent, RecordVersion: 1}
		return writeBinding(ctx, tx, out, 0)
	})
	if err != nil {
		return Binding{}, err
	}
	return out, nil
}

// MarkBindingReady moves an INTENT binding to ATTACHED_READY if its record version is
// expectedVersion. A binding already ready is returned unchanged.
func (s *Store) MarkBindingReady(ctx context.Context, g Guard, bindingID string, expectedVersion uint64) (Binding, error) {
	var out Binding
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := fence(ctx, tx, g); err != nil {
			return err
		}
		b, ok, err := bindingWhere(ctx, tx, "binding_id", bindingID)
		switch {
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("%w: binding %s", ErrNotFound, bindingID)
		case b.State == BindingAttachedReady:
			out = b
			return nil
		case b.RecordVersion != expectedVersion:
			return fmt.Errorf("%w: binding %s at version %d, update from %d", ErrVersionConflict, bindingID, b.RecordVersion, expectedVersion)
		}
		b.State, b.RecordVersion = BindingAttachedReady, b.RecordVersion+1
		out = b
		return writeBinding(ctx, tx, b, expectedVersion)
	})
	if err != nil {
		return Binding{}, err
	}
	return out, nil
}

func writeBinding(ctx context.Context, tx *sql.Tx, b Binding, fromVersion uint64) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if fromVersion == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO bindings (binding_id, prepared_id, revision_id, record_version, record) VALUES (?, ?, ?, ?, ?)`,
			b.BindingID, b.PreparedID, b.RevisionID, b.RecordVersion, string(raw))
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE bindings SET record_version = ?, record = ? WHERE binding_id = ? AND record_version = ?`,
			b.RecordVersion, string(raw), b.BindingID, fromVersion)
		if err == nil {
			if n, rerr := res.RowsAffected(); rerr != nil || n != 1 {
				err = errors.Join(fmt.Errorf("%w: binding %s changed during update", ErrVersionConflict, b.BindingID), rerr)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("jobledger: write binding: %w", err)
	}
	return appendLedger(ctx, tx, "binding", b.BindingID, b)
}

func bindingWhere(ctx context.Context, q queryRower, column, value string) (Binding, bool, error) {
	query := `SELECT record FROM bindings WHERE binding_id = ?`
	if column == "prepared_id" {
		query = `SELECT record FROM bindings WHERE prepared_id = ?`
	}
	var raw string
	err := q.QueryRowContext(ctx, query, value).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, false, nil
	}
	if err != nil {
		return Binding{}, false, fmt.Errorf("jobledger: read binding: %w", err)
	}
	var b Binding
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return Binding{}, false, fmt.Errorf("jobledger: decode binding: %w", err)
	}
	return b, true, nil
}

// GetBinding returns a binding by id.
func (s *Store) GetBinding(ctx context.Context, id string) (Binding, bool, error) {
	return bindingWhere(ctx, s.db, "binding_id", id)
}

// UsableBindings returns the ATTACHED_READY bindings of a Revision. Pending (INTENT)
// bindings are never selected.
func (s *Store) UsableBindings(ctx context.Context, revisionID string) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM bindings WHERE revision_id = ? ORDER BY rowid`, revisionID)
	if err != nil {
		return nil, fmt.Errorf("jobledger: list bindings: %w", err)
	}
	defer rows.Close()
	var out []Binding
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("jobledger: list bindings: %w", err)
		}
		var b Binding
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, fmt.Errorf("jobledger: decode binding: %w", err)
		}
		if b.State == BindingAttachedReady {
			out = append(out, b)
		}
	}
	return out, rows.Err()
}
