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
