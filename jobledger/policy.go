package jobledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PolicyChange is one block or unblock of a resource's eligibility.
type PolicyChange struct {
	Actor     string
	Operation string
	Resource  string
	Blocked   bool
	Reason    string
}

// PolicyState is a resource's current eligibility. Revision starts at 1 and grows by
// one per change.
type PolicyState struct {
	Resource  string
	Blocked   bool
	Revision  uint64
	Actor     string
	Operation string
	Reason    string
	UpdatedAt time.Time
}

// PolicyEvent is one audit entry of the append-only eligibility history.
type PolicyEvent struct {
	Seq      int64
	Revision uint64
	Change   PolicyChange
	At       time.Time
}

// SetPolicy records a block or unblock of change.Resource and its audit entry in one
// transaction. expectedRevision is the resource's current revision (0 for a resource
// with no history); a different stored revision is ErrVersionConflict.
//
// This is the data-side eligibility/block history that D01.3 asks current-fact
// reconcile to compare. The current-policy authority for authorization stays in the
// admin-control area (W09); a restored copy of this history is evidence only.
// Changing eligibility never touches an accepted Revision or its manifest.
func (s *Store) SetPolicy(ctx context.Context, g Guard, change PolicyChange, expectedRevision uint64) (PolicyState, error) {
	if change.Resource == "" || change.Actor == "" || change.Operation == "" {
		return PolicyState{}, fmt.Errorf("jobledger: policy change needs actor, operation and resource")
	}
	var out PolicyState
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := fence(ctx, tx, g); err != nil {
			return err
		}
		cur, err := policyState(ctx, tx, change.Resource)
		if err != nil {
			return err
		}
		if cur.Revision != expectedRevision {
			return fmt.Errorf("%w: policy %q at revision %d, update from %d", ErrVersionConflict, change.Resource, cur.Revision, expectedRevision)
		}
		out = PolicyState{Resource: change.Resource, Blocked: change.Blocked, Revision: cur.Revision + 1,
			Actor: change.Actor, Operation: change.Operation, Reason: change.Reason, UpdatedAt: s.now().UTC()}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO policy (resource, revision, record) VALUES (?, ?, ?)
			ON CONFLICT (resource) DO UPDATE SET revision = excluded.revision, record = excluded.record`,
			change.Resource, out.Revision, string(raw)); err != nil {
			return fmt.Errorf("jobledger: write policy: %w", err)
		}
		ev := PolicyEvent{Revision: out.Revision, Change: change, At: out.UpdatedAt}
		evRaw, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO policy_events (resource, record) VALUES (?, ?)`, change.Resource, string(evRaw)); err != nil {
			return fmt.Errorf("jobledger: write policy event: %w", err)
		}
		return appendLedger(ctx, tx, "policy", change.Resource, ev)
	})
	if err != nil {
		return PolicyState{}, err
	}
	return out, nil
}

// policyState returns a resource's current eligibility; a resource with no history
// is not blocked, at revision 0.
func policyState(ctx context.Context, q queryRower, resource string) (PolicyState, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM policy WHERE resource = ?`, resource).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return PolicyState{Resource: resource}, nil
	}
	if err != nil {
		return PolicyState{}, fmt.Errorf("jobledger: read policy: %w", err)
	}
	var st PolicyState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return PolicyState{}, fmt.Errorf("jobledger: decode policy: %w", err)
	}
	return st, nil
}

// Policy returns a resource's current eligibility; a resource with no history is
// not blocked, at revision 0.
func (s *Store) Policy(ctx context.Context, resource string) (PolicyState, error) {
	return policyState(ctx, s.db, resource)
}

// PolicyHistory returns a resource's audit entries, oldest first.
func (s *Store) PolicyHistory(ctx context.Context, resource string) ([]PolicyEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, record FROM policy_events WHERE resource = ? ORDER BY seq`, resource)
	if err != nil {
		return nil, fmt.Errorf("jobledger: read policy history: %w", err)
	}
	defer rows.Close()
	var out []PolicyEvent
	for rows.Next() {
		var seq int64
		var raw string
		if err := rows.Scan(&seq, &raw); err != nil {
			return nil, fmt.Errorf("jobledger: read policy history: %w", err)
		}
		var ev PolicyEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return nil, fmt.Errorf("jobledger: decode policy event: %w", err)
		}
		ev.Seq = seq
		out = append(out, ev)
	}
	return out, rows.Err()
}
