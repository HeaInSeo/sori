package adminlock_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/internal/adminlock/locktest"
)

func TestMain(m *testing.M) {
	locktest.RunHolderIfRequested()
	os.Exit(m.Run())
}

// areas returns an admin area and a data root in separate temporary directories.
func areas(t *testing.T) (adminDir, dataRoot string) {
	t.Helper()
	return t.TempDir(), t.TempDir()
}

// entries lists the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

// recorder collects Logf lines.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) count(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func TestValidateAdminLockWait(t *testing.T) {
	for _, d := range []time.Duration{0, time.Minute, 15 * time.Minute, 60 * time.Minute} {
		if err := adminlock.ValidateAdminLockWait(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	for _, d := range []time.Duration{time.Second, 59 * time.Second, 60*time.Minute + time.Second, -time.Minute} {
		if err := adminlock.ValidateAdminLockWait(d); !errors.Is(err, adminlock.ErrAdminLockWait) {
			t.Errorf("%s: %v, want ErrAdminLockWait", d, err)
		}
	}
}

// An out-of-range admin_lock_wait is refused before any file is opened or created.
func TestServerStartRefusesWaitOutOfRange(t *testing.T) {
	adminDir, dataRoot := areas(t)
	_, err := adminlock.AcquireServerStart(context.Background(), adminDir, dataRoot, adminlock.ServerOptions{AdminLockWait: 30 * time.Second})
	if !errors.Is(err, adminlock.ErrAdminLockWait) {
		t.Fatalf("err = %v, want ErrAdminLockWait", err)
	}
	if got := entries(t, dataRoot); len(got) != 0 {
		t.Fatalf("data root = %v, want empty", got)
	}
}

// L2 (ii), two processes: a second server start loses server.lock and returns
// SERVER_LOCK_HELD with no lock held.
func TestServerStartLoserTwoProcesses(t *testing.T) {
	adminDir, dataRoot := areas(t)
	ctx := context.Background()
	h := locktest.Hold(t, locktest.Server, adminDir, dataRoot)

	_, err := adminlock.AcquireServerStart(ctx, adminDir, dataRoot, adminlock.ServerOptions{})
	if !errors.Is(err, adminlock.ErrServerLockHeld) {
		t.Fatalf("second start: %v, want ErrServerLockHeld", err)
	}
	// The loser holds no admin.lock: an offline command still gets it and is refused
	// only on server.lock.
	if _, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true); !errors.Is(err, adminlock.ErrServerRunning) {
		t.Fatalf("offline while server runs: %v, want ErrServerRunning", err)
	}

	h.Release(t)
	s, err := adminlock.AcquireServerStart(ctx, adminDir, dataRoot, adminlock.ServerOptions{})
	if err != nil {
		t.Fatalf("start after the first server exited: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// L3, two processes: an offline command is refused with SERVER_RUNNING while a
// server holds server.lock, and it releases admin.lock when refused.
func TestOfflineRefusedWhileServerRunsTwoProcesses(t *testing.T) {
	adminDir, dataRoot := areas(t)
	ctx := context.Background()
	locktest.Hold(t, locktest.Server, adminDir, dataRoot)

	_, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true)
	if !errors.Is(err, adminlock.ErrServerRunning) {
		t.Fatalf("err = %v, want ErrServerRunning", err)
	}
	// admin.lock was released: a shared lock is free at once.
	s, err := adminlock.AcquireServerStartWait(ctx, adminDir, t.TempDir(), 200*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("admin.lock still held after the refusal: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// L2 (i) and K16, two processes: a server start waits while a sori-admin command
// holds admin.lock, logs WAITING_ADMIN_LOCK, and starts after the command exits.
func TestServerStartWaitsForAdminCommandTwoProcesses(t *testing.T) {
	defer adminlock.SetIntervals(10*time.Millisecond, 50*time.Millisecond)()
	adminDir, dataRoot := areas(t)
	h := locktest.Hold(t, locktest.Offline, adminDir, dataRoot)

	rec := &recorder{}
	type result struct {
		s   *adminlock.Server
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := adminlock.AcquireServerStartWait(context.Background(), adminDir, dataRoot, 30*time.Second, rec.logf)
		done <- result{s, err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for rec.count("WAITING_ADMIN_LOCK") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no repeated WAITING_ADMIN_LOCK while the admin command holds admin.lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("start returned while the admin command runs: %v", r.err)
	default:
	}

	h.Release(t)
	r := <-done
	if r.err != nil {
		t.Fatalf("start after the admin command exited: %v", r.err)
	}
	if r.s.AdminLockMissing {
		t.Fatal("AdminLockMissing with admin.lock present")
	}
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
}

// L2 (i), two processes: when admin_lock_wait runs out the start returns ADMIN_BUSY
// without taking server.lock.
func TestServerStartAdminBusyTwoProcesses(t *testing.T) {
	defer adminlock.SetIntervals(10*time.Millisecond, time.Hour)()
	adminDir, dataRoot := areas(t)
	h := locktest.Hold(t, locktest.Offline, adminDir, dataRoot)
	otherRoot := t.TempDir()

	start := time.Now()
	_, err := adminlock.AcquireServerStartWait(context.Background(), adminDir, otherRoot, 300*time.Millisecond, nil)
	if !errors.Is(err, adminlock.ErrAdminBusy) {
		t.Fatalf("err = %v, want ErrAdminBusy", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("returned after %s, before admin_lock_wait", waited)
	}
	// The start never reached server.lock (L1: admin.lock first).
	if got := entries(t, otherRoot); len(got) != 0 {
		t.Fatalf("data root = %v, want empty", got)
	}
	h.Release(t)
}

// An offline command waits for admin.lock while another command holds it, and
// gives up with ADMIN_BUSY when its context ends.
func TestOfflineWaitsForAnotherAdminCommand(t *testing.T) {
	defer adminlock.SetIntervals(10*time.Millisecond, time.Hour)()
	adminDir, dataRoot := areas(t)
	h := locktest.Hold(t, locktest.Offline, adminDir, dataRoot)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true); !errors.Is(err, adminlock.ErrAdminBusy) {
		t.Fatalf("err = %v, want ErrAdminBusy", err)
	}

	h.Release(t)
	o, err := adminlock.AcquireOffline(context.Background(), adminDir, dataRoot, true)
	if err != nil {
		t.Fatalf("after the other command exited: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
}

// aliasOf returns a spelling of dir through a link and "..": top/link/.. with
// top/link pointing at dir/child. The kernel resolves it to dir; filepath.Clean
// makes it top. It skips where symlinks are unavailable.
func aliasOf(t *testing.T, dir string) (alias, top string) {
	t.Helper()
	top = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "child"), filepath.Join(top, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	sep := string(filepath.Separator)
	return top + sep + "link" + sep + "..", top
}

// Both locks are found through any spelling of their directory: an offline
// command that names the admin area or the data root through link/.. meets the
// locks held under the resolved directory and creates no lock file elsewhere.
func TestLocksFollowDirectoryAlias(t *testing.T) {
	defer adminlock.SetIntervals(10*time.Millisecond, time.Hour)()
	ctx := context.Background()

	t.Run("admin.lock", func(t *testing.T) {
		adminDir, dataRoot := areas(t)
		alias, top := aliasOf(t, adminDir)
		h := locktest.Hold(t, locktest.Offline, adminDir, dataRoot)
		defer h.Release(t)
		short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if _, err := adminlock.AcquireOffline(short, alias, t.TempDir(), true); !errors.Is(err, adminlock.ErrAdminBusy) {
			t.Fatalf("offline: %v, want ErrAdminBusy", err)
		}
		if _, err := adminlock.AcquireServerStartWait(ctx, alias, t.TempDir(), 200*time.Millisecond, nil); !errors.Is(err, adminlock.ErrAdminBusy) {
			t.Fatalf("server start: %v, want ErrAdminBusy", err)
		}
		if got := entries(t, top); len(got) != 1 || got[0] != "link" {
			t.Fatalf("lexical admin area = %v, want only link", got)
		}
	})

	t.Run("server.lock", func(t *testing.T) {
		adminDir, dataRoot := areas(t)
		alias, top := aliasOf(t, dataRoot)
		s, err := adminlock.AcquireServerStart(ctx, adminDir, dataRoot, adminlock.ServerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := adminlock.AcquireOffline(ctx, t.TempDir(), alias, true); !errors.Is(err, adminlock.ErrServerRunning) {
			t.Fatalf("offline: %v, want ErrServerRunning", err)
		}
		if _, err := adminlock.AcquireServerStart(ctx, t.TempDir(), alias, adminlock.ServerOptions{}); !errors.Is(err, adminlock.ErrServerLockHeld) {
			t.Fatalf("server start: %v, want ErrServerLockHeld", err)
		}
		if got := entries(t, top); len(got) != 1 || got[0] != "link" {
			t.Fatalf("lexical data root = %v, want only link", got)
		}
	})
}

// An empty directory means the working directory, as with filepath.Join.
func TestLocksInWorkingDirectoryForEmptyDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	o, err := adminlock.AcquireOffline(context.Background(), "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, dir); len(got) != 2 || got[0] != adminlock.AdminLockName || got[1] != adminlock.ServerLockName {
		t.Fatalf("working directory = %v, want admin.lock and server.lock", got)
	}
}

// Without admin.lock (init has not run) a server start goes on and reports it.
func TestServerStartWithoutAdminLock(t *testing.T) {
	adminDir, dataRoot := areas(t)
	s, err := adminlock.AcquireServerStart(context.Background(), adminDir, dataRoot, adminlock.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.AdminLockMissing {
		t.Fatal("AdminLockMissing = false without admin.lock")
	}
	if got := entries(t, adminDir); len(got) != 0 {
		t.Fatalf("admin area = %v; a server start must not create admin.lock", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// Only init creates admin.lock; other offline commands need it to exist.
func TestOfflineCreatesAdminLockOnlyForInit(t *testing.T) {
	adminDir, dataRoot := areas(t)
	ctx := context.Background()
	if _, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-init without admin.lock: %v, want not exist", err)
	}
	if got := entries(t, dataRoot); len(got) != 0 {
		t.Fatalf("data root = %v, want empty", got)
	}
	o, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(adminDir, adminlock.AdminLockName))
	if err != nil || fi.Size() != 0 {
		t.Fatalf("admin.lock = %v, %v; want an empty file", fi, err)
	}
	o, err = adminlock.AcquireOffline(ctx, adminDir, dataRoot, false)
	if err != nil {
		t.Fatalf("non-init after init created admin.lock: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
}

// After ReleaseAdmin an offline command can take admin.lock but is still refused
// on server.lock, as with a running server (L2 (iv)).
func TestServerReleaseAdminKeepsServerLock(t *testing.T) {
	defer adminlock.SetIntervals(10*time.Millisecond, time.Hour)()
	adminDir, dataRoot := areas(t)
	ctx := context.Background()
	o, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := adminlock.AcquireServerStart(ctx, adminDir, dataRoot, adminlock.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	// Before ReleaseAdmin the shared admin.lock blocks an offline command.
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := adminlock.AcquireOffline(short, adminDir, dataRoot, false); !errors.Is(err, adminlock.ErrAdminBusy) {
		t.Fatalf("offline before ReleaseAdmin: %v, want ErrAdminBusy", err)
	}
	if err := s.ReleaseAdmin(); err != nil {
		t.Fatal(err)
	}
	after, cancelAfter := context.WithTimeout(ctx, 2*time.Second)
	defer cancelAfter()
	if _, err := adminlock.AcquireOffline(after, adminDir, dataRoot, false); !errors.Is(err, adminlock.ErrServerRunning) {
		t.Fatalf("offline after ReleaseAdmin: %v, want ErrServerRunning", err)
	}
}

// The server's admin.lock is shared: two starts on different data roots both get it
// at once, and neither blocks the other.
func TestServerStartAdminLockIsShared(t *testing.T) {
	adminDir, dataRoot := areas(t)
	ctx := context.Background()
	o, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}

	first, err := adminlock.AcquireServerStartWait(ctx, adminDir, dataRoot, 200*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	}()
	second, err := adminlock.AcquireServerStartWait(ctx, adminDir, t.TempDir(), 200*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("second start while the first holds admin.lock shared: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
