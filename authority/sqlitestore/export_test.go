package sqlitestore

import "database/sql"

// SetCommitHooksForTest installs the fault hooks around every COMMIT. Test-only.
func (s *Store) SetCommitHooksForTest(before func() error, after func()) {
	s.beforeCommit, s.afterCommit = before, after
}

// SetSchemaVersionForTest overwrites the stored schema version. Test-only.
func SetSchemaVersionForTest(path, version string) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, version)
	return err
}
