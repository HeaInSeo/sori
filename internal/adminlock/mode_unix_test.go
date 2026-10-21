//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package adminlock_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/HeaInSeo/sori/internal/adminlock"
)

// acquire takes the locks of an offline init or of a server start.
func acquire(t *testing.T, offline bool, adminDir, dataRoot string) (interface{ Close() error }, error) {
	t.Helper()
	ctx := context.Background()
	if offline {
		o, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true)
		if err != nil {
			return nil, err
		}
		return o, nil
	}
	s, err := adminlock.AcquireServerStart(ctx, adminDir, dataRoot, adminlock.ServerOptions{})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// serverLockMode returns the permission bits of dataRoot/server.lock.
func serverLockMode(t *testing.T, dataRoot string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(dataRoot, adminlock.ServerLockName))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("server.lock mode = %s, want a regular file", fi.Mode())
	}
	return fi.Mode().Perm()
}

var lockPaths = []struct {
	name    string
	offline bool
}{{"offline", true}, {"server start", false}}

// Codex P1 4339947250: a server.lock that a process creates is 0660 whatever its
// umask, so a server of the sori group under another uid can open it read-write.
func TestServerLockCreatedWithGroupModeUnderUmask(t *testing.T) {
	for _, mask := range []int{0o022, 0o027, 0o077} {
		for _, p := range lockPaths {
			t.Run(fmt.Sprintf("umask %04o %s", mask, p.name), func(t *testing.T) {
				adminDir, dataRoot := areas(t)
				old := syscall.Umask(mask)
				l, err := acquire(t, p.offline, adminDir, dataRoot)
				syscall.Umask(old)
				if err != nil {
					t.Fatal(err)
				}
				if got := serverLockMode(t, dataRoot); got != 0o660 {
					t.Errorf("created server.lock mode = %04o, want 0660", got)
				}
				noTemporaryNames(t, dataRoot)
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
				// The next holder opens the same file and the mode stays 0660.
				l, err = acquire(t, p.offline, adminDir, dataRoot)
				if err != nil {
					t.Fatal(err)
				}
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
				if got := serverLockMode(t, dataRoot); got != 0o660 {
					t.Errorf("reopened server.lock mode = %04o, want 0660", got)
				}
			})
		}
	}
}

// An existing server.lock is opened as it is: its mode and inode do not change.
func TestServerLockKeepsExistingFile(t *testing.T) {
	for _, mode := range []fs.FileMode{0o600, 0o640, 0o644} {
		for _, p := range lockPaths {
			t.Run(fmt.Sprintf("%04o %s", mode, p.name), func(t *testing.T) {
				adminDir, dataRoot := areas(t)
				path := filepath.Join(dataRoot, adminlock.ServerLockName)
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				before, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				l, err := acquire(t, p.offline, adminDir, dataRoot)
				if err != nil {
					t.Fatal(err)
				}
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
				after, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(before, after) {
					t.Error("server.lock was replaced")
				}
				if got := after.Mode().Perm(); got != mode {
					t.Errorf("existing server.lock mode = %04o, want %04o unchanged", got, mode)
				}
			})
		}
	}
}

// When another process creates server.lock after the open found none, the
// exclusive create fails, the existing file is opened and its mode is not
// changed: only the creator sets the mode.
func TestServerLockCreatedConcurrentlyIsNotChanged(t *testing.T) {
	for _, p := range lockPaths {
		t.Run(p.name, func(t *testing.T) {
			adminDir, dataRoot := areas(t)
			path := filepath.Join(dataRoot, adminlock.ServerLockName)
			calls := 0
			defer adminlock.SetBeforeLockCreate(func(name string) {
				if name != adminlock.ServerLockName {
					return
				}
				calls++
				if calls == 1 {
					if err := os.WriteFile(path, nil, 0o600); err != nil {
						t.Error(err)
					}
				}
			})()
			l, err := acquire(t, p.offline, adminDir, dataRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Errorf("create hook ran %d times, want 1", calls)
			}
			if got := serverLockMode(t, dataRoot); got != 0o600 {
				t.Errorf("concurrently created server.lock mode = %04o, want 0600 unchanged", got)
			}
		})
	}
}

// A dangling symlink at server.lock is refused: no lock is held and the link
// target is not created.
func TestServerLockRefusesDanglingSymlink(t *testing.T) {
	for _, p := range lockPaths {
		t.Run(p.name, func(t *testing.T) {
			adminDir, dataRoot := areas(t)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.Symlink(target, filepath.Join(dataRoot, adminlock.ServerLockName)); err != nil {
				t.Fatal(err)
			}
			l, err := acquire(t, p.offline, adminDir, dataRoot)
			if err == nil {
				_ = l.Close()
				t.Fatal("acquired the locks through a dangling server.lock symlink")
			}
			if !errors.Is(err, fs.ErrExist) {
				t.Errorf("err = %v, want it to wrap fs.ErrExist", err)
			}
			if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("symlink target: %v, want not created", err)
			}
			// admin.lock was released: another init can take it at once.
			o, err := adminlock.AcquireOffline(context.Background(), adminDir, t.TempDir(), true)
			if err != nil {
				t.Fatalf("admin.lock still held after the refusal: %v", err)
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// lockMode returns the permission bits of dir/name, which must be a regular file.
func lockMode(t *testing.T, dir, name string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s mode = %s, want a regular file", name, fi.Mode())
	}
	return fi.Mode().Perm()
}

// noTemporaryNames fails if dir holds a temporary name of a lock file creation.
func noTemporaryNames(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary name %s left in %s", e.Name(), dir)
		}
	}
}

// initLocks takes the locks of an offline init, which creates admin.lock.
func initLocks(adminDir, dataRoot string) (*adminlock.Offline, error) {
	return adminlock.AcquireOffline(context.Background(), adminDir, dataRoot, true)
}

// Codex P1 4340737885: the admin.lock that init creates is 0640 whatever its
// umask, so a server of the sori group under another uid can open it for its
// shared lock. Neither creation leaves a temporary name behind.
func TestAdminLockCreatedWithGroupModeUnderUmask(t *testing.T) {
	for _, mask := range []int{0o022, 0o027, 0o077} {
		t.Run(fmt.Sprintf("umask %04o", mask), func(t *testing.T) {
			adminDir, dataRoot := areas(t)
			old := syscall.Umask(mask)
			o, err := initLocks(adminDir, dataRoot)
			syscall.Umask(old)
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			if got := lockMode(t, adminDir, adminlock.AdminLockName); got != 0o640 {
				t.Errorf("created admin.lock mode = %04o, want 0640", got)
			}
			noTemporaryNames(t, adminDir)
			noTemporaryNames(t, dataRoot)
			// A later offline command opens the same file and the mode stays 0640.
			o, err = adminlock.AcquireOffline(context.Background(), adminDir, dataRoot, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			if got := lockMode(t, adminDir, adminlock.AdminLockName); got != 0o640 {
				t.Errorf("reopened admin.lock mode = %04o, want 0640", got)
			}
		})
	}
}

// An existing admin.lock is opened as it is by init: its mode and inode do not
// change.
func TestAdminLockKeepsExistingFile(t *testing.T) {
	for _, mode := range []fs.FileMode{0o600, 0o644, 0o660} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			adminDir, dataRoot := areas(t)
			path := filepath.Join(adminDir, adminlock.AdminLockName)
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			o, err := initLocks(adminDir, dataRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Error("admin.lock was replaced")
			}
			if got := after.Mode().Perm(); got != mode {
				t.Errorf("existing admin.lock mode = %04o, want %04o unchanged", got, mode)
			}
		})
	}
}

// When another process creates admin.lock after init found none, init opens that
// file and leaves its mode alone.
func TestAdminLockCreatedConcurrentlyIsNotChanged(t *testing.T) {
	adminDir, dataRoot := areas(t)
	path := filepath.Join(adminDir, adminlock.AdminLockName)
	calls := 0
	defer adminlock.SetBeforeLockCreate(func(name string) {
		if name != adminlock.AdminLockName {
			return
		}
		calls++
		if calls == 1 {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	})()
	o, err := initLocks(adminDir, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("create hook ran %d times, want 1", calls)
	}
	if got := lockMode(t, adminDir, adminlock.AdminLockName); got != 0o600 {
		t.Errorf("concurrently created admin.lock mode = %04o, want 0600 unchanged", got)
	}
	noTemporaryNames(t, adminDir)
}

// A dangling symlink at admin.lock is refused by init: no lock is taken and the
// link target is not created.
func TestAdminLockRefusesDanglingSymlink(t *testing.T) {
	adminDir, dataRoot := areas(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(target, filepath.Join(adminDir, adminlock.AdminLockName)); err != nil {
		t.Fatal(err)
	}
	o, err := initLocks(adminDir, dataRoot)
	if err == nil {
		_ = o.Close()
		t.Fatal("init took the locks through a dangling admin.lock symlink")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("err = %v, want it to wrap fs.ErrExist", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("symlink target: %v, want not created", err)
	}
	if _, err := os.Lstat(filepath.Join(dataRoot, adminlock.ServerLockName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("server.lock: %v, want not created", err)
	}
	noTemporaryNames(t, adminDir)
}

// errCut stands for a crash of the creating process.
var errCut = errors.New("creation cut")

// cutCreation runs acquire with the creation of the lock file name cut where a
// crash could cut it: after the new file has its mode and before it has the lock
// file's name. It checks that at that point the name does not exist and the new
// file already has want, in the same directory, and that the cut leaves no lock
// file behind.
func cutCreation(t *testing.T, dir, name string, want fs.FileMode, acquire func() (interface{ Close() error }, error)) {
	t.Helper()
	path := filepath.Join(dir, name)
	cuts := 0
	restore := adminlock.SetBeforeLockPublish(func(n, tmp string) {
		if n != name {
			return
		}
		cuts++
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s before its publication: %v, want not to exist", name, err)
		}
		fi, err := os.Lstat(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s to be published has mode %04o, want %04o", name, got, want)
		}
		if a, b := filepath.Clean(filepath.Dir(tmp)), filepath.Clean(dir); a != b {
			t.Errorf("%s created in %s, want %s", name, a, b)
		}
		panic(errCut)
	})
	func() {
		defer func() {
			if r := recover(); r != errCut {
				t.Errorf("creation was not cut: recover() = %v", r)
			}
		}()
		l, err := acquire()
		if err == nil {
			_ = l.Close()
		}
	}()
	restore()
	if cuts != 1 {
		t.Errorf("publication hook ran %d times, want 1", cuts)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s after the cut: %v, want not to exist", name, err)
	}
}

// Codex P2 4340737900: a crash while a lock file is being created never leaves
// the lock file name with a umask-masked mode that every later open would keep.
// The name either does not exist or names a file that has its C.5.8 mode, and
// the next process creates it with that mode.
func TestLockCreationCutPublishesNothing(t *testing.T) {
	t.Run("admin.lock by init", func(t *testing.T) {
		adminDir, dataRoot := areas(t)
		old := syscall.Umask(0o077)
		defer syscall.Umask(old)
		cutCreation(t, adminDir, adminlock.AdminLockName, 0o640, func() (interface{ Close() error }, error) {
			return initLocks(adminDir, dataRoot)
		})
		o, err := initLocks(adminDir, dataRoot)
		if err != nil {
			t.Fatal(err)
		}
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
		if got := lockMode(t, adminDir, adminlock.AdminLockName); got != 0o640 {
			t.Errorf("admin.lock created after the cut has mode %04o, want 0640", got)
		}
	})
	for _, p := range lockPaths {
		t.Run("server.lock by "+p.name, func(t *testing.T) {
			adminDir, dataRoot := areas(t)
			old := syscall.Umask(0o077)
			defer syscall.Umask(old)
			cutCreation(t, dataRoot, adminlock.ServerLockName, 0o660, func() (interface{ Close() error }, error) {
				// A server start in an empty admin area holds no admin.lock, so the
				// cut leaves no locked descriptor in this process.
				return adminlock.AcquireServerStart(context.Background(), t.TempDir(), dataRoot, adminlock.ServerOptions{})
			})
			// The next offline command or server start creates server.lock.
			l, err := acquire(t, p.offline, adminDir, dataRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if got := lockMode(t, dataRoot, adminlock.ServerLockName); got != 0o660 {
				t.Errorf("server.lock created after the cut has mode %04o, want 0660", got)
			}
		})
	}
}
