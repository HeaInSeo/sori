//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package admininit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/internal/adminlock/locktest"
)

// Two processes: the job ledger is already a hard link to another installation's
// provisioned ledger, whose server holds that root's server.lock. The other
// server and init would use one file under two locks, so init refuses before it
// writes anything, and the other ledger stays byte-identical.
func TestInitRefusesExistingHardLinkedLedger(t *testing.T) {
	other := t.TempDir()
	op := admininit.Paths{JobLedger: filepath.Join(other, "jobs.db"), Authority: filepath.Join(other, "authority.db")}
	provision(t, op)
	h := locktest.Hold(t, locktest.Server, t.TempDir(), other)
	defer h.Release(t)
	before := readFile(t, op.JobLedger)
	p := dataRoot(t)
	link(t, op.JobLedger, p.JobLedger)

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if readFile(t, op.JobLedger) != before {
		t.Fatal("other installation's job ledger changed")
	}
	if _, err := os.Stat(p.Authority); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authority: %v, want none created", err)
	}
}

// An authority database that is already a hard link to another installation's
// authority is refused the same way.
func TestInitRefusesExistingHardLinkedAuthority(t *testing.T) {
	other := t.TempDir()
	op := admininit.Paths{JobLedger: filepath.Join(other, "jobs.db"), Authority: filepath.Join(other, "authority.db")}
	provision(t, op)
	before := readFile(t, op.Authority)
	p := dataRoot(t)
	link(t, op.Authority, p.Authority)

	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, noID(t))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if readFile(t, op.Authority) != before {
		t.Fatal("other installation's authority changed")
	}
	if _, err := os.Stat(p.JobLedger); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job ledger: %v, want none created", err)
	}
}

// A hard link made to the bound job ledger from another data root before the
// markers is refused, and neither name gets markers.
func TestInitRefusesLinkAddedToBoundLedger(t *testing.T) {
	p := dataRoot(t)
	alias := filepath.Join(t.TempDir(), "jobs.db")
	defer swapAt(t, admininit.StepMarkers, func() {
		link(t, p.JobLedger, alias)
	})()

	var calls int
	_, err := admininit.ProvisionAndBootstrap(context.Background(), p, countingID(&calls))
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	checkNoMarkers(t, p.JobLedger)
	checkNoMarkers(t, alias)
}

// A FIFO put over the empty job ledger the preflight inspected is refused at
// once: bind does not block waiting for a writer to open it.
func TestInitRefusesFIFOWithoutBlocking(t *testing.T) {
	p := dataRoot(t)
	writeFile(t, p.JobLedger, nil)
	defer swapAt(t, admininit.StepJobLedgerSchema, func() {
		if err := os.Remove(p.JobLedger); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(p.JobLedger, 0o600); err != nil {
			t.Error(err)
		}
	})()

	done := make(chan error, 1)
	go func() {
		_, err := admininit.ProvisionAndBootstrap(context.Background(), p, func() (string, error) { return admininit.NewInstallationID() })
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, admininit.ErrRefused) {
			t.Fatalf("err = %v, want ErrRefused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Init blocked on a FIFO at the job ledger name")
	}
}
