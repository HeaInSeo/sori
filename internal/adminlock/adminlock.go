// Package adminlock implements the two advisory locks of the RW-C1 lock protocol
// (D01.3-r6 C.5.8): admin.lock in the admin area and server.lock in the data root.
// Both are flock locks on files that carry no content and are never deleted.
//
//   - L1, lock order: every process takes admin.lock before server.lock and never
//     waits for admin.lock while holding server.lock.
//   - L2, server start (AcquireServerStart): admin.lock shared with a bounded wait,
//     then server.lock exclusive without waiting. The server releases admin.lock
//     after it has classified its start and keeps server.lock until it exits.
//   - L3, offline command (AcquireOffline): admin.lock exclusive, then server.lock
//     exclusive without waiting. Both are held until the command exits.
//
// A process that loses either lock returns before it opens any database, so it
// writes no schema, WAL or marker. flock needs the same kernel and a local
// filesystem for both areas; NFS and other network filesystems are not supported.
package adminlock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// File names of the two locks.
const (
	AdminLockName  = "admin.lock"
	ServerLockName = "server.lock"
)

// Bounds of the server's admin_lock_wait (C.5.8 L2).
const (
	DefaultAdminLockWait = 15 * time.Minute
	MinAdminLockWait     = time.Minute
	MaxAdminLockWait     = 60 * time.Minute
)

var (
	// ErrAdminBusy: a server start waited admin_lock_wait for a sori-admin command,
	// or an offline command's context ended while it waited for admin.lock.
	ErrAdminBusy = errors.New("adminlock: ADMIN_BUSY")
	// ErrServerLockHeld: a server start found server.lock held by another process.
	ErrServerLockHeld = errors.New("adminlock: SERVER_LOCK_HELD")
	// ErrServerRunning: an offline command found server.lock held by a server.
	ErrServerRunning = errors.New("adminlock: SERVER_RUNNING")
	// ErrAdminLockWait: admin_lock_wait is outside MinAdminLockWait..MaxAdminLockWait.
	ErrAdminLockWait = errors.New("adminlock: admin_lock_wait out of range")
	// ErrUnsupported: this platform has no flock.
	ErrUnsupported = errors.New("adminlock: flock is not supported on this platform")
)

// pollInterval is how often a held lock is tried again. waitLogInterval is how
// often a server start logs WAITING_ADMIN_LOCK while it waits.
var (
	pollInterval    = 100 * time.Millisecond
	waitLogInterval = 30 * time.Second
)

// ValidateAdminLockWait refuses an admin_lock_wait outside 1–60 minutes. Zero is
// valid and means DefaultAdminLockWait.
func ValidateAdminLockWait(d time.Duration) error {
	if d == 0 {
		return nil
	}
	if d < MinAdminLockWait || d > MaxAdminLockWait {
		return fmt.Errorf("%w: %s (allowed %s to %s)", ErrAdminLockWait, d, MinAdminLockWait, MaxAdminLockWait)
	}
	return nil
}

// Offline holds both locks of an offline sori-admin command (L3).
type Offline struct {
	admin  *os.File
	server *os.File
}

// AcquireOffline takes admin.lock in adminDir exclusive, waiting until ctx ends,
// and then dataRoot/server.lock exclusive without waiting. createAdmin is true
// only for init, which creates admin.lock (mode 0640) as the first file it writes;
// every other command requires it to exist. If a server holds server.lock it
// releases admin.lock and returns ErrServerRunning.
func AcquireOffline(ctx context.Context, adminDir, dataRoot string, createAdmin bool) (*Offline, error) {
	if !supported {
		return nil, ErrUnsupported
	}
	var admin *os.File
	var err error
	if createAdmin {
		admin, err = openLock(adminDir, AdminLockName, os.O_RDONLY, adminLockMode)
	} else {
		// #nosec G304 -- fixed lock file name inside the configured admin area.
		admin, err = os.Open(lockPath(adminDir, AdminLockName))
	}
	if err != nil {
		return nil, fmt.Errorf("adminlock: open %s: %w", AdminLockName, err)
	}
	if err := waitLock(ctx, admin, true, nil); err != nil {
		return nil, errors.Join(err, admin.Close())
	}
	server, err := lockServer(dataRoot)
	if err != nil {
		if errors.Is(err, ErrServerLockHeld) {
			err = ErrServerRunning
		}
		return nil, errors.Join(err, admin.Close())
	}
	return &Offline{admin: admin, server: server}, nil
}

// Close releases server.lock and then admin.lock.
func (o *Offline) Close() error {
	return errors.Join(o.server.Close(), o.admin.Close())
}

// ServerOptions configures AcquireServerStart.
type ServerOptions struct {
	// AdminLockWait bounds the wait for admin.lock. Zero means DefaultAdminLockWait.
	AdminLockWait time.Duration
	// Logf, when set, receives WAITING_ADMIN_LOCK lines while the start waits.
	Logf func(format string, args ...any)
}

// Server holds the locks of a server start (L2).
type Server struct {
	admin  *os.File
	server *os.File
	// AdminLockMissing reports that admin.lock does not exist, so the start went on
	// without it. With an activation epoch that is ADMIN_AREA_INVALID; without one
	// init has not run yet. The caller classifies it.
	AdminLockMissing bool
}

// AcquireServerStart takes admin.lock in adminDir shared, waiting at most
// opts.AdminLockWait while a sori-admin command holds it, and then
// dataRoot/server.lock exclusive without waiting. It returns ErrAdminBusy when the
// wait runs out and ErrServerLockHeld when another process holds server.lock; in
// both cases it holds no lock and has opened no database.
func AcquireServerStart(ctx context.Context, adminDir, dataRoot string, opts ServerOptions) (*Server, error) {
	if err := ValidateAdminLockWait(opts.AdminLockWait); err != nil {
		return nil, err
	}
	if opts.AdminLockWait == 0 {
		opts.AdminLockWait = DefaultAdminLockWait
	}
	return acquireServerStart(ctx, adminDir, dataRoot, opts)
}

func acquireServerStart(ctx context.Context, adminDir, dataRoot string, opts ServerOptions) (*Server, error) {
	if !supported {
		return nil, ErrUnsupported
	}
	// #nosec G304 -- fixed lock file name inside the configured admin area.
	admin, err := os.Open(lockPath(adminDir, AdminLockName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		admin = nil
	case err != nil:
		return nil, fmt.Errorf("adminlock: open %s: %w", AdminLockName, err)
	default:
		wctx, cancel := context.WithTimeout(ctx, opts.AdminLockWait)
		err = waitLock(wctx, admin, false, opts.Logf)
		cancel()
		if err != nil {
			return nil, errors.Join(err, admin.Close())
		}
	}
	server, err := lockServer(dataRoot)
	if err != nil {
		if admin != nil {
			err = errors.Join(err, admin.Close())
		}
		return nil, err
	}
	return &Server{admin: admin, server: server, AdminLockMissing: admin == nil}, nil
}

// ReleaseAdmin releases admin.lock once the start is classified (L2 (iv)).
// server.lock stays held.
func (s *Server) ReleaseAdmin() error {
	if s.admin == nil {
		return nil
	}
	err := s.admin.Close()
	s.admin = nil
	return err
}

// Close releases server.lock and, if still held, admin.lock.
func (s *Server) Close() error {
	return errors.Join(s.server.Close(), s.ReleaseAdmin())
}

// Modes C.5.8 gives the two lock files: a server of the sori group running under
// another uid opens admin.lock read-only for its shared lock and server.lock
// read-write.
const (
	adminLockMode  fs.FileMode = 0o640
	serverLockMode fs.FileMode = 0o660
)

// openLockAttempts bounds how often openLock retries when the name is created
// or removed between its open and its publication.
const openLockAttempts = 3

// beforeLockCreate runs after openLock found no lock file and before it creates
// one. beforeLockPublish runs once the new file has its mode and before it gets
// the lock file's name. Tests use them to make the name appear in between and to
// cut the creation where a crash could.
var (
	beforeLockCreate  = func(string) {}
	beforeLockPublish = func(string, string) {}
)

// lockServer opens dataRoot/server.lock, creating it (mode 0660) if needed, and
// takes it exclusive without waiting.
func lockServer(dataRoot string) (*os.File, error) {
	f, err := openLock(dataRoot, ServerLockName, os.O_RDWR, serverLockMode)
	if err != nil {
		return nil, fmt.Errorf("adminlock: open %s: %w", ServerLockName, err)
	}
	ok, err := tryLock(f, true)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("adminlock: lock %s: %w", ServerLockName, err), f.Close())
	}
	if !ok {
		return nil, errors.Join(ErrServerLockHeld, f.Close())
	}
	return f, nil
}

// openLock opens the lock file name in dir with flag. An existing file is opened
// as it is and its mode is left alone. Otherwise publishLock creates it with mode,
// and the name is opened then. If the name appears between the open and the
// publication, it tries again a bounded number of times. A dangling symlink at
// the name is refused rather than followed.
func openLock(dir, name string, flag int, mode fs.FileMode) (*os.File, error) {
	path := lockPath(dir, name)
	var err error
	for range openLockAttempts {
		var f *os.File
		// #nosec G304 -- fixed lock file name inside the configured area.
		f, err = os.OpenFile(path, flag, 0)
		if !errors.Is(err, fs.ErrNotExist) {
			return f, err
		}
		beforeLockCreate(name)
		err = publishLock(dir, name, mode)
		if err == nil {
			// #nosec G304 -- fixed lock file name inside the configured area.
			return os.OpenFile(path, flag, 0)
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return nil, err
}

// publishLock creates the lock file name in dir with mode, so that the name never
// shows a file with any other mode. The umask masks a create mode (0022 turns
// 0660 into 0640, 0077 turns 0640 into 0600), which would lock a server of the
// sori group under another uid out of the file, and a later open leaves an
// existing file's mode alone. So the file is first created under a temporary
// name in dir, its mode is set on the descriptor and synced, and only then is it
// linked to the name, which fails with fs.ErrExist if the name exists in any
// form. The temporary name is removed in every case. A crash before the link
// leaves no lock file, only a temporary name, and the next process creates the
// lock file again.
func publishLock(dir, name string, mode fs.FileMode) (err error) {
	tmpDir := dir
	if tmpDir == "" {
		tmpDir = "."
	}
	tmp, err := os.CreateTemp(tmpDir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, tmp.Close(), os.Remove(tmp.Name())) }()
	// #nosec G302 -- C.5.8 lock file mode, set only on the file this process created.
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set mode %s: %w", mode, err)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	beforeLockPublish(name, tmp.Name())
	return os.Link(tmp.Name(), lockPath(dir, name))
}

// lockPath appends name to dir without cleaning dir. filepath.Join would turn
// "top/link/.." into "top", but the kernel follows link before the "..", and so
// do the database opens that the lock guards. Every spelling of a directory
// therefore names the same lock file.
func lockPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + string(filepath.Separator) + name
}

// waitLock takes f's lock, trying again every pollInterval until ctx ends. When
// logf is set it logs WAITING_ADMIN_LOCK at the first wait and every
// waitLogInterval after that.
func waitLock(ctx context.Context, f *os.File, exclusive bool, logf func(string, ...any)) error {
	start := time.Now()
	var nextLog time.Time
	for {
		ok, err := tryLock(f, exclusive)
		if err != nil {
			return fmt.Errorf("adminlock: lock %s: %w", f.Name(), err)
		}
		if ok {
			return nil
		}
		if now := time.Now(); logf != nil && !now.Before(nextLog) {
			logf("WAITING_ADMIN_LOCK: %s is held by a sori-admin command; waited %s", f.Name(), now.Sub(start).Round(time.Second))
			nextLog = now.Add(waitLogInterval)
		}
		t := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w: %w", ErrAdminBusy, ctx.Err())
		case <-t.C:
		}
	}
}
