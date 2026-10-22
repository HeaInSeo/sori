//go:build linux

package admininit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// identitySupported reports whether this platform can read a file's birth time
// (statx) and so build the T2 identity tuple.
const identitySupported = true

// errLoop is what O_NOFOLLOW gives for a symlink at the final name.
var errLoop error = unix.ELOOP

// fsIocGetversion is FS_IOC_GETVERSION, _IOR('v', 1, long), in the asm-generic
// ioctl encoding; x/sys/unix does not define it. On an architecture with another
// encoding the ioctl fails and no generation is recorded, which the identity
// tuple allows: the birth time is the mandatory part.
const fsIocGetversion = 0x80007601 | uint(unsafe.Sizeof(uintptr(0)))<<16

// statxMask is what every identity read asks for.
const statxMask = unix.STATX_BASIC_STATS | unix.STATX_BTIME

// fileIdentity returns the identity tuple of the open file f, read from the
// descriptor (statx AT_EMPTY_PATH) and FS_IOC_GETVERSION on it. It fails with
// errNoBirthTime if the filesystem does not report a birth time.
func fileIdentity(f *os.File) (fileID, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return fileID{}, err
	}
	var (
		stx    unix.Statx_t
		gen    uint32
		genErr error
		sErr   error
	)
	err = raw.Control(func(fd uintptr) {
		sErr = unix.Statx(int(fd), "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT, statxMask, &stx) // #nosec G115 -- a descriptor fits int.
		if sErr == nil {
			gen, genErr = unix.IoctlGetUint32(int(fd), fsIocGetversion) // #nosec G115 -- a descriptor fits int.
		}
	})
	if err = errors.Join(err, sErr); err != nil {
		return fileID{}, fmt.Errorf("statx %s: %w", f.Name(), err)
	}
	id, err := fromStatx(&stx)
	if err != nil {
		return fileID{}, fmt.Errorf("%s: %w", f.Name(), err)
	}
	if genErr == nil && useGeneration {
		id.Generation = &gen
	}
	return id, nil
}

// pathIdentity returns the identity tuple of the entry at path without following
// a final symlink and without the generation, which needs a descriptor.
func pathIdentity(path string) (fileID, error) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_SYNC_AS_STAT, statxMask, &stx); err != nil {
		return fileID{}, &os.PathError{Op: "statx", Path: path, Err: err}
	}
	id, err := fromStatx(&stx)
	if err != nil {
		return fileID{}, fmt.Errorf("%s: %w", path, err)
	}
	return id, nil
}

// openIdentity opens the entry at path read-only, without following a final
// symlink or blocking on a FIFO, and returns its full identity tuple. The caller
// must check that it is a regular file.
func openIdentity(path string) (fileID, error) {
	// #nosec G304 -- a database path Init pinned inside the data root.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fileID{}, err
	}
	id, err := fileIdentity(f)
	return id, errors.Join(err, f.Close())
}

func fromStatx(stx *unix.Statx_t) (fileID, error) {
	if stx.Mask&unix.STATX_BTIME == 0 {
		return fileID{}, errNoBirthTime
	}
	return fileID{
		Dev:       unix.Mkdev(stx.Dev_major, stx.Dev_minor),
		Ino:       stx.Ino,
		BtimeSec:  stx.Btime.Sec,
		BtimeNsec: stx.Btime.Nsec,
		UID:       stx.Uid,
		GID:       stx.Gid,
		Mode:      uint32(stx.Mode),
		Nlink:     stx.Nlink,
		Size:      stx.Size,
	}, nil
}

// coarseNow reads CLOCK_REALTIME_COARSE, the clock the kernel stamps file times
// from.
func coarseNow() (stamp, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME_COARSE, &ts); err != nil {
		return stamp{}, fmt.Errorf("clock_gettime: %w", err)
	}
	return stamp{Sec: ts.Sec, Nsec: uint32(ts.Nsec)}, nil // #nosec G115 -- tv_nsec is below 1e9.
}

// maxTickWait bounds waitPast. A coarse clock ticks every few milliseconds; a
// longer wait means the clock is behind the file's birth time.
const maxTickWait = 2 * time.Second

// waitPast waits until CLOCK_REALTIME_COARSE is later than t (TRUST r4 §3.2
// step 7). A file created afterwards then gets a strictly later birth time, as
// long as the filesystem stamps times at least as finely as the clock ticks
// (P3-TR7): xfs, ext4 with large inodes and btrfs stamp nanoseconds. The G1 mode
// 2770 excludes the coarse FAT family, which cannot hold it.
func waitPast(t stamp) error {
	deadline := time.Now().Add(maxTickWait)
	for {
		now, err := coarseNow()
		if err != nil {
			return err
		}
		if onTickWait != nil {
			now = onTickWait(t, now)
		}
		if now.after(t) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: the realtime clock stayed at or before a new file's birth time %s for %s", ErrRefused, t, maxTickWait)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// setGroupMode gives the open file f the group gid and the mode perm. fchmod runs
// after the create because the umask applies to it (G4).
func setGroupMode(f *os.File, gid int, perm os.FileMode) error {
	if err := f.Chown(-1, gid); err != nil {
		return err
	}
	return f.Chmod(perm)
}

// The tags of a POSIX ACL xattr entry (linux/posix_acl_xattr.h).
const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclOther    = 0x20
	aclWrite    = 0x02
)

const (
	xattrACLAccess  = "system.posix_acl_access"
	xattrACLDefault = "system.posix_acl_default"
)

type aclEntry struct {
	tag, perm uint16
	id        uint32
}

// readACL returns the entries of the ACL xattr name of path, not following a
// final symlink, or nil if there is none or the filesystem has no ACLs.
func readACL(path, name string) ([]aclEntry, error) {
	for {
		n, err := unix.Lgetxattr(path, name, nil)
		switch {
		case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
			return nil, nil
		case err != nil:
			return nil, &os.PathError{Op: "getxattr " + name, Path: path, Err: err}
		}
		buf := make([]byte, n)
		m, err := unix.Lgetxattr(path, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue // the ACL grew between the two calls
		}
		if err != nil {
			return nil, &os.PathError{Op: "getxattr " + name, Path: path, Err: err}
		}
		return parseACL(buf[:m])
	}
}

// parseACL decodes a POSIX ACL xattr: a 4-byte version 2 header, then 8-byte
// entries of tag, permissions and id, all little-endian.
func parseACL(b []byte) ([]aclEntry, error) {
	if len(b) < 4 || binary.LittleEndian.Uint32(b) != 2 || (len(b)-4)%8 != 0 {
		return nil, errors.New("malformed POSIX ACL xattr")
	}
	var out []aclEntry
	for b = b[4:]; len(b) > 0; b = b[8:] {
		out = append(out, aclEntry{
			tag:  binary.LittleEndian.Uint16(b),
			perm: binary.LittleEndian.Uint16(b[2:]),
			id:   binary.LittleEndian.Uint32(b[4:]),
		})
	}
	return out, nil
}

// extendedACL reports whether entries hold more than the three base entries,
// which the mode bits already show.
func extendedACL(entries []aclEntry) bool {
	for _, e := range entries {
		if e.tag != aclUserObj && e.tag != aclGroupObj && e.tag != aclOther {
			return true
		}
	}
	return false
}

// writeACL reports whether entries grant write to a named user or group.
func writeACL(entries []aclEntry) bool {
	for _, e := range entries {
		if (e.tag == aclUser || e.tag == aclGroup) && e.perm&aclWrite != 0 {
			return true
		}
	}
	return false
}

// checkTrustedDir checks one directory of the data root against G1: owner the
// init principal, group sori, mode exactly 2770 (setgid, no sticky), no ACL
// entries beyond the base three and no default ACL.
func checkTrustedDir(dir string, tr trust) error {
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return &os.PathError{Op: "lstat", Path: dir, Err: err}
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return fmt.Errorf("%s is not a directory", dir)
	case st.Uid != tr.uid:
		return fmt.Errorf("%s is owned by uid %d, want the init principal %d", dir, st.Uid, tr.uid)
	case st.Gid != tr.gid:
		return fmt.Errorf("%s has group %d, want sori (%d)", dir, st.Gid, tr.gid)
	case st.Mode&0o7777 != 0o2770:
		return fmt.Errorf("%s has mode %04o, want exactly 2770", dir, st.Mode&0o7777)
	}
	acc, err := readACL(dir, xattrACLAccess)
	if err != nil {
		return err
	}
	if extendedACL(acc) {
		return fmt.Errorf("%s has ACL entries beyond owner, group and other", dir)
	}
	def, err := readACL(dir, xattrACLDefault)
	if err != nil {
		return err
	}
	if len(def) != 0 {
		return fmt.Errorf("%s has a default ACL", dir)
	}
	return nil
}

// checkAncestors checks every directory above realRoot up to "/": owner root, no
// group or other write and no ACL entry that grants write (G1). A test may stop
// the walk below ancestorStop.
func checkAncestors(realRoot string) error {
	for dir := filepath.Dir(realRoot); dir != ancestorStop; dir = filepath.Dir(dir) {
		var st unix.Stat_t
		if err := unix.Lstat(dir, &st); err != nil {
			return &os.PathError{Op: "lstat", Path: dir, Err: err}
		}
		switch {
		case st.Uid != ancestorOwner:
			return fmt.Errorf("ancestor %s is owned by uid %d, want %d", dir, st.Uid, ancestorOwner)
		case st.Mode&0o022 != 0:
			return fmt.Errorf("ancestor %s has mode %04o: group or other can write it", dir, st.Mode&0o7777)
		}
		acc, err := readACL(dir, xattrACLAccess)
		if err != nil {
			return err
		}
		if writeACL(acc) {
			return fmt.Errorf("ancestor %s has an ACL entry that grants write", dir)
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
	return nil
}

// fileACL refuses a database file with ACL entries beyond the base three (G2).
func fileACL(path string) error {
	acc, err := readACL(path, xattrACLAccess)
	if err != nil {
		return err
	}
	if extendedACL(acc) {
		return fmt.Errorf("%s has ACL entries beyond owner, group and other", path)
	}
	return nil
}

// retryableIO reports whether err comes from an I/O condition a plain rerun may
// clear (TRUST r4 §3.2 retryable list), so a step-8 refusal is not made durable.
func retryableIO(err error) bool {
	var errno unix.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case unix.EIO, unix.ENOSPC, unix.EDQUOT, unix.EAGAIN, unix.EINTR,
		unix.ENOMEM, unix.EMFILE, unix.ENFILE, unix.EROFS:
		return true
	}
	return false
}

// syncDir fsyncs the directory dir.
func syncDir(dir string) error {
	// #nosec G304 -- the admin area or the data root.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// createExcl creates path with O_CREAT|O_EXCL|O_NOFOLLOW, mode 0600 before
// fchmod.
func createExcl(path string) (*os.File, error) {
	// #nosec G304 -- a database path Init pinned inside the data root.
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
}
