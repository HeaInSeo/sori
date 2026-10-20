package admininit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/internal/adminlock/locktest"
)

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
	adminDir, root, p := layout(t)
	outside := t.TempDir()
	h := locktest.Hold(t, locktest.Server, t.TempDir(), outside)
	defer h.Release(t)
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		if err := os.Remove(p.JobLedger); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		symlink(t, filepath.Join(outside, "jobs.db"), p.JobLedger)
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
	outside := t.TempDir()
	defer swapAt(t, admininit.StepAuthoritySchema, func() {
		symlink(t, filepath.Join(outside, "authority.db"), p.Authority)
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
	defer swapAt(t, admininit.StepMarkers, func() {
		tmp := p.JobLedger + ".new"
		writeFile(t, tmp, nil)
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
	})()

	var calls int
	_, err := admininit.Init(context.Background(), adminDir, root, p, countingID(&calls))
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
	adminDir, root, p := layout(t)
	outside := t.TempDir()
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		symlink(t, filepath.Join(outside, "jobs.db-wal"), p.JobLedger+"-wal")
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		link(t, op.JobLedger, p.JobLedger)
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
	defer swapAt(t, admininit.StepAuthoritySchema, func() {
		link(t, outside, p.Authority)
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
	writeFile(t, p.JobLedger, nil)
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		tmp := p.JobLedger + ".new"
		writeFile(t, tmp, nil)
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
	tmp := filepath.Join(root, "staged.db")
	foreign(t, tmp)
	before := readFile(t, tmp)
	defer swapAt(t, admininit.StepAuthoritySchema, func() {
		if err := os.Rename(tmp, p.Authority); err != nil {
			t.Fatal(err)
		}
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
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

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
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
	adminDir, root, p := layout(t)
	defer admininit.SetAfterSee(func() {
		if err := os.Rename(op.Authority, p.Authority); err != nil {
			t.Fatal(err)
		}
	})()

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if _, err := os.Stat(p.JobLedger); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job ledger: %v, want none created", err)
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
