package admininit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/internal/adminlock/locktest"
)

// TestMain makes the test user the init principal and its group the sori group,
// and stops the G1 ancestor walk below the temporary directory, whose own
// ancestors the tests do not control.
func TestMain(m *testing.M) {
	locktest.RunHolderIfRequested()
	tmp, err := filepath.EvalSymlinks(testTempBase())
	if err != nil {
		panic(err)
	}
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid()) // #nosec G115 -- ids fit 32 bits.
	admininit.SetTestTrust(uid, gid, uid, tmp)
	if crashChild != nil {
		crashChild()
	}
	os.Exit(m.Run())
}

// crashChild, set by the Linux crash-cut tests, runs Init in a re-executed
// test process and kills that process at a cut when the environment asks for
// it; otherwise it returns at once.
var crashChild func()

// testHostID is the explicit server.host_id every test passes to Init
// (FMT-PREP-r2 §8.2: no fallback).
const testHostID = "h1"

// refusalReason returns the reason of an init refusal, or "" for any other
// error or none.
func refusalReason(err error) string {
	var r *admininit.Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// testTempBase is the directory t.TempDir creates its directories in: GOTMPDIR
// when it is set (the Makefile sets it), else os.TempDir.
func testTempBase() string {
	if dir := os.Getenv("GOTMPDIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// layout returns an admin area, a trusted data root and the two database paths
// inside it.
func layout(t *testing.T) (adminDir, root string, p admininit.Paths) {
	t.Helper()
	adminDir, root = t.TempDir(), trustedDir(t)
	return adminDir, root, admininit.Paths{JobLedger: filepath.Join(root, "jobs.db"), Authority: filepath.Join(root, "authority.db")}
}

// trustedDir returns a new temporary directory in the G1 layout: mode 2770 with
// the test user's group. Init needs file birth times, which only its Linux
// build reads, so other platforms skip.
func trustedDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Init needs Linux statx birth times")
	}
	dir := t.TempDir()
	trust(t, dir)
	return dir
}

// trust gives an existing directory the G1 mode 2770.
func trust(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
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
	res, err := admininit.Init(ctx, adminDir, root, testHostID, p, countingID(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Bootstrapped || calls != 1 || !probed {
		t.Fatalf("res = %+v, newID calls = %d, probed = %v", res, calls, probed)
	}
	checkInitialized(t, p, res.InstallationID)
	want := []string{adminlock.AdminLockName, admininit.EpochName, admininit.IntentName, admininit.KeysDirName, admininit.PolicyDirName}
	if got := names(t, adminDir); !slices.Equal(got, want) {
		t.Fatalf("admin area = %v, want %v", got, want)
	}

	// Both locks are released when Init returns.
	s, err := adminlock.AcquireServerStart(ctx, adminDir, root, adminlock.ServerOptions{})
	if err != nil {
		t.Fatalf("server start after init: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A complete init refuses a rerun (C6g).
	if _, err := admininit.Init(ctx, adminDir, root, testHostID, p, noID(t)); refusalReason(err) != admininit.ReasonAlreadyComplete {
		t.Fatalf("rerun after epoch = %v, want %s", err, admininit.ReasonAlreadyComplete)
	}
	// With epoch absent (a lost epoch publication, C6f), a rerun reuses the
	// installation id.
	if err := os.Remove(filepath.Join(adminDir, admininit.EpochName)); err != nil {
		t.Fatal(err)
	}
	res2, err := admininit.Init(ctx, adminDir, root, testHostID, p, noID(t))
	if err != nil || res2.Bootstrapped || res2.InstallationID != res.InstallationID {
		t.Fatalf("rerun = %+v, %v; want reuse of %q", res2, err, res.InstallationID)
	}
}

// Two processes: while a server holds server.lock, init returns SERVER_RUNNING
// before it opens either database, so no database, WAL or marker exists afterwards.
func TestInitLosesToRunningServerBeforeDB(t *testing.T) {
	adminDir, root, p := layout(t)
	h := locktest.Hold(t, locktest.Server, adminDir, root)

	_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, noID(t))
	if !errors.Is(err, adminlock.ErrServerRunning) {
		t.Fatalf("err = %v, want ErrServerRunning", err)
	}
	if got := names(t, root); !slices.Equal(got, []string{adminlock.ServerLockName}) {
		t.Fatalf("data root = %v, want only server.lock", got)
	}

	h.Release(t)
	var calls int
	if _, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, countingID(&calls)); err != nil || calls != 1 {
		t.Fatalf("init after the server exited: %v, newID calls = %d", err, calls)
	}
}

// A data root spelled through link/.. names the directory the kernel resolves,
// not the lexically cleaned one. With top/link pointing at canon/child, init
// through top/link/.. must find the server that holds canon/server.lock.
func TestInitLosesToRunningServerThroughDataRootAlias(t *testing.T) {
	adminDir, canon, top := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(canon, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(canon, "child"), filepath.Join(top, "link"))
	h := locktest.Hold(t, locktest.Server, adminDir, canon)
	defer h.Release(t)

	sep := string(filepath.Separator)
	alias := top + sep + "link" + sep + ".."
	p := admininit.Paths{JobLedger: alias + sep + "jobs.db", Authority: alias + sep + "authority.db"}
	_, err := admininit.Init(context.Background(), adminDir, alias, testHostID, p, noID(t))
	if !errors.Is(err, adminlock.ErrServerRunning) {
		t.Fatalf("err = %v, want ErrServerRunning", err)
	}
	if got := names(t, canon); !slices.Equal(got, []string{"child", adminlock.ServerLockName}) {
		t.Fatalf("resolved data root = %v, want only child and server.lock", got)
	}
	if got := names(t, top); !slices.Equal(got, []string{"link"}) {
		t.Fatalf("lexical data root = %v, want only link", got)
	}
}

// Two processes: a data-root symlink retargeted after the check moves neither
// the lock nor the databases. top/root points at a when init checks it; before
// the locks are taken, the link is swapped to b, whose server.lock a running
// server holds. Init must still lock a/server.lock and create the databases in
// a, so b holds nothing but its server's lock.
func TestInitPinsDataRootAgainstRetarget(t *testing.T) {
	adminDir, a, b, top := t.TempDir(), trustedDir(t), t.TempDir(), t.TempDir()
	link := filepath.Join(top, "root")
	symlink(t, a, link)
	h := locktest.Hold(t, locktest.Server, t.TempDir(), b)
	defer h.Release(t)
	defer admininit.SetAfterCheck(func() { retarget(t, b, link) })()

	p := admininit.Paths{JobLedger: filepath.Join(link, "jobs.db"), Authority: filepath.Join(link, "authority.db")}
	var calls int
	res, err := admininit.Init(context.Background(), adminDir, link, testHostID, p, countingID(&calls))
	if err != nil || calls != 1 {
		t.Fatalf("init: %v, newID calls = %d", err, calls)
	}
	if got := names(t, b); !slices.Equal(got, []string{adminlock.ServerLockName}) {
		t.Fatalf("retargeted root = %v, want only server.lock", got)
	}
	if got := names(t, a); !slices.Contains(got, adminlock.ServerLockName) {
		t.Fatalf("checked root = %v, want server.lock", got)
	}
	checkInitialized(t, admininit.Paths{JobLedger: filepath.Join(a, "jobs.db"), Authority: filepath.Join(a, "authority.db")}, res.InstallationID)
}

// A directory link inside the data root is pinned the same way: root/dir points
// at root/sub when init checks it and is swapped to a directory outside the
// root after the check. The job ledger must land in root/sub.
func TestInitPinsDatabasePathAgainstRetarget(t *testing.T) {
	adminDir, root, p := layout(t)
	sub, outside := filepath.Join(root, "sub"), t.TempDir()
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	trust(t, sub)
	link := filepath.Join(root, "dir")
	symlink(t, sub, link)
	p.JobLedger = filepath.Join(link, "jobs.db")
	defer admininit.SetAfterCheck(func() { retarget(t, outside, link) })()

	var calls int
	res, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, countingID(&calls))
	if err != nil || calls != 1 {
		t.Fatalf("init: %v, newID calls = %d", err, calls)
	}
	if got := names(t, outside); len(got) != 0 {
		t.Fatalf("outside = %v, want empty", got)
	}
	checkInitialized(t, admininit.Paths{JobLedger: filepath.Join(sub, "jobs.db"), Authority: p.Authority}, res.InstallationID)
}

// retarget atomically replaces the symlink link with one pointing at target.
func retarget(t *testing.T, target, link string) {
	t.Helper()
	tmp := link + ".new"
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
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

			_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, noID(t))
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
	// Authority is relative and inside the root, so only the job ledger can be refused.
	p.JobLedger, p.Authority = "link"+sep+".."+sep+"jobs.db", "authority.db"
	preParent := names(t, filepath.Dir(outside))
	if _, err := admininit.Init(context.Background(), adminDir, ".", testHostID, p, noID(t)); !errors.Is(err, admininit.ErrRefused) {
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
	target := trustedDir(t)
	if err := os.Mkdir(filepath.Join(target, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	trust(t, filepath.Join(target, "sub"))
	root := filepath.Join(t.TempDir(), "root")
	symlink(t, target, root)
	symlink(t, filepath.Join(target, "sub"), filepath.Join(target, "inner"))
	p := admininit.Paths{JobLedger: filepath.Join(root, "inner", "jobs.db"), Authority: filepath.Join(root, "authority.db")}
	var calls int
	if _, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, countingID(&calls)); err != nil || calls != 1 {
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
			_, err := admininit.Init(context.Background(), adminDir, root, testHostID, paths, noID(t))
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
