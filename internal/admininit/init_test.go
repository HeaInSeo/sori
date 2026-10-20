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

// symlink makes link point at target, skipping where symlinks are unavailable.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
}

// A database path that escapes the data root through a symlink is refused
// before any lock file is created, and nothing is created outside the root.
func TestInitRefusesSymlinkEscape(t *testing.T) {
	sep := string(filepath.Separator)
	cases := map[string]func(t *testing.T, root, outside string) string{
		"through a linked directory": func(_ *testing.T, root, _ string) string {
			return filepath.Join(root, "link", "jobs.db")
		},
		// filepath.Clean would make this root/jobs.db, but the kernel follows
		// link first and opens a file in outside's parent.
		"link then dot-dot": func(_ *testing.T, root, _ string) string {
			return root + sep + "link" + sep + ".." + sep + "jobs.db"
		},
		"final entry is a link": func(t *testing.T, root, outside string) string {
			db := filepath.Join(outside, "jobs.db")
			if err := os.WriteFile(db, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			symlink(t, db, filepath.Join(root, "ledger.db"))
			return filepath.Join(root, "ledger.db")
		},
		"dangling final link": func(t *testing.T, root, outside string) string {
			symlink(t, filepath.Join(outside, "absent.db"), filepath.Join(root, "ledger.db"))
			return filepath.Join(root, "ledger.db")
		},
		"final name dot-dot": func(_ *testing.T, root, _ string) string {
			return root + sep + "link" + sep + ".."
		},
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			symlink(t, outside, filepath.Join(root, "link"))
			p.JobLedger = path(t, root, outside)
			preRoot := names(t, root)
			preOutside := names(t, outside)
			preParent := names(t, filepath.Dir(outside))

			_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
			if !errors.Is(err, admininit.ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
			if got := names(t, adminDir); len(got) != 0 {
				t.Fatalf("admin area = %v, want empty", got)
			}
			for dir, want := range map[string][]string{root: preRoot, outside: preOutside, filepath.Dir(outside): preParent} {
				if got := names(t, dir); !slices.Equal(got, want) {
					t.Fatalf("%s = %v, want %v", dir, got, want)
				}
			}
		})
	}
}

// Relative paths keep link/.. raw as well: filepath.Abs would clean it away.
func TestInitRefusesRelativeSymlinkEscape(t *testing.T) {
	adminDir, root, p := layout(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, filepath.Join(root, "link"))
	t.Chdir(root)
	sep := string(filepath.Separator)
	p.JobLedger = "link" + sep + ".." + sep + "jobs.db"
	preParent := names(t, filepath.Dir(outside))
	if _, err := admininit.Init(context.Background(), adminDir, ".", p, noID(t)); !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if got := names(t, filepath.Dir(outside)); !slices.Equal(got, preParent) {
		t.Fatalf("outside's parent = %v, want %v", got, preParent)
	}
}

// Symlinks that stay inside the data root are accepted: a symlinked data root
// and a linked subdirectory of it.
func TestInitAcceptsSymlinkInsideDataRoot(t *testing.T) {
	adminDir := t.TempDir()
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	symlink(t, target, root)
	symlink(t, filepath.Join(target, "sub"), filepath.Join(target, "inner"))
	p := admininit.Paths{JobLedger: filepath.Join(root, "inner", "jobs.db"), Authority: filepath.Join(root, "authority.db")}
	var calls int
	if _, err := admininit.Init(context.Background(), adminDir, root, p, countingID(&calls)); err != nil || calls != 1 {
		t.Fatalf("init: %v, newID calls = %d", err, calls)
	}
	if _, err := os.Stat(filepath.Join(target, "sub", "jobs.db")); err != nil {
		t.Fatalf("job ledger: %v", err)
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
