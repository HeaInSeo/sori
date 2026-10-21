package admininit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HeaInSeo/sori/authority/sqlitestore"
	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/internal/adminlock/locktest"
	"github.com/HeaInSeo/sori/jobledger"
)

// The binding tests drive the database phase (ProvisionAndBootstrap) directly.
// Init creates and records both files before the phase (TRUST r4 §3.2 step 7)
// and refuses any file it did not create, so the absent-name and existing-file
// cases of bind are reached only through the phase itself. Init's own identity
// checks are in intent_test.go.

// swapAt runs swap once, just before the hook point at.
func swapAt(t *testing.T, at admininit.Step, swap func()) (restore func()) {
	t.Helper()
	done := false
	return admininit.SetBeforeOpen(func(s admininit.Step) {
		if s == at && !done {
			done = true
			swap()
		}
	})
}

// Two processes: an absent job ledger checked by init is created as a symlink
// into another data root before SQLite opens it, while a server holds that
// root's server.lock. Init must refuse and write nothing there.
func TestInitRefusesFinalNameSymlinkedAfterPreflight(t *testing.T) {
	p := dataRoot(t)
	outside := t.TempDir()
	h := locktest.Hold(t, locktest.Server, t.TempDir(), outside)
	defer h.Release(t)
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		if err := os.Remove(p.JobLedger); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		symlink(t, filepath.Join(outside, "jobs.db"), p.JobLedger)
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if got := names(t, outside); !slices.Equal(got, []string{adminlock.ServerLockName}) {
		t.Fatalf("other data root = %v, want only server.lock", got)
	}
}

// A symlink that appears at an absent final name after the preflight is not
// followed when the file is first opened: nothing is created at its target.
func TestInitRefusesSymlinkBeforeFirstOpen(t *testing.T) {
	p := dataRoot(t)
	outside := t.TempDir()
	defer swapAt(t, admininit.StepAuthoritySchema, func() {
		symlink(t, filepath.Join(outside, "authority.db"), p.Authority)
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if got := names(t, outside); len(got) != 0 {
		t.Fatalf("outside = %v, want empty", got)
	}
}

// A job ledger replaced by another regular file before the markers are written
// is refused, and the replacement gets no markers.
func TestInitRefusesReplacedDatabase(t *testing.T) {
	p := dataRoot(t)
	defer swapAt(t, admininit.StepMarkers, func() {
		tmp := p.JobLedger + ".new"
		writeFile(t, tmp, nil)
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
	})()

	var calls int
	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, countingID(&calls))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if fi, err := os.Stat(p.JobLedger); err != nil || fi.Size() != 0 {
		t.Fatalf("replacement: %v, %v; want an untouched empty file", fi, err)
	}
}

// A WAL sidecar that is a symlink out of the root is refused before SQLite
// opens the database next to it.
func TestInitRefusesSidecarSymlink(t *testing.T) {
	p := dataRoot(t)
	outside := t.TempDir()
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		symlink(t, filepath.Join(outside, "jobs.db-wal"), p.JobLedger+"-wal")
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if got := names(t, outside); len(got) != 0 {
		t.Fatalf("outside = %v, want empty", got)
	}
}

// A hard link to another installation's provisioned job ledger, put at the
// absent ledger name after the preflight, is refused, and that ledger gets no
// markers.
func TestInitRefusesHardLinkAtAbsentLedger(t *testing.T) {
	other := t.TempDir()
	op := admininit.Paths{JobLedger: filepath.Join(other, "jobs.db"), Authority: filepath.Join(other, "authority.db")}
	provision(t, op)
	before := readFile(t, op.JobLedger)
	p := dataRoot(t)
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		link(t, op.JobLedger, p.JobLedger)
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if readFile(t, op.JobLedger) != before {
		t.Fatal("other installation's job ledger changed")
	}
}

// A hard link to a foreign SQLite file, put at the absent authority name after
// the preflight, is refused before SQLite opens it.
func TestInitRefusesHardLinkAtAbsentAuthority(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "other.db")
	foreign(t, outside)
	before := readFile(t, outside)
	p := dataRoot(t)
	defer swapAt(t, admininit.StepAuthoritySchema, func() {
		link(t, outside, p.Authority)
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if readFile(t, outside) != before {
		t.Fatal("foreign file changed")
	}
}

// A file renamed over the empty job ledger the preflight inspected is refused
// before SQLite opens it, and the replacement stays empty.
func TestInitRefusesRenamedInLedger(t *testing.T) {
	p := dataRoot(t)
	writeFile(t, p.JobLedger, nil)
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		tmp := p.JobLedger + ".new"
		writeFile(t, tmp, nil)
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if fi, err := os.Stat(p.JobLedger); err != nil || fi.Size() != 0 {
		t.Fatalf("replacement: %v, %v; want an untouched empty file", fi, err)
	}
}

// A foreign SQLite file renamed in at the absent authority name after the
// preflight is refused, and it is not changed.
func TestInitRefusesRenamedInAuthority(t *testing.T) {
	p := dataRoot(t)
	tmp := filepath.Join(filepath.Dir(p.JobLedger), "staged.db")
	foreign(t, tmp)
	before := readFile(t, tmp)
	defer swapAt(t, admininit.StepAuthoritySchema, func() {
		if err := os.Rename(tmp, p.Authority); err != nil {
			t.Fatal(err)
		}
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if readFile(t, p.Authority) != before {
		t.Fatal("renamed-in authority file changed")
	}
}

// A database the preflight saw as an empty file and that grew before the first
// open is refused: the held file must have the size the preflight saw.
func TestInitRefusesGrownPristineFile(t *testing.T) {
	p := dataRoot(t)
	writeFile(t, p.Authority, nil)
	defer swapAt(t, admininit.StepAuthoritySchema, func() {
		f, err := os.OpenFile(p.Authority, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if got := readFile(t, p.Authority); got != "x" {
		t.Fatalf("authority = %q, want the untouched one byte", got)
	}
}

// An authority database that appears at the absent name while the preflight
// reads it is refused: init never opens a SCHEMA_ONLY authority, so without the
// recheck the markers would be written next to a file nobody provisioned.
func TestInitRefusesDatabaseAppearingDuringPreflight(t *testing.T) {
	other := t.TempDir()
	op := admininit.Paths{JobLedger: filepath.Join(other, "jobs.db"), Authority: filepath.Join(other, "authority.db")}
	provision(t, op)
	p := dataRoot(t)
	defer admininit.SetAfterSee(func() {
		if err := os.Rename(op.Authority, p.Authority); err != nil {
			t.Fatal(err)
		}
	})()

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if _, err := os.Stat(p.JobLedger); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job ledger: %v, want none created", err)
	}
}

// A SCHEMA_ONLY authority that no step opens is still bound through the marker
// commit: a foreign file renamed over it before the markers is refused, and the
// job ledger gets no markers.
func TestInitRefusesSchemaOnlyAuthorityReplacedBeforeMarkers(t *testing.T) {
	p := dataRoot(t)
	provisionAuthority(t, p.Authority)
	tmp := filepath.Join(filepath.Dir(p.JobLedger), "staged.db")
	foreign(t, tmp)
	defer swapAt(t, admininit.StepMarkers, func() {
		if err := os.Rename(tmp, p.Authority); err != nil {
			t.Fatal(err)
		}
	})()

	var calls int
	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, countingID(&calls))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	checkNoMarkers(t, p.JobLedger)
}

// A SCHEMA_ONLY authority removed before the markers is refused the same way.
func TestInitRefusesSchemaOnlyAuthorityRemovedBeforeMarkers(t *testing.T) {
	p := dataRoot(t)
	provisionAuthority(t, p.Authority)
	defer swapAt(t, admininit.StepMarkers, func() {
		if err := os.Remove(p.Authority); err != nil {
			t.Fatal(err)
		}
	})()

	var calls int
	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, countingID(&calls))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	checkNoMarkers(t, p.JobLedger)
}

// Two processes: the job ledger is replaced by a file that is not a database
// after the step's first check, so the step itself fails. The check after the
// step still runs and reports the replacement.
func TestInitChecksAfterFailedStep(t *testing.T) {
	p := dataRoot(t)
	defer admininit.SetBeforeStep(func(s admininit.Step) {
		if s != admininit.StepMarkers {
			return
		}
		tmp := p.JobLedger + ".new"
		writeFile(t, tmp, []byte("not a database, but longer than one SQLite header page of one hundred bytes ............"))
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
	})()

	var calls int
	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, countingID(&calls))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused from the check after the failed step", err)
	}
}

// Two processes: a foreign SQLite file renamed over the job ledger after the
// step's first check and before SQLite opens the path is reported by the check
// after the step. SQLite opens by path, so the step may already have written
// to the replacement; init refuses, and the ledger it bound gets no markers.
func TestInitRefusesLedgerReplacedDuringStep(t *testing.T) {
	p := dataRoot(t)
	tmp := filepath.Join(filepath.Dir(p.JobLedger), "staged.db")
	foreign(t, tmp)
	var bound string
	defer admininit.SetBeforeStep(func(s admininit.Step) {
		if s != admininit.StepMarkers {
			return
		}
		bound = p.JobLedger + ".bound"
		if err := os.Rename(p.JobLedger, bound); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
	})()

	var calls int
	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, countingID(&calls))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	checkNoMarkers(t, bound)
}

func provisionAuthority(t *testing.T, path string) {
	t.Helper()
	a, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

// checkNoMarkers asserts that the job ledger at path, if any, has no init
// markers.
func checkNoMarkers(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return
	}
	got, err := jobledger.InspectInitState(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == jobledger.InitMarkerEmpty {
		t.Fatalf("job ledger %s got markers %q", path, got.InstallationID)
	}
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Link(target, name); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
