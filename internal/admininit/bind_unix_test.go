//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package admininit_test

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/internal/admininit"
)

// A FIFO put over the empty job ledger the preflight inspected is refused at
// once: bind does not block waiting for a writer to open it.
func TestInitRefusesFIFOWithoutBlocking(t *testing.T) {
	adminDir, root, p := layout(t)
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
		_, err := admininit.Init(context.Background(), adminDir, root, p, func() (string, error) { return admininit.NewInstallationID() })
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
