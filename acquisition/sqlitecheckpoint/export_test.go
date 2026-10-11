package sqlitecheckpoint

import (
	"database/sql"
	"time"
)

// SetOpenBusyBudgetForTest replaces Open's SQLITE_BUSY budget and returns a func
// that restores it. Test-only; not safe with parallel tests.
func SetOpenBusyBudgetForTest(d time.Duration) (restore func()) {
	old := openBusyBudget
	openBusyBudget = d
	return func() { openBusyBudget = old }
}

// SetOpenAfterConnectForTest installs a hook that runs in each open attempt after
// the connection is made and before init, and returns a func that removes it.
// Test-only; not safe with parallel tests.
func SetOpenAfterConnectForTest(hook func()) (restore func()) {
	old := openAfterConnect
	openAfterConnect = hook
	return func() { openAfterConnect = old }
}

// BusyTimeoutForTest reports the open connection's busy_timeout in milliseconds.
// Test-only.
func (s *Store) BusyTimeoutForTest() (ms int, err error) {
	err = s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&ms)
	return ms, err
}

// SetCommitHooksForTest installs the fault hooks around every COMMIT. Test-only.
func (s *Store) SetCommitHooksForTest(before func() error, after func()) {
	s.beforeCommit, s.afterCommit = before, after
}

// SetSchemaVersionForTest overwrites the stored schema version. Test-only.
func SetSchemaVersionForTest(path, version string) error {
	db, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE checkpoint_meta SET value = ? WHERE key = 'schema_version'`, version)
	return err
}
