package jobledger

import "database/sql"

// SetCommitHooksForTest installs the fault hooks around every COMMIT. Test-only.
func (s *Store) SetCommitHooksForTest(before func() error, after func()) {
	s.beforeCommit, s.afterCommit = before, after
}

// ExecForTest runs a raw statement against the store. Test-only.
func (s *Store) ExecForTest(query string, args ...any) error {
	_, err := s.db.Exec(query, args...)
	return err
}

// QueryRowForTest runs a raw single-row query against the store. Test-only.
func (s *Store) QueryRowForTest(query string, args ...any) *sql.Row {
	return s.db.QueryRow(query, args...)
}
