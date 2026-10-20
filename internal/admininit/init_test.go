package admininit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/internal/adminlock/locktest"
)

func TestMain(m *testing.M) {
	locktest.RunHolderIfRequested()
	os.Exit(m.Run())
}

// layout returns an admin area, a data root and the two database paths inside it.
func layout(t *testing.T) (adminDir, root string, p admininit.Paths) {
	t.Helper()
	adminDir, root = t.TempDir(), t.TempDir()
	return adminDir, root, admininit.Paths{JobLedger: filepath.Join(root, "jobs.db"), Authority: filepath.Join(root, "authority.db")}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(des))
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

func TestInitHoldsLocksAndBootstraps(t *testing.T) {
	adminDir, root, p := layout(t)
	ctx := context.Background()

	// While the database phase runs, a server start cannot take admin.lock.
	probed := false
	defer admininit.SetAfterStep(func(admininit.Step) error {
		if probed {
			return nil
		}
		probed = true
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, err := adminlock.AcquireServerStart(short, adminDir, t.TempDir(), adminlock.ServerOptions{})
		if !errors.Is(err, adminlock.ErrAdminBusy) {
			t.Errorf("server start during init: %v, want ErrAdminBusy", err)
		}
		return nil
	})()

	var calls int
	res, err := admininit.Init(ctx, adminDir, root, p, countingID(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Bootstrapped || calls != 1 || !probed {
		t.Fatalf("res = %+v, newID calls = %d, probed = %v", res, calls, probed)
	}
	checkInitialized(t, p, res.InstallationID)
	if got := names(t, adminDir); !slices.Equal(got, []string{adminlock.AdminLockName}) {
		t.Fatalf("admin area = %v, want only admin.lock", got)
	}

	// Both locks are released when Init returns.
	s, err := adminlock.AcquireServerStart(ctx, adminDir, root, adminlock.ServerOptions{})
	if err != nil {
		t.Fatalf("server start after init: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A rerun reuses the installation id.
	res2, err := admininit.Init(ctx, adminDir, root, p, noID(t))
	if err != nil || res2.Bootstrapped || res2.InstallationID != res.InstallationID {
		t.Fatalf("rerun = %+v, %v; want reuse of %q", res2, err, res.InstallationID)
	}
}

// Two processes: while a server holds server.lock, init returns SERVER_RUNNING
// before it opens either database, so no database, WAL or marker exists afterwards.
func TestInitLosesToRunningServerBeforeDB(t *testing.T) {
	adminDir, root, p := layout(t)
	h := locktest.Hold(t, locktest.Server, adminDir, root)

	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	if !errors.Is(err, adminlock.ErrServerRunning) {
		t.Fatalf("err = %v, want ErrServerRunning", err)
	}
	if got := names(t, root); !slices.Equal(got, []string{adminlock.ServerLockName}) {
		t.Fatalf("data root = %v, want only server.lock", got)
	}

	h.Release(t)
	var calls int
	if _, err := admininit.Init(context.Background(), adminDir, root, p, countingID(&calls)); err != nil || calls != 1 {
		t.Fatalf("init after the server exited: %v, newID calls = %d", err, calls)
	}
}

// A database path outside the data root is refused before any lock file is created.
func TestInitRefusesPathOutsideDataRoot(t *testing.T) {
	adminDir, root, p := layout(t)
	outside := t.TempDir()
	cases := map[string]admininit.Paths{
		"job ledger outside": {JobLedger: filepath.Join(outside, "jobs.db"), Authority: p.Authority},
		"authority outside":  {JobLedger: p.JobLedger, Authority: filepath.Join(outside, "authority.db")},
		"dot-dot":            {JobLedger: filepath.Join(root, "..", "jobs.db"), Authority: p.Authority},
		"the root itself":    {JobLedger: root, Authority: p.Authority},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := admininit.Init(context.Background(), adminDir, root, paths, noID(t))
			if !errors.Is(err, admininit.ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
			if got := names(t, adminDir); len(got) != 0 {
				t.Fatalf("admin area = %v, want empty", got)
			}
			if got := names(t, root); len(got) != 0 {
				t.Fatalf("data root = %v, want empty", got)
			}
		})
	}
}
