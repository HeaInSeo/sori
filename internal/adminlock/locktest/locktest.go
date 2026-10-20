// Package locktest holds adminlock locks from a separate process for two-process
// tests. A test package calls RunHolderIfRequested first in its TestMain; Hold then
// re-executes the test binary as the holder.
package locktest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/internal/adminlock"
)

// Mode selects which locks the holder process takes.
type Mode string

const (
	// Offline holds both locks as an offline sori-admin command does (L3).
	Offline Mode = "offline"
	// Server holds server.lock as a running server does, after releasing admin.lock (L2).
	Server Mode = "server"
)

const (
	modeEnv  = "SORI_LOCKTEST_MODE"
	adminEnv = "SORI_LOCKTEST_ADMIN"
	dataEnv  = "SORI_LOCKTEST_DATA"
	ready    = "LOCKED"
)

// RunHolderIfRequested runs the holder and exits when this process was started by
// Hold. Otherwise it returns at once.
func RunHolderIfRequested() {
	mode := Mode(os.Getenv(modeEnv))
	if mode == "" {
		return
	}
	os.Exit(hold(mode, os.Getenv(adminEnv), os.Getenv(dataEnv)))
}

func hold(mode Mode, adminDir, dataRoot string) int {
	ctx := context.Background()
	var closer io.Closer
	switch mode {
	case Offline:
		o, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true)
		if err != nil {
			fmt.Println("ERR", err)
			return 3
		}
		closer = o
	case Server:
		s, err := adminlock.AcquireServerStart(ctx, adminDir, dataRoot, adminlock.ServerOptions{})
		if err == nil {
			err = s.ReleaseAdmin()
		}
		if err != nil {
			fmt.Println("ERR", err)
			return 3
		}
		closer = s
	default:
		fmt.Println("ERR unknown mode", mode)
		return 2
	}
	fmt.Println(ready)
	// Hold until the parent closes stdin.
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := closer.Close(); err != nil {
		fmt.Println("ERR", err)
		return 3
	}
	return 0
}

// Holder is a running holder process.
type Holder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	PID   int
}

// Hold starts a holder process that takes the locks of mode and returns once it
// holds them. The process is stopped at the end of the test if Release was not
// called.
func Hold(t *testing.T, mode Mode, adminDir, dataRoot string) *Holder {
	t.Helper()
	// #nosec G204 -- re-executes the running test binary itself as the holder.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), modeEnv+"="+string(mode), adminEnv+"="+adminDir, dataEnv+"="+dataRoot)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &Holder{cmd: cmd, stdin: stdin, PID: cmd.Process.Pid}
	t.Cleanup(func() {
		_ = h.stdin.Close()
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
	})

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case got := <-line:
		if got != ready {
			t.Fatalf("holder pid %d: %q, want %q", h.PID, got, ready)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("holder pid %d did not take the locks", h.PID)
	}
	if h.PID == os.Getpid() {
		t.Fatal("holder runs in the test process")
	}
	return h
}

// Release lets the holder release its locks and exit, and waits for it.
func (h *Holder) Release(t *testing.T) {
	t.Helper()
	if err := h.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("holder pid %d: %v", h.PID, err)
	}
}
