package sqlitestore_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/HeaInSeo/sori/authority"
	"github.com/HeaInSeo/sori/authority/authoritytest"
	"github.com/HeaInSeo/sori/authority/sqlitestore"
)

func open(t *testing.T, path string) *sqlitestore.Store {
	t.Helper()
	s, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The SQLite adapter must pass the full backend-neutral contract, including the
// Reopen tier: Reopen closes the database and opens the same file again.
func TestSQLiteStoreConformance(t *testing.T) {
	authoritytest.Run(t, authoritytest.Harness{
		New: func(t *testing.T) authority.Store {
			return open(t, filepath.Join(t.TempDir(), "authority.db"))
		},
		Reopen: func(t *testing.T, s authority.Store) authority.Store {
			old := s.(*sqlitestore.Store)
			if err := old.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			return open(t, old.Path())
		},
	})
}

// A database written by another schema version is refused, not silently migrated.
func TestOpenRefusesOtherSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.db")
	s := open(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sqlitestore.SetSchemaVersionForTest(path, "999"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitestore.Open(path); !errors.Is(err, sqlitestore.ErrSchemaVersion) {
		t.Fatalf("Open: err = %v, want ErrSchemaVersion", err)
	}
}
