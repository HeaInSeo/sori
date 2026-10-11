package sqlitecheckpoint_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/HeaInSeo/sori/acquisition"
	"github.com/HeaInSeo/sori/acquisition/checkpointtest"
	"github.com/HeaInSeo/sori/acquisition/sqlitecheckpoint"
	"github.com/HeaInSeo/sori/authority/sqlitestore"
)

func open(t *testing.T, path string) *sqlitecheckpoint.Store {
	t.Helper()
	s, err := sqlitecheckpoint.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The SQLite adapter must pass the full backend-neutral contract, including the
// durability tier: Reopen closes the database and opens the same file again.
func TestSQLiteCheckpointStoreConformance(t *testing.T) {
	checkpointtest.Run(t, checkpointtest.Harness{
		New: func(t *testing.T) acquisition.CheckpointStore {
			return open(t, filepath.Join(t.TempDir(), "checkpoint.db"))
		},
		Reopen: func(t *testing.T, s acquisition.CheckpointStore) acquisition.CheckpointStore {
			old := s.(*sqlitecheckpoint.Store)
			if err := old.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			return open(t, old.Path())
		},
	})
}

// A database written by another schema version is refused, not silently migrated.
func TestOpenRefusesOtherSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := open(t, path).Close(); err != nil {
		t.Fatal(err)
	}
	if err := sqlitecheckpoint.SetSchemaVersionForTest(path, "999"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitecheckpoint.Open(path); !errors.Is(err, sqlitecheckpoint.ErrSchemaVersion) {
		t.Fatalf("Open: err = %v, want ErrSchemaVersion", err)
	}
}

// An empty path or ":memory:" would give an in-memory or private temporary database
// that loses every checkpoint on close, so Open refuses it.
func TestOpenRejectsTransientPaths(t *testing.T) {
	for _, path := range []string{"", ":memory:"} {
		s, err := sqlitecheckpoint.Open(path)
		if !errors.Is(err, sqlitecheckpoint.ErrTransientPath) {
			if s != nil {
				_ = s.Close()
			}
			t.Fatalf("Open(%q): err = %v, want ErrTransientPath", path, err)
		}
	}
}

// An authority database is not a checkpoint database: Open refuses it instead of
// adding checkpoint tables to it.
func TestOpenRefusesAuthorityDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.db")
	a, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitecheckpoint.Open(path); !errors.Is(err, sqlitecheckpoint.ErrSchemaVersion) {
		t.Fatalf("Open(authority db): err = %v, want ErrSchemaVersion", err)
	}
}

// A failure at the commit boundary rolls back Begin and Update; nothing partial is
// visible, and the retry behaves as if the failed call never happened.
func TestCommitBoundaryFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "checkpoint.db"))
	boom := errors.New("injected commit-boundary failure")

	s.SetCommitHooksForTest(func() error { return boom }, nil)
	if _, _, err := s.Begin(ctx, checkpointtest.Operation("op-fault")); !errors.Is(err, boom) {
		t.Fatalf("Begin under failure: err = %v", err)
	}
	s.SetCommitHooksForTest(nil, nil)
	if _, ok, err := s.Get(ctx, "op-fault"); ok || err != nil {
		t.Fatalf("rolled-back Begin is visible: ok=%v err=%v", ok, err)
	}
	op, created, err := s.Begin(ctx, checkpointtest.Operation("op-fault"))
	if err != nil || !created || op.Version != 1 {
		t.Fatalf("Begin retry = %+v created=%v err=%v", op, created, err)
	}

	next := op
	next.Phase = acquisition.PhaseStaged
	next.StagedPath = "/staging/op-fault"
	s.SetCommitHooksForTest(func() error { return boom }, nil)
	if _, err := s.Update(ctx, next); !errors.Is(err, boom) {
		t.Fatalf("Update under failure: err = %v", err)
	}
	s.SetCommitHooksForTest(nil, nil)
	if got, _, _ := s.Get(ctx, "op-fault"); !reflect.DeepEqual(got, op) {
		t.Fatalf("rolled-back Update is visible: %+v", got)
	}
	if got, err := s.Update(ctx, next); err != nil || got.Version != 2 {
		t.Fatalf("Update retry = %+v, %v; want version 2", got, err)
	}
}
