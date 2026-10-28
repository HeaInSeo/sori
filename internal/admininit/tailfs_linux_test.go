//go:build linux

package admininit

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func openDirFD(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// testGID returns a supplementary group of the test process that differs from
// its primary group, so that a missing fchown is visible. Without one it
// returns the primary group and says so.
func testGID(t *testing.T) uint32 {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != os.Getegid() {
			return uint32(g) // #nosec G115 -- test gid.
		}
	}
	t.Log("no supplementary group: the group change is not observable in this run")
	return uint32(os.Getegid()) // #nosec G115 -- test gid.
}

// restrictUmask sets umask 077 for the test, so that every mode the code
// relies on must be set explicitly.
func restrictUmask(t *testing.T) {
	old := unix.Umask(0o077)
	t.Cleanup(func() { unix.Umask(old) })
}

func recordTrace(t *testing.T) *[]string {
	t.Helper()
	var steps []string
	tailTrace = func(s string) { steps = append(steps, s) }
	t.Cleanup(func() { tailTrace = nil })
	return &steps
}

func statOf(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t)
}

func wantInvalid(t *testing.T, err error) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Reason != ReasonAdminAreaInvalid {
		t.Fatalf("error %v, want refusal %s", err, ReasonAdminAreaInvalid)
	}
}

// TestWriteTailFile: exact bytes, group and mode set on the temporary file,
// a stale temporary file removed first, and the §2.2.1 step order.
func TestWriteTailFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".keyring.tmp"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	dfd := openDirFD(t, dir)
	restrictUmask(t)
	steps := recordTrace(t)
	gid := testGID(t)
	if err := writeTailFile(dfd, KeyringName, []byte(vecV3), gid); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"unlinkat .keyring.tmp", "openat .keyring.tmp", "write .keyring.tmp", "fchown .keyring.tmp",
		"fchmod .keyring.tmp", "fsync .keyring.tmp", "close .keyring.tmp", "renameat .keyring.tmp keyring", "fsync dir",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps %q, want %q", *steps, want)
	}
	b, err := os.ReadFile(filepath.Join(dir, KeyringName))
	if err != nil || string(b) != vecV3 {
		t.Fatalf("content %q, %v", b, err)
	}
	st := statOf(t, filepath.Join(dir, KeyringName))
	if st.Mode&0o7777 != 0o640 || st.Gid != gid {
		t.Fatalf("mode %#o gid %d, want 0640 gid %d", st.Mode&0o7777, st.Gid, gid)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".keyring.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp left: %v", err)
	}
}

// TestConvergeSubdir: a fresh directory, one left at mode 0000 (C6b'-unreadable)
// and one at 0700 holding a stale temp (C6b'/C6b) all converge to 0750, group
// gid, and the steps run in the §2.2 order.
func TestConvergeSubdir(t *testing.T) {
	euid := uint32(os.Geteuid()) // #nosec G115 -- test uid.
	gid := testGID(t)
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, p string)
	}{
		{"fresh", func(*testing.T, string) {}},
		{"mode0000", func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"mode0700-stale-temp", func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, ".keyring.tmp"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			admin := t.TempDir()
			p := filepath.Join(admin, KeysDirName)
			c.setup(t, p)
			afd := openDirFD(t, admin)
			restrictUmask(t)
			steps := recordTrace(t)
			dfd, err := convergeSubdir(afd, KeysDirName, euid, gid)
			if err != nil {
				t.Fatal(err)
			}
			_ = unix.Close(dfd)
			want := []string{"mkdirat keys", "fstatat keys", "open-path keys", "chmod-fd keys", "openat keys",
				"fchown keys", "fchmod keys", "fsync keys", "fsync admin"}
			if !reflect.DeepEqual(*steps, want) {
				t.Fatalf("steps %q, want %q", *steps, want)
			}
			st := statOf(t, p)
			if st.Mode&0o7777 != 0o750 || st.Gid != gid {
				t.Fatalf("mode %#o gid %d, want 0750 gid %d", st.Mode&0o7777, st.Gid, gid)
			}
		})
	}
}

// TestConvergeSubdirRefusals: the §10 outside-action states are refused with
// ADMIN_AREA_INVALID.
func TestConvergeSubdirRefusals(t *testing.T) {
	euid := uint32(os.Geteuid()) // #nosec G115 -- test uid.
	gid := uint32(os.Getegid())  // #nosec G115 -- test gid.
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, admin, p string)
	}{
		{"symlink-to-dir", func(t *testing.T, admin, p string) {
			if err := os.Mkdir(filepath.Join(admin, "other"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("other", p); err != nil {
				t.Fatal(err)
			}
		}},
		{"regular-file", func(t *testing.T, _, p string) {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra-entry", func(t *testing.T, _, p string) {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "extra"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"keyring-is-directory", func(t *testing.T, _, p string) {
			if err := os.MkdirAll(filepath.Join(p, KeyringName), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			admin := t.TempDir()
			p := filepath.Join(admin, KeysDirName)
			c.setup(t, admin, p)
			afd := openDirFD(t, admin)
			dfd, err := convergeSubdir(afd, KeysDirName, euid, gid)
			if err == nil {
				_ = unix.Close(dfd)
			}
			wantInvalid(t, err)
		})
	}
	// A foreign owner is refused at step 2: present the directory as owned by
	// another uid.
	admin := t.TempDir()
	if err := os.Mkdir(filepath.Join(admin, KeysDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := convergeSubdir(openDirFD(t, admin), KeysDirName, euid+1, gid)
	wantInvalid(t, err)
}

// TestConvergeSubdirPolicy: policy/ converges with its own whitelist: a stale
// .state.tmp is allowed, a keyring name is not.
func TestConvergeSubdirPolicy(t *testing.T) {
	euid := uint32(os.Geteuid()) // #nosec G115 -- test uid.
	gid := testGID(t)
	for _, c := range []struct {
		entry string
		ok    bool
	}{{".state.tmp", true}, {KeyringName, false}} {
		admin := t.TempDir()
		p := filepath.Join(admin, PolicyDirName)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, c.entry), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		dfd, err := convergeSubdir(openDirFD(t, admin), PolicyDirName, euid, gid)
		if !c.ok {
			wantInvalid(t, err)
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.entry, err)
		}
		_ = unix.Close(dfd)
		if st := statOf(t, p); st.Mode&0o7777 != 0o750 || st.Gid != gid {
			t.Fatalf("mode %#o gid %d", st.Mode&0o7777, st.Gid)
		}
	}
}

// TestConvergeSubdirSwapDoesNotFollow (P3-FMT7): the entry is replaced by a
// symlink to another directory after the identity check and before the mode
// change. The mode change still lands on the checked inode, the symlink target
// keeps its mode, and the rule refuses at the re-check.
func TestConvergeSubdirSwapDoesNotFollow(t *testing.T) {
	euid := uint32(os.Geteuid()) // #nosec G115 -- test uid.
	gid := uint32(os.Getegid())  // #nosec G115 -- test gid.
	admin := t.TempDir()
	p := filepath.Join(admin, KeysDirName)
	moved := filepath.Join(admin, "moved")
	victim := filepath.Join(admin, "victim")
	for _, d := range []string{p, victim} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	beforeTailChmod = func() {
		if err := os.Rename(p, moved); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("victim", p); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeTailChmod = nil })
	_, err := convergeSubdir(openDirFD(t, admin), KeysDirName, euid, gid)
	wantInvalid(t, err)
	if m := statOf(t, victim).Mode & 0o7777; m != 0o700 {
		t.Fatalf("symlink target mode changed to %#o", m)
	}
	if m := statOf(t, moved).Mode & 0o7777; m != 0o750 {
		t.Fatalf("checked inode mode %#o, want 0750", m)
	}
}

// TestConvergeSubdirSwapToDirectory: the entry is replaced by another real
// directory between the check and the open. The dev/ino re-check refuses it
// and its group and mode stay unchanged.
func TestConvergeSubdirSwapToDirectory(t *testing.T) {
	euid := uint32(os.Geteuid()) // #nosec G115 -- test uid.
	gid := testGID(t)
	admin := t.TempDir()
	p := filepath.Join(admin, KeysDirName)
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	beforeTailChmod = func() {
		if err := os.Rename(p, filepath.Join(admin, "moved")); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeTailChmod = nil })
	_, err := convergeSubdir(openDirFD(t, admin), KeysDirName, euid, gid)
	wantInvalid(t, err)
	if st := statOf(t, p); st.Mode&0o7777 != 0o700 || (gid != uint32(os.Getegid()) && st.Gid == gid) { // #nosec G115 -- test gid.
		t.Fatalf("replacement directory changed: mode %#o gid %d", st.Mode&0o7777, st.Gid)
	}
}
