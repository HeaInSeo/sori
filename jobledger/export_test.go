package jobledger

// SetCommitHooksForTest installs the fault hooks around every COMMIT. Test-only.
func (s *Store) SetCommitHooksForTest(before func() error, after func()) {
	s.beforeCommit, s.afterCommit = before, after
}

// ExecForTest runs a raw statement against the store. Test-only.
func (s *Store) ExecForTest(query string, args ...any) error {
	_, err := s.db.Exec(query, args...)
	return err
}
