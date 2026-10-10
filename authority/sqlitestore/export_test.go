package sqlitestore

import "database/sql"

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
