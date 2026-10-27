//go:build linux

package admininit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/internal/adminlock/locktest"
)

// tailState is every name below the admin area with its mode, group and
// bytes, except admin.lock, so a test can show that nothing was written.
func tailState(t *testing.T, adminDir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(adminDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(adminDir, path)
		st, _ := fi.Sys().(*syscall.Stat_t)
		fmt.Fprintf(&b, "%s %s gid=%d", rel, fi.Mode(), st.Gid)
		if fi.Mode().IsRegular() && rel != adminlock.AdminLockName {
			data, err := os.ReadFile(path) // #nosec G304 -- test admin area.
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %x", sha256.Sum256(data))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// checkComplete asserts the end state of a complete init for installation id:
// FMT-PREP-r2 §2.1 owners and modes, the three records valid under the strict
// readers, the deny-all default document for id, and host_id as given.
func checkComplete(t *testing.T, adminDir, id string) admininit.Keyring {
	t.Helper()
	uid, gid := testIDs()
	for _, c := range []struct {
		rel  string
		mode os.FileMode
	}{
		{admininit.KeysDirName, os.ModeDir | 0o750},
		{admininit.PolicyDirName, os.ModeDir | 0o750},
		{filepath.Join(admininit.KeysDirName, admininit.KeyringName), 0o640},
		{filepath.Join(admininit.PolicyDirName, admininit.PolicyStateName), 0o640},
		{admininit.EpochName, 0o640},
	} {
		fi, err := os.Lstat(filepath.Join(adminDir, c.rel))
		if err != nil {
			t.Fatal(err)
		}
		st, _ := fi.Sys().(*syscall.Stat_t)
		if fi.Mode() != c.mode || st.Uid != uid || st.Gid != gid {
			t.Fatalf("%s: mode %s uid %d gid %d, want %s uid %d gid %d", c.rel, fi.Mode(), st.Uid, st.Gid, c.mode, uid, gid)
		}
	}
	for dir, want := range map[string][]string{
		admininit.KeysDirName:   {admininit.KeyringName},
		admininit.PolicyDirName: {admininit.PolicyStateName},
	} {
		if got := names(t, filepath.Join(adminDir, dir)); !slices.Equal(got, want) {
			t.Fatalf("%s holds %v, want %v", dir, got, want)
		}
	}
	k, ps, e, err := admininit.ReadTail(adminDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Keys) != 1 || k.Keys[0].KeyID != k.CurrentKeyID || k.Keys[0].State != "ACTIVE" || len(k.Keys[0].Secret) != 32 {
		t.Fatalf("keyring %+v", k)
	}
	doc := `{"credentials":[],"grants":[],"installation_id":"` + id + `","schema_version":1}`
	sum := sha256.Sum256([]byte(doc))
	if string(ps.Document) != doc || ps.Digest != "sha256:"+hex.EncodeToString(sum[:]) || ps.PolicyRevision != 1 ||
		ps.HighWater != 1 || ps.RevocationBasis != "none" || ps.Owner != "unix-uid:"+strconv.Itoa(int(uid)) {
		t.Fatalf("policy/state %+v", ps)
	}
	if e.InstallationID != id || e.ActivationEpoch != 1 || e.CutCounter != 0 || e.HostID != testHostID {
		t.Fatalf("epoch %+v, want installation %s, epoch 1, cut 0, host %s", e, id, testHostID)
	}
	return k
}

// TestInitWritesAdminTail: a first init writes keyring, deny-all policy/state
// and epoch for the installation id (FMT-PREP-r2 §3–§6), and Status reads them
// back without the key secret.
func TestInitWritesAdminTail(t *testing.T) {
	adminDir, root, p := layout(t)
	ctx := context.Background()
	if _, err := admininit.ReadStatus(ctx, adminDir); err == nil {
		t.Fatal("status before init succeeded")
	}
	res, err := admininit.Init(ctx, adminDir, root, testHostID, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	k := checkComplete(t, adminDir, res.InstallationID)
	before := tailState(t, adminDir)
	st, err := admininit.ReadStatus(ctx, adminDir)
	if err != nil {
		t.Fatal(err)
	}
	want := admininit.Status{
		InstallationID: res.InstallationID, ActivationEpoch: 1, CutCounter: 0, HostID: testHostID,
		CurrentKeyID: k.CurrentKeyID, Keys: []admininit.StatusKey{{KeyID: k.CurrentKeyID, State: "ACTIVE"}},
	}
	if fmt.Sprintf("%+v", st) != fmt.Sprintf("%+v", want) {
		t.Fatalf("status %+v, want %+v", st, want)
	}
	if after := tailState(t, adminDir); after != before {
		t.Fatalf("status changed the admin area:\n%s\nwant\n%s", after, before)
	}
}

// TestReadStatusNotInitialized: with epoch absent Status reports so and writes
// nothing.
func TestReadStatusNotInitialized(t *testing.T) {
	adminDir, _, _ := layout(t)
	if _, err := admininit.ReadStatus(context.Background(), adminDir); err == nil {
		t.Fatal("status without admin.lock succeeded")
	}
	f, err := os.OpenFile(filepath.Join(adminDir, adminlock.AdminLockName), os.O_CREATE|os.O_RDONLY, 0o640) // #nosec G304 -- test admin area.
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before := tailState(t, adminDir)
	if _, err := admininit.ReadStatus(context.Background(), adminDir); !errors.Is(err, admininit.ErrNotInitialized) {
		t.Fatalf("status = %v, want ErrNotInitialized", err)
	}
	if after := tailState(t, adminDir); after != before {
		t.Fatal("status wrote")
	}
}

// TestInitHostIDRequired: server.host_id is required and checked against the
// §8.2 grammar at step 4, before init-intent; nothing is recorded. Steps 2 and
// 3 keep their precedence.
func TestInitHostIDRequired(t *testing.T) {
	for _, h := range []string{"", strings.Repeat("a", 65), "h 1", "h/1", "é", "h1\n"} {
		t.Run(strconv.Quote(h), func(t *testing.T) {
			adminDir, root, p := layout(t)
			_, err := admininit.Init(context.Background(), adminDir, root, h, p, noID(t))
			wantRefusal(t, err, admininit.ReasonHostIDInvalid, false)
			checkNothingRecorded(t, adminDir, root)
			if got := names(t, adminDir); !slices.Equal(got, []string{adminlock.AdminLockName}) {
				t.Fatalf("admin area = %v", got)
			}
		})
	}
	for _, h := range []string{"h1", "node-01.lab", "A~b_c", strings.Repeat("z", 64)} {
		t.Run("ok "+h, func(t *testing.T) {
			adminDir, root, p := layout(t)
			if _, err := admininit.Init(context.Background(), adminDir, root, h, p, nil); err != nil {
				t.Fatal(err)
			}
			_, _, e, err := admininit.ReadTail(adminDir)
			if err != nil || e.HostID != h {
				t.Fatalf("epoch host %q, %v; want %q", e.HostID, err, h)
			}
		})
	}
	t.Run("epoch first", func(t *testing.T) {
		adminDir, root, p := layout(t)
		if err := os.WriteFile(filepath.Join(adminDir, admininit.EpochName), nil, 0o640); err != nil {
			t.Fatal(err)
		}
		_, err := admininit.Init(context.Background(), adminDir, root, "", p, noID(t))
		wantRefusal(t, err, admininit.ReasonAlreadyComplete, false)
	})
	t.Run("refused first", func(t *testing.T) {
		adminDir, root, p := layout(t)
		if err := os.WriteFile(filepath.Join(adminDir, admininit.RefusedName), nil, 0o640); err != nil {
			t.Fatal(err)
		}
		_, err := admininit.Init(context.Background(), adminDir, root, "", p, noID(t))
		wantRefusal(t, err, admininit.ReasonRefusedStands, false)
	})
}

// TestInitAdminAreaPreflight: the step-4 admin-area checks of FMT-PREP-r2 §7
// refuse before init-intent and record nothing.
func TestInitAdminAreaPreflight(t *testing.T) {
	cases := map[string]struct {
		put    func(t *testing.T, adminDir string)
		reason string
	}{
		"unknown file": {func(t *testing.T, d string) {
			writeEmpty(t, filepath.Join(d, "imported_from"))
		}, admininit.ReasonAdminAreaNotEmpty},
		"unknown directory": {func(t *testing.T, d string) {
			mkdir(t, filepath.Join(d, "activations"))
		}, admininit.ReasonAdminAreaNotEmpty},
		"keyring at top level": {func(t *testing.T, d string) {
			writeEmpty(t, filepath.Join(d, admininit.KeyringName))
		}, admininit.ReasonAdminAreaNotEmpty},
		"keys regular file": {func(t *testing.T, d string) {
			writeEmpty(t, filepath.Join(d, admininit.KeysDirName))
		}, admininit.ReasonAdminAreaInvalid},
		"keys symlink to directory": {func(t *testing.T, d string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(d, admininit.KeysDirName)); err != nil {
				t.Fatal(err)
			}
		}, admininit.ReasonAdminAreaInvalid},
		"policy regular file": {func(t *testing.T, d string) {
			writeEmpty(t, filepath.Join(d, admininit.PolicyDirName))
		}, admininit.ReasonAdminAreaInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			c.put(t, adminDir)
			_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, noID(t))
			wantRefusal(t, err, c.reason, false)
			checkNothingRecorded(t, adminDir, root)
		})
	}
	// Whitelisted leftovers are accepted.
	adminDir, root, p := layout(t)
	for _, n := range []string{".init-intent.tmp", ".init-refused.tmp", ".epoch.tmp", ".admin.lock.tmp-123"} {
		writeEmpty(t, filepath.Join(adminDir, n))
	}
	mkdir(t, filepath.Join(adminDir, admininit.KeysDirName))
	if _, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, nil); err != nil {
		t.Fatal(err)
	}
}

func writeEmpty(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
}

// TestInitPreEpochCheck: a tail record found invalid on the §5.2 re-read
// refuses ADMIN_TAIL_INVALID, writes no epoch and no init-refused, and a plain
// rerun completes with the same installation id.
func TestInitPreEpochCheck(t *testing.T) {
	for name, corrupt := range map[string]func(t *testing.T, adminDir string){
		"policy state": func(t *testing.T, d string) {
			writeEmpty(t, filepath.Join(d, admininit.PolicyDirName, admininit.PolicyStateName))
		},
		"keyring removed": func(t *testing.T, d string) {
			if err := os.Remove(filepath.Join(d, admininit.KeysDirName, admininit.KeyringName)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			restore := admininit.SetAfterStep(func(s admininit.Step) error {
				if s == admininit.StepPolicyState {
					corrupt(t, adminDir)
				}
				return nil
			})
			_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, nil)
			restore()
			wantRefusal(t, err, admininit.ReasonAdminTailInvalid, false)
			for _, n := range []string{admininit.EpochName, admininit.RefusedName} {
				if exists(t, filepath.Join(adminDir, n)) {
					t.Fatalf("%s written", n)
				}
			}
			id := readIntent(t, adminDir).InstallationID
			res, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, noID(t))
			if err != nil || res.InstallationID != id {
				t.Fatalf("rerun = %+v, %v; want %s", res, err, id)
			}
			checkComplete(t, adminDir, id)
		})
	}
	// I in init-intent differs from the I init wrote into policy/state.
	t.Run("intent installation id", func(t *testing.T) {
		adminDir, root, p := layout(t)
		path := filepath.Join(adminDir, admininit.IntentName)
		restore := admininit.SetAfterStep(func(s admininit.Step) error {
			if s != admininit.StepPolicyState {
				return nil
			}
			b, err := os.ReadFile(path) // #nosec G304 -- test admin area.
			if err != nil {
				t.Fatal(err)
			}
			id := readIntent(t, adminDir).InstallationID
			other := strings.Repeat("f", 32)
			if err := os.WriteFile(path, []byte(strings.Replace(string(b), `"`+id+`"`, `"`+other+`"`, 1)), 0o640); err != nil {
				t.Fatal(err)
			}
			return nil
		})
		_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, nil)
		restore()
		wantRefusal(t, err, admininit.ReasonAdminTailInvalid, false)
		for _, n := range []string{admininit.EpochName, admininit.RefusedName} {
			if exists(t, filepath.Join(adminDir, n)) {
				t.Fatalf("%s written", n)
			}
		}
	})
}

// TestReadStatusTakesAdminLockShared: Status waits for admin.lock shared, so
// it does not read while another process holds the L3 locks.
func TestReadStatusTakesAdminLockShared(t *testing.T) {
	adminDir, root, p := layout(t)
	if _, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, nil); err != nil {
		t.Fatal(err)
	}
	h := locktest.Hold(t, locktest.Offline, adminDir, root)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := admininit.ReadStatus(ctx, adminDir); err == nil {
		t.Fatal("status read while another process held admin.lock exclusive")
	}
	h.Release(t)
	if _, err := admininit.ReadStatus(context.Background(), adminDir); err != nil {
		t.Fatal(err)
	}
}

// TestInitLoserWritesNoTail: two processes. While another process holds the
// L3 locks, or a server holds server.lock, init writes no admin-tail file.
func TestInitLoserWritesNoTail(t *testing.T) {
	t.Run("server", func(t *testing.T) {
		adminDir, root, p := layout(t)
		h := locktest.Hold(t, locktest.Server, adminDir, root)
		_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, noID(t))
		if !errors.Is(err, adminlock.ErrServerRunning) {
			t.Fatalf("init = %v, want ErrServerRunning", err)
		}
		h.Release(t)
		if got := names(t, adminDir); !slices.Equal(got, []string{adminlock.AdminLockName}) {
			t.Fatalf("admin area = %v, want only admin.lock", got)
		}
	})
	t.Run("offline", func(t *testing.T) {
		adminDir, root, p := layout(t)
		h := locktest.Hold(t, locktest.Offline, adminDir, root)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if _, err := admininit.Init(ctx, adminDir, root, testHostID, p, noID(t)); err == nil {
			t.Fatal("init succeeded while another process held the locks")
		}
		h.Release(t)
		if got := names(t, adminDir); !slices.Equal(got, []string{adminlock.AdminLockName}) {
			t.Fatalf("admin area = %v, want only admin.lock", got)
		}
	})
}

// Crash cuts. The parent test re-executes the test binary; the child runs
// Init and sends itself SIGKILL at the cut, so the kernel ends the process
// with no deferred function, no close and no further system call of Init.
const (
	crashAdminEnv = "SORI_CRASHTEST_ADMIN"
	crashRootEnv  = "SORI_CRASHTEST_ROOT"
	crashAtEnv    = "SORI_CRASHTEST_AT"    // "step:<Step>" or "trace:<step>#<n>"
	crashUmaskEnv = "SORI_CRASHTEST_UMASK" // "<trace step>=<octal umask>"
)

func init() { crashChild = runCrashChild }

func runCrashChild() {
	adminDir := os.Getenv(crashAdminEnv)
	if adminDir == "" {
		return
	}
	root, at := os.Getenv(crashRootEnv), os.Getenv(crashAtEnv)
	kill := func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		for {
			time.Sleep(time.Hour)
		}
	}
	umaskAt, umaskVal, _ := strings.Cut(os.Getenv(crashUmaskEnv), "=")
	seen := map[string]int{}
	admininit.SetTailTrace(func(s string) {
		if s == umaskAt {
			m, _ := strconv.ParseUint(umaskVal, 8, 32)
			syscall.Umask(int(m))
		}
		seen[s]++
		if at == "trace:"+s+"#"+strconv.Itoa(seen[s]) {
			kill()
		}
	})
	admininit.SetAfterStep(func(s admininit.Step) error {
		if at == "step:"+string(s) {
			kill()
		}
		return nil
	})
	p := admininit.Paths{JobLedger: filepath.Join(root, "jobs.db"), Authority: filepath.Join(root, "authority.db")}
	_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, nil)
	fmt.Println("NOT KILLED", err)
	os.Exit(4)
}

// killAt runs init in a child process killed by SIGKILL at the cut.
func killAt(t *testing.T, adminDir, root, at, umask string) {
	t.Helper()
	// #nosec G204 G702 -- re-executes the running test binary itself.
	cmd := osexec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), crashAdminEnv+"="+adminDir, crashRootEnv+"="+root, crashAtEnv+"="+at, crashUmaskEnv+"="+umask)
	out, err := cmd.CombinedOutput()
	var ee *osexec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("child at %s: %v: %s", at, err, out)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("child at %s ended %v, want SIGKILL: %s", at, err, out)
	}
}

// TestInitTailCrashCuts: FMT-PREP-r2 §6 C6a–C6e and C6b′ with a real process
// killed at the cut. The state left is checked, then a plain rerun completes
// with the same installation id, replaces any keyring of the killed attempt
// and leaves the §2.1 owners and modes. A process kill keeps the page cache,
// so these cuts do not exercise lost writes of a power loss.
func TestInitTailCrashCuts(t *testing.T) {
	cuts := []struct {
		name, at, umask string
		left            func(t *testing.T, adminDir string)
	}{
		{"C6a", "step:" + string(admininit.StepRecordID), "", func(t *testing.T, d string) {
			wantNames(t, d, adminlock.AdminLockName, admininit.IntentName)
		}},
		{"C6b'-keys", "trace:fstatat keys#1", "mkdirat keys=077", func(t *testing.T, d string) {
			wantDirMode(t, filepath.Join(d, admininit.KeysDirName), 0o700)
		}},
		{"C6b'-keys-0000", "trace:fstatat keys#1", "mkdirat keys=777", func(t *testing.T, d string) {
			wantDirMode(t, filepath.Join(d, admininit.KeysDirName), 0)
		}},
		{"C6b'-policy", "trace:fstatat policy#1", "mkdirat policy=077", func(t *testing.T, d string) {
			wantDirMode(t, filepath.Join(d, admininit.PolicyDirName), 0o700)
			if !exists(t, filepath.Join(d, admininit.KeysDirName, admininit.KeyringName)) {
				t.Fatal("no keyring before the policy cut")
			}
		}},
		{"C6b-keyring-temp", "trace:fchown .keyring.tmp#1", "", func(t *testing.T, d string) {
			wantNames(t, filepath.Join(d, admininit.KeysDirName), ".keyring.tmp")
		}},
		{"C6b-state-temp", "trace:renameat .state.tmp state#1", "", func(t *testing.T, d string) {
			wantNames(t, filepath.Join(d, admininit.PolicyDirName), ".state.tmp")
		}},
		{"C6c", "step:" + string(admininit.StepKeyring), "", func(t *testing.T, d string) {
			wantNames(t, d, adminlock.AdminLockName, admininit.IntentName, admininit.KeysDirName)
		}},
		{"C6d", "step:" + string(admininit.StepPolicyState), "", func(t *testing.T, d string) {
			wantNames(t, d, adminlock.AdminLockName, admininit.IntentName, admininit.KeysDirName, admininit.PolicyDirName)
		}},
		{"C6e", "trace:renameat .epoch.tmp epoch#1", "", func(t *testing.T, d string) {
			wantNames(t, d, ".epoch.tmp", adminlock.AdminLockName, admininit.IntentName, admininit.KeysDirName, admininit.PolicyDirName)
		}},
	}
	for _, c := range cuts {
		t.Run(c.name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			killAt(t, adminDir, root, c.at, c.umask)
			c.left(t, adminDir)
			if exists(t, filepath.Join(adminDir, admininit.EpochName)) {
				t.Fatal("epoch present after a pre-epoch cut")
			}
			id := readIntent(t, adminDir).InstallationID
			if id == "" {
				t.Fatal("no installation id recorded before step 10")
			}
			// The keyring of the killed attempt, if it got that far.
			old, _, _, _ := admininit.ReadTail(adminDir)
			oldKey := old.CurrentKeyID
			res, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, noID(t))
			if err != nil || res.InstallationID != id || res.Bootstrapped {
				t.Fatalf("rerun = %+v, %v; want reuse of %s", res, err, id)
			}
			k := checkComplete(t, adminDir, id)
			if oldKey != "" && k.CurrentKeyID == oldKey {
				t.Fatalf("rerun kept the abandoned key %s", oldKey)
			}
			wantNames(t, adminDir, adminlock.AdminLockName, admininit.EpochName, admininit.IntentName, admininit.KeysDirName, admininit.PolicyDirName)
		})
	}
}

// TestInitCrashAfterEpoch: C6f (epoch renamed, process killed before the
// directory fsync) and C6g (complete, killed before Init returned: the ACK is
// lost). Epoch is present, so a rerun refuses INIT_ALREADY_COMPLETE with
// nothing written, and Status reports the existing installation, host, epoch
// and key ids; init never reissues them.
func TestInitCrashAfterEpoch(t *testing.T) {
	for name, at := range map[string]string{
		"C6f": "trace:fsync dir#3",
		"C6g": "step:" + string(admininit.StepEpoch),
	} {
		t.Run(name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			killAt(t, adminDir, root, at, "")
			id := readIntent(t, adminDir).InstallationID
			k := checkComplete(t, adminDir, id)
			before, rootBefore := tailState(t, adminDir), listing(t, root)
			_, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, noID(t))
			wantRefusal(t, err, admininit.ReasonAlreadyComplete, false)
			if after := tailState(t, adminDir); after != before {
				t.Fatalf("rerun wrote the admin area:\n%s\nwant\n%s", after, before)
			}
			if got := listing(t, root); !slices.Equal(got, rootBefore) {
				t.Fatalf("rerun changed the data root: %v, want %v", got, rootBefore)
			}
			st, err := admininit.ReadStatus(context.Background(), adminDir)
			if err != nil || st.InstallationID != id || st.HostID != testHostID || st.ActivationEpoch != 1 ||
				st.CutCounter != 0 || st.CurrentKeyID != k.CurrentKeyID {
				t.Fatalf("status %+v, %v", st, err)
			}
		})
	}
}

func wantNames(t *testing.T, dir string, want ...string) {
	t.Helper()
	if got := names(t, dir); !slices.Equal(got, want) {
		t.Fatalf("%s holds %v, want %v", dir, got, want)
	}
}

func wantDirMode(t *testing.T, dir string, perm os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != perm {
		t.Fatalf("%s: %s, want a directory with mode %o", dir, fi.Mode(), perm)
	}
	if names := mustList(t, dir); len(names) != 0 {
		t.Fatalf("%s holds %v, want empty", dir, names)
	}
}

func mustList(t *testing.T, dir string) []string {
	t.Helper()
	f, err := os.Open(dir) // #nosec G304 -- test admin area.
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil
		}
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	n, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
