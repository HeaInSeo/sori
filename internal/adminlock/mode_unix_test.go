//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package adminlock_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
			defer adminlock.SetBeforeServerLockCreate(func() {
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
