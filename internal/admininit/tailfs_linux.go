//go:build linux

package admininit

// This file is the file-system side of the admin tail (FMT-PREP-r2 §2.2 and
// §2.2.1): the converge rule for keys/ and policy/ and the tail writer for
// keys/keyring, policy/state and epoch. Every step works on directory
// descriptors and never follows a symlink.
//
// P3-FMT7: fchmodat(adminfd, d, mode, 0) would resolve d again and follow a
// symlink put there after the identity check. Step 3 therefore opens d with
// O_PATH|O_NOFOLLOW (this needs no permission on d, so it also works on a
// directory left at mode 0000), checks its dev/ino, and changes the mode of
// exactly that inode through /proc/self/fd/<fd>. The kernel resolves that
// link to the open file, not to the current name d. Without /proc the step
// fails as I/O and nothing is changed.

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// ReasonAdminAreaInvalid refuses an admin-tail directory or entry that init
// did not create (FMT-PREP-r2 §7). It is not durable: the operator removes the
// entry and reruns.
const ReasonAdminAreaInvalid = "ADMIN_AREA_INVALID"

// tailTrace, when set by tests, sees each step of the converge rule and the
// tail writer in order.
var tailTrace func(step string)

// beforeTailChmod, when set by tests, runs after step 3 opened and checked the
// directory and before its mode is changed.
var beforeTailChmod func()

func trace(step string) {
	if tailTrace != nil {
		tailTrace(step)
	}
}

// subdirWhitelist lists the names allowed inside keys/ and policy/ (§7).
var subdirWhitelist = map[string][]string{
	KeysDirName:   {KeyringName, "." + KeyringName + ".tmp"},
	PolicyDirName: {PolicyStateName, "." + PolicyStateName + ".tmp"},
}

func invalid(format string, args ...any) error {
	return refuse(ReasonAdminAreaInvalid, format, args...)
}

// convergeSubdir runs the §2.2 converge rule for d (keys or policy) under the
// open admin directory adminfd and returns an open descriptor of d. euid is
// the init principal and gid the gid of group sori.
func convergeSubdir(adminfd int, d string, euid, gid uint32) (int, error) {
	allowed, ok := subdirWhitelist[d]
	if !ok {
		return -1, fmt.Errorf("admininit: %s is not an admin-tail directory", d)
	}
	// 1. Create; an existing entry is checked next.
	trace("mkdirat " + d)
	if err := unix.Mkdirat(adminfd, d, 0o750); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("admininit: mkdir %s: %w", d, err)
	}
	// 2. Type and owner, without following a symlink.
	var st unix.Stat_t
	trace("fstatat " + d)
	if err := unix.Fstatat(adminfd, d, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, fmt.Errorf("admininit: stat %s: %w", d, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != euid {
		return -1, invalid("%s is not a directory owned by uid %d (mode %#o, uid %d)", d, euid, st.Mode, st.Uid)
	}
	// 3. Restore owner rwx on exactly that inode (P3-FMT7).
	trace("open-path " + d)
	pfd, err := unix.Openat(adminfd, d, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, invalid("%s: open: %v", d, err)
	}
	if err := sameInode(pfd, &st); err != nil {
		_ = unix.Close(pfd)
		return -1, invalid("%s: %v", d, err)
	}
	if beforeTailChmod != nil {
		beforeTailChmod()
	}
	trace("chmod-fd " + d)
	err = unix.Fchmodat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(pfd), 0o750, 0)
	_ = unix.Close(pfd)
	if err != nil {
		return -1, fmt.Errorf("admininit: chmod %s: %w", d, err)
	}
	// 4. Open for reading and check that it is still the same directory.
	trace("openat " + d)
	dfd, err := unix.Openat(adminfd, d, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, invalid("%s: open: %v", d, err)
	}
	if err := convergeOpen(dfd, d, &st, gid, allowed); err != nil {
		_ = unix.Close(dfd)
		return -1, err
	}
	// 7. Make the directory and its entry in the admin area durable.
	trace("fsync " + d)
	if err := unix.Fsync(dfd); err != nil {
		_ = unix.Close(dfd)
		return -1, fmt.Errorf("admininit: sync %s: %w", d, err)
	}
	trace("fsync admin")
	if err := unix.Fsync(adminfd); err != nil {
		_ = unix.Close(dfd)
		return -1, fmt.Errorf("admininit: sync admin area: %w", err)
	}
	return dfd, nil
}

// convergeOpen runs steps 4 (identity), 5 (group and mode) and 6 (whitelist)
// on the open directory dfd.
func convergeOpen(dfd int, d string, st *unix.Stat_t, gid uint32, allowed []string) error {
	if err := sameInode(dfd, st); err != nil {
		return invalid("%s: %v", d, err)
	}
	trace("fchown " + d)
	if err := unix.Fchown(dfd, -1, int(gid)); err != nil {
		return fmt.Errorf("admininit: chown %s: %w", d, err)
	}
	trace("fchmod " + d)
	if err := unix.Fchmod(dfd, 0o750); err != nil {
		return fmt.Errorf("admininit: chmod %s: %w", d, err)
	}
	names, err := readDirNames(dfd)
	if err != nil {
		return fmt.Errorf("admininit: list %s: %w", d, err)
	}
	for _, n := range names {
		ok := false
		for _, a := range allowed {
			ok = ok || n == a
		}
		if !ok {
			return invalid("%s holds %q, which init does not create", d, n)
		}
		var est unix.Stat_t
		if err := unix.Fstatat(dfd, n, &est, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("admininit: stat %s/%s: %w", d, n, err)
		}
		if est.Mode&unix.S_IFMT != unix.S_IFREG {
			return invalid("%s/%s is not a regular file (mode %#o)", d, n, est.Mode)
		}
	}
	return nil
}

func sameInode(fd int, want *unix.Stat_t) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Dev != want.Dev || st.Ino != want.Ino {
		return errors.New("the entry was replaced while init checked it")
	}
	return nil
}

// readDirNames lists dfd without consuming it: it reads a duplicate.
func readDirNames(dfd int) ([]string, error) {
	nfd, err := unix.Openat(dfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(nfd), ".") // #nosec G115 -- a file descriptor is non-negative.
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// writeTailFile is the §2.2.1 tail writer: name in the directory dfd gets
// exactly b, group gid, mode 0640, through a same-directory temporary file
// that is created O_EXCL|O_NOFOLLOW, has its group and mode set and is
// fsynced before the rename, after which the directory is fsynced.
func writeTailFile(dfd int, name string, b []byte, gid uint32) error {
	tmp := "." + name + ".tmp"
	trace("unlinkat " + tmp)
	if err := unix.Unlinkat(dfd, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("admininit: write %s: remove stale temp: %w", name, err)
	}
	trace("openat " + tmp)
	fd, err := unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o640)
	if err != nil {
		return fmt.Errorf("admininit: write %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), tmp) // #nosec G115 -- a file descriptor is non-negative.
	trace("write " + tmp)
	_, err = f.Write(b)
	if err == nil {
		trace("fchown " + tmp)
		err = unix.Fchown(fd, -1, int(gid))
	}
	if err == nil {
		trace("fchmod " + tmp)
		err = unix.Fchmod(fd, 0o640)
	}
	if err == nil {
		trace("fsync " + tmp)
		err = f.Sync()
	}
	trace("close " + tmp)
	if err := errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("admininit: write %s: %w", name, err)
	}
	trace("renameat " + tmp + " " + name)
	if err := unix.Renameat(dfd, tmp, dfd, name); err != nil {
		return fmt.Errorf("admininit: write %s: %w", name, err)
	}
	trace("fsync dir")
	if err := unix.Fsync(dfd); err != nil {
		return fmt.Errorf("admininit: write %s: sync directory: %w", name, err)
	}
	return nil
}
