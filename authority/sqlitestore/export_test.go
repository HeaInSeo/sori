package sqlitestore

import "database/sql"

// SetCommitHooksForTest installs the fault hooks around every COMMIT. Test-only.
func (s *Store) SetCommitHooksForTest(before func() error, after func()) {
	s.beforeCommit, s.afterCommit = before, after
}

// JournalModeForTest reports the open connection's journal_mode and synchronous
// pragmas. Test-only.
func (s *Store) JournalModeForTest() (journal string, synchronous int, err error) {
	if err = s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		return "", 0, err
	}
	err = s.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous)
	return journal, synchronous, err
}

// SetSchemaVersionForTest overwrites the stored schema version. Test-only.
func SetSchemaVersionForTest(path, version string) error {
	db, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, version)
	return err
}
