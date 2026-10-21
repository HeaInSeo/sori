//go:build linux

package admininit_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HeaInSeo/sori/authority/sqlitestore"
	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/jobledger"
)

// The tests in this file cover the data-root trust steps of SORI-D01-TRUST-r4
// that Init runs under its locks: I-T1 (G1 preflight), I-T2 (init-intent,
// identity tuple, durable refusal, crash cuts) and I-G4 (0660 group hand-over).
// A second uid is not available here; the owner and group checks are exercised
// by changing the expected ids (SetTestTrust) instead.

var errCrash = errors.New("crash")

func testIDs() (uid, gid uint32) {
	return uint32(os.Geteuid()), uint32(os.Getegid()) // #nosec G115 -- ids fit 32 bits.
}

func refusal(t *testing.T, err error) *admininit.Refusal {
	t.Helper()
	var r *admininit.Refusal
	if !errors.As(err, &r) || !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want an admininit.Refusal", err)
	}
	return r
}

func wantRefusal(t *testing.T, err error, reason string, durable bool) {
	t.Helper()
	r := refusal(t, err)
	if r.Reason != reason || r.Durable != durable {
		t.Fatalf("refusal = %s (durable %v): %v; want %s (durable %v)", r.Reason, r.Durable, r.Err, reason, durable)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// listing lists the names, modes and sizes in dir, except server.lock, which
// every run creates when it takes the locks.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range names(t, dir) {
		if n == adminlock.ServerLockName {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %d", n, fi.Mode(), fi.Size()))
	}
	return out
}

// checkRefusedStands asserts that a durable refusal is recorded and that a
// plain rerun refuses with INIT_REFUSED_STANDS and changes nothing in the root.
func checkRefusedStands(t *testing.T, adminDir, root string, p admininit.Paths) {
	t.Helper()
	if !exists(t, filepath.Join(adminDir, admininit.RefusedName)) {
		t.Fatal("init-refused not written")
	}
	before := listing(t, root)
	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	wantRefusal(t, err, admininit.ReasonRefusedStands, false)
	if after := listing(t, root); !slices.Equal(before, after) {
		t.Fatalf("rerun changed the data root: %v -> %v", before, after)
	}
}

// checkNothingRecorded asserts a retryable refusal: no init-intent, no
// init-refused, and only server.lock in the data root.
func checkNothingRecorded(t *testing.T, adminDir, root string) {
	t.Helper()
	for _, n := range []string{admininit.IntentName, admininit.RefusedName} {
		if exists(t, filepath.Join(adminDir, n)) {
			t.Fatalf("%s written", n)
		}
	}
	if got := names(t, root); !slices.Equal(got, []string{adminlock.ServerLockName}) {
		t.Fatalf("data root = %v, want only server.lock", got)
	}
}

type testStamp struct {
	Sec  int64  `json:"sec"`
	Nsec uint32 `json:"nsec"`
}

func (s testStamp) before(o testStamp) bool {
	return s.Sec < o.Sec || (s.Sec == o.Sec && s.Nsec < o.Nsec)
}

type testDB struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	Ino        uint64  `json:"ino"`
	BtimeSec   int64   `json:"btime_sec"`
	BtimeNsec  uint32  `json:"btime_nsec"`
	Generation *uint32 `json:"generation"`
	UID        uint32  `json:"uid"`
	GID        uint32  `json:"gid"`
	Mode       uint32  `json:"mode"`
}

func (d testDB) btime() testStamp { return testStamp{Sec: d.BtimeSec, Nsec: d.BtimeNsec} }

type testIntent struct {
	Version   int       `json:"version"`
	Nonce     string    `json:"intent_nonce"`
	CreatedAt testStamp `json:"intent_created_at"`
	DataRoot  struct {
		Path string `json:"path"`
	} `json:"data_root"`
	Databases      []testDB `json:"databases"`
	InstallationID string   `json:"installation_id"`
}

func (in testIntent) db(t *testing.T, name string) testDB {
	t.Helper()
	for _, d := range in.Databases {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("intent has no %s record: %+v", name, in.Databases)
	return testDB{}
}

func readIntent(t *testing.T, adminDir string) testIntent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(adminDir, admininit.IntentName))
	if err != nil {
		t.Fatal(err)
	}
	var in testIntent
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// editIntent rewrites one numeric field of the named database's record in
// init-intent.
func editIntent(t *testing.T, adminDir, db, field string, edit func(int64) int64) {
	t.Helper()
	path := filepath.Join(adminDir, admininit.IntentName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, d := range m["databases"].([]any) {
		rec := d.(map[string]any)
		if rec["name"] == db {
			v, err := rec[field].(json.Number).Int64()
			if err != nil {
				t.Fatal(err)
			}
			rec[field] = edit(v)
		}
	}
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

func crashAt(at admininit.Step) (restore func()) {
	return admininit.SetAfterStep(func(s admininit.Step) error {
		if s == at {
			return errCrash
		}
		return nil
	})
}

func crashInit(t *testing.T, adminDir, root string, p admininit.Paths, at admininit.Step) {
	t.Helper()
	restore := crashAt(at)
	_, err := admininit.Init(context.Background(), adminDir, root, p, nil)
	restore()
	if !errors.Is(err, errCrash) {
		t.Fatalf("init stopped at %s: %v, want the crash", at, err)
	}
}

// checkHandOver asserts G4: both databases and every sidecar are regular files,
// group gid, mode 0660.
func checkHandOver(t *testing.T, p admininit.Paths, gid uint32) {
	t.Helper()
	for _, db := range []string{p.Authority, p.JobLedger} {
		for _, name := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
			var st unix.Stat_t
			err := unix.Lstat(name, &st)
			if errors.Is(err, unix.ENOENT) && name != db {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Gid != gid || st.Mode&0o7777 != 0o660 {
				t.Fatalf("%s: mode %o gid %d; want a regular file 0660 group %d", name, st.Mode, st.Gid, gid)
			}
		}
	}
}

// A first init records the data root and both databases with a birth time at
// or after the intent's reference time, the installation id, and leaves both
// databases and their sidecars 0660 group sori even under umask 077 (I-G4).
// A server-side open and close of both stores afterwards (same uid; a second
// uid is NOT RUN here) leaves an init rerun working with the same id.
func TestInitRecordsIntentAndHandsOver(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	_, gid := testIDs()
	adminDir, root, p := layout(t)
	res, err := admininit.Init(context.Background(), adminDir, root, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := readIntent(t, adminDir)
	if in.Version != 1 || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(in.Nonce) || in.DataRoot.Path != root || in.InstallationID != res.InstallationID {
		t.Fatalf("intent = %+v, result %+v", in, res)
	}
	if len(in.Databases) != 2 || in.Databases[0].Name != "authority" || in.Databases[1].Name != "jobledger" {
		t.Fatalf("intent databases = %+v, want authority then jobledger", in.Databases)
	}
	for _, d := range in.Databases {
		if d.BtimeSec == 0 || d.Mode&0o7777 != 0o660 || d.GID != gid || d.btime().before(in.CreatedAt) {
			t.Fatalf("record %+v, intent created %+v", d, in.CreatedAt)
		}
	}
	if fi, err := os.Stat(filepath.Join(adminDir, admininit.IntentName)); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("init-intent: %v, %v; want mode 0640", fi, err)
	}
	checkHandOver(t, p, gid)

	// G4 reopen: the stores open as the server would, and init still resumes.
	ledger, err := jobledger.Open(p.JobLedger)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := sqlitestore.Open(p.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(ledger.Close(), auth.Close()); err != nil {
		t.Fatal(err)
	}
	again, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	if err != nil || again.InstallationID != res.InstallationID || again.Bootstrapped {
		t.Fatalf("rerun = %+v, %v; want reuse of %q", again, err, res.InstallationID)
	}
	checkHandOver(t, p, gid)
}

// setACL writes a POSIX ACL xattr with the given extra entries (tag, perm, id)
// after the base entries, skipping where the filesystem has no ACLs.
func setACL(t *testing.T, path, name string, mode uint16, extra ...[3]uint32) {
	t.Helper()
	b := binary.LittleEndian.AppendUint32(nil, 2)
	add := func(tag, perm uint16, id uint32) {
		b = binary.LittleEndian.AppendUint16(b, tag)
		b = binary.LittleEndian.AppendUint16(b, perm)
		b = binary.LittleEndian.AppendUint32(b, id)
	}
	add(0x01, mode>>6&7, 0xffffffff)
	for _, e := range extra {
		if e[0] == 0x02 {
			add(uint16(e[0]), uint16(e[1]), e[2]) // #nosec G115 -- small test values.
		}
	}
	add(0x04, mode>>3&7, 0xffffffff)
	for _, e := range extra {
		if e[0] == 0x08 {
			add(uint16(e[0]), uint16(e[1]), e[2]) // #nosec G115 -- small test values.
		}
	}
	if len(extra) > 0 {
		// The mask is the group class of the mode, so the mode bits stay as
		// they are and only the ACL entry differs.
		add(0x10, mode>>3&7, 0xffffffff)
	}
	add(0x20, mode&7, 0xffffffff)
	if err := unix.Lsetxattr(path, name, b, 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("no ACLs here: %v", err)
		}
		t.Fatalf("setxattr %s: %v", name, err)
	}
}

// I-T1: a data root that is not exactly sori-admin:sori 2770 without sticky and
// without extended or default ACLs is refused before anything is recorded, and
// the refusal is retryable.
func TestInitRefusesUntrustedDataRoot(t *testing.T) {
	uid, gid := testIDs()
	tmp, err := filepath.EvalSymlinks(testTempBase())
	if err != nil {
		t.Fatal(err)
	}
	chmod := func(mode os.FileMode) func(*testing.T, string, admininit.Paths) {
		return func(t *testing.T, root string, _ admininit.Paths) {
			if err := os.Chmod(root, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := map[string]func(t *testing.T, root string, p admininit.Paths){
		"no setgid 0770":   chmod(0o770),
		"sticky 3770":      chmod(0o770 | os.ModeSetgid | os.ModeSticky),
		"other write 2777": chmod(0o777 | os.ModeSetgid),
		"group read 2750":  chmod(0o750 | os.ModeSetgid),
		"owner only 2700":  chmod(0o700 | os.ModeSetgid),
		"extended ACL": func(t *testing.T, root string, _ admininit.Paths) {
			setACL(t, root, "system.posix_acl_access", 0o770, [3]uint32{0x02, 7, 65534})
		},
		"default ACL": func(t *testing.T, root string, _ admininit.Paths) { setACL(t, root, "system.posix_acl_default", 0o770) },
		"other group": func(t *testing.T, _ string, _ admininit.Paths) {
			t.Cleanup(admininit.SetTestTrust(uid, gid+1, uid, tmp))
		},
		"other owner": func(t *testing.T, _ string, _ admininit.Paths) {
			t.Cleanup(admininit.SetTestTrust(uid+1, gid, uid, tmp))
		},
		"untrusted sub dir": nil,
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			if setup == nil {
				sub := filepath.Join(root, "sub")
				if err := os.Mkdir(sub, 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(sub, 0o777); err != nil {
					t.Fatal(err)
				}
				p.JobLedger = filepath.Join(sub, "jobs.db")
			} else {
				setup(t, root, p)
			}
			_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
			wantRefusal(t, err, admininit.ReasonUntrustedDataRoot, false)
			for _, n := range []string{admininit.IntentName, admininit.RefusedName} {
				if exists(t, filepath.Join(adminDir, n)) {
					t.Fatalf("%s written", n)
				}
			}
			if exists(t, p.JobLedger) || exists(t, p.Authority) {
				t.Fatal("a database was created")
			}
		})
	}
}

// I-T1 ancestors: an ancestor that group or other can write, or that grants
// write through an ACL entry, is refused; with the production rule, an
// ancestor owned by anyone but root is refused.
func TestInitRefusesUntrustedAncestor(t *testing.T) {
	cases := map[string]func(t *testing.T, anc string){
		"other write": func(t *testing.T, anc string) {
			if err := os.Chmod(anc, 0o757); err != nil {
				t.Fatal(err)
			}
		},
		"group write": func(t *testing.T, anc string) {
			if err := os.Chmod(anc, 0o775); err != nil {
				t.Fatal(err)
			}
		},
		"ACL write": func(t *testing.T, anc string) {
			setACL(t, anc, "system.posix_acl_access", 0o755, [3]uint32{0x02, 7, 65534})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			anc := filepath.Join(t.TempDir(), "anc")
			if err := os.Mkdir(anc, 0o755); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(anc, "root")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			trust(t, root)
			setup(t, anc)
			adminDir := t.TempDir()
			p := admininit.Paths{JobLedger: filepath.Join(root, "jobs.db"), Authority: filepath.Join(root, "authority.db")}
			_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
			wantRefusal(t, err, admininit.ReasonUntrustedDataRoot, false)
			checkNothingRecorded(t, adminDir, root)
		})
	}
	t.Run("production rule", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: the temporary ancestors are root-owned")
		}
		defer admininit.ProductionAncestors()()
		adminDir, root, p := layout(t)
		_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
		wantRefusal(t, err, admininit.ReasonUntrustedDataRoot, false)
		checkNothingRecorded(t, adminDir, root)
	})
}

// I-T2 step 5: at a first init any existing database or sidecar name, even an
// empty file or another installation's copied pair, is refused durably, and a
// plain rerun refuses without adopting it.
func TestInitRefusesPreexistingFiles(t *testing.T) {
	cases := map[string]func(t *testing.T, p admininit.Paths){
		"empty job ledger": func(t *testing.T, p admininit.Paths) { writeFile(t, p.JobLedger, nil) },
		"empty authority":  func(t *testing.T, p admininit.Paths) { writeFile(t, p.Authority, nil) },
		"ledger WAL":       func(t *testing.T, p admininit.Paths) { writeFile(t, p.JobLedger+"-wal", nil) },
		"authority journal": func(t *testing.T, p admininit.Paths) {
			writeFile(t, p.Authority+"-journal", nil)
		},
		"foreign pair": func(t *testing.T, p admininit.Paths) {
			other := t.TempDir()
			op := admininit.Paths{JobLedger: filepath.Join(other, "jobs.db"), Authority: filepath.Join(other, "authority.db")}
			if _, err := admininit.ProvisionAndBootstrap(context.Background(), op, nil); err != nil {
				t.Fatal(err)
			}
			writeFile(t, p.JobLedger, []byte(readFile(t, op.JobLedger)))
			writeFile(t, p.Authority, []byte(readFile(t, op.Authority)))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			setup(t, p)
			before := listing(t, root)
			_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
			wantRefusal(t, err, admininit.ReasonPreexisting, true)
			if exists(t, filepath.Join(adminDir, admininit.IntentName)) {
				t.Fatal("init-intent written")
			}
			if after := listing(t, root); !slices.Equal(before, after) {
				t.Fatalf("data root changed: %v -> %v", before, after)
			}
			checkRefusedStands(t, adminDir, root, p)
		})
	}
}

// I-T2 crash cuts C1–C5 and the steps around them: a crash after any durable
// step resumes on a plain rerun with exactly one installation id.
func TestInitCrashCuts(t *testing.T) {
	const id1 = "00000000000000000000000000000001"
	steps := []admininit.Step{
		admininit.StepIntent,          // C1
		admininit.StepCreateAuthority, // C2
		admininit.StepRecordAuthority, // C3
		admininit.StepAuthoritySchema, // C4
		admininit.StepCreateJobLedger, // C2 for the ledger
		admininit.StepRecordJobLedger, // C3 for the ledger
		admininit.StepJobLedgerSchema, // C4
		admininit.StepMarkers,         // C5
		admininit.StepRecordID,        // after C5
	}
	for _, at := range steps {
		t.Run(string(at), func(t *testing.T) {
			adminDir, root, p := layout(t)
			calls := 0
			restore := crashAt(at)
			_, err := admininit.Init(context.Background(), adminDir, root, p, countingID(&calls))
			restore()
			if !errors.Is(err, errCrash) {
				t.Fatalf("first run: %v, want the crash", err)
			}
			res, err := admininit.Init(context.Background(), adminDir, root, p, countingID(&calls))
			if err != nil {
				t.Fatal(err)
			}
			if res.InstallationID != id1 || calls != 1 {
				t.Fatalf("rerun = %+v after %d id calls; want %q after 1", res, calls, id1)
			}
			if in := readIntent(t, adminDir); in.InstallationID != id1 || len(in.Databases) != 2 {
				t.Fatalf("intent = %+v", in)
			}
			checkInitialized(t, p, id1)
		})
	}
}

// P3-TR6: the C2 cut accepts an unrecorded empty file only if its birth time is
// at or after the intent's reference time. An older file renamed in is refused
// durably.
func TestInitRefusesOlderUnrecordedFile(t *testing.T) {
	adminDir, root, p := layout(t)
	staged := filepath.Join(root, "staged")
	writeFile(t, staged, nil)
	if err := os.Chmod(staged, 0o660); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	crashInit(t, adminDir, root, p, admininit.StepIntent)
	if err := os.Rename(staged, p.Authority); err != nil {
		t.Fatal(err)
	}
	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	wantRefusal(t, err, admininit.ReasonUnrecorded, true)
	checkRefusedStands(t, adminDir, root, p)
}

// I-T2 C2 before the hand-over: a crash, or a failed chown or chmod, after the
// O_EXCL create and before the group and mode are set leaves an owner-only empty
// file. The plain rerun accepts it as the C2 cut, hands it over and records it
// with mode 0660, under umask 077 too.
func TestInitResumesCutBeforeHandOver(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	const id1 = "00000000000000000000000000000001"
	_, gid := testIDs()
	for _, at := range []admininit.Step{admininit.StepCreateAuthority, admininit.StepCreateJobLedger} {
		t.Run(string(at), func(t *testing.T) {
			adminDir, root, p := layout(t)
			path, name := p.Authority, "authority"
			if at == admininit.StepCreateJobLedger {
				path, name = p.JobLedger, "jobledger"
			}
			calls := 0
			restore := admininit.SetBeforeHandOver(func(s admininit.Step) error {
				if s == at {
					return errCrash
				}
				return nil
			})
			_, err := admininit.Init(context.Background(), adminDir, root, p, countingID(&calls))
			restore()
			if !errors.Is(err, errCrash) {
				t.Fatalf("first run: %v, want the crash", err)
			}
			if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != 0 {
				t.Fatalf("cut left %v, %v; want an empty 0600 file", fi, err)
			}
			res, err := admininit.Init(context.Background(), adminDir, root, p, countingID(&calls))
			if err != nil {
				t.Fatal(err)
			}
			if res.InstallationID != id1 || calls != 1 {
				t.Fatalf("rerun = %+v after %d id calls; want %q after 1", res, calls, id1)
			}
			in := readIntent(t, adminDir)
			if d := in.db(t, name); d.Mode&0o7777 != 0o660 || d.GID != gid {
				t.Fatalf("%s recorded mode %o gid %d; want 0660 group %d", name, d.Mode, d.GID, gid)
			}
			if exists(t, filepath.Join(adminDir, admininit.RefusedName)) {
				t.Fatal("init-refused written")
			}
			checkHandOver(t, p, gid)
			checkInitialized(t, p, id1)
		})
	}
}

// The resume hands over only the cut file it checked: another empty owner-only
// file renamed over the name between the check and the hand-over is refused
// durably and left as it is.
func TestInitRefusesSwapDuringCutHandOver(t *testing.T) {
	adminDir, root, p := layout(t)
	restore := admininit.SetBeforeHandOver(func(s admininit.Step) error {
		if s == admininit.StepCreateAuthority {
			return errCrash
		}
		return nil
	})
	_, err := admininit.Init(context.Background(), adminDir, root, p, nil)
	restore()
	if !errors.Is(err, errCrash) {
		t.Fatalf("first run: %v, want the crash", err)
	}
	swapped := false
	restore = admininit.SetBeforeHandOver(func(s admininit.Step) error {
		if s != admininit.StepCreateAuthority || swapped {
			return nil
		}
		swapped = true
		tmp := p.Authority + ".swap"
		writeFile(t, tmp, nil)
		return os.Rename(tmp, p.Authority)
	})
	_, err = admininit.Init(context.Background(), adminDir, root, p, noID(t))
	restore()
	if !swapped {
		t.Fatal("the resume did not reach the hand-over")
	}
	wantRefusal(t, err, admininit.ReasonUnrecorded, true)
	if fi, err := os.Lstat(p.Authority); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("swapped file now %v, %v; want it left 0600", fi, err)
	}
	checkRefusedStands(t, adminDir, root, p)
}

// The owner-only C2 cut is accepted only as the file init created: an unrecorded
// file that is older than the intent, not empty, linked twice or readable by the
// group without being 0660 is still refused durably.
func TestInitRefusesForeignOwnerOnlyFile(t *testing.T) {
	cases := map[string]struct {
		mode  os.FileMode
		data  []byte
		older bool
		link  bool
	}{
		"0600 older than the intent": {mode: 0o600, older: true},
		"0600 not empty":             {mode: 0o600, data: []byte("x")},
		"0600 two links":             {mode: 0o600, link: true},
		"0640":                       {mode: 0o640},
		"0606":                       {mode: 0o606},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			adminDir, root, p := layout(t)
			staged := filepath.Join(root, "staged")
			if c.older {
				writeFile(t, staged, c.data)
				time.Sleep(20 * time.Millisecond)
			}
			crashInit(t, adminDir, root, p, admininit.StepIntent)
			if !c.older {
				writeFile(t, staged, c.data)
			}
			if err := os.Chmod(staged, c.mode); err != nil {
				t.Fatal(err)
			}
			if c.link {
				if err := os.Link(staged, staged+".2"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(staged, p.Authority); err != nil {
				t.Fatal(err)
			}
			_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
			wantRefusal(t, err, admininit.ReasonUnrecorded, true)
			if fi, err := os.Lstat(p.Authority); err != nil || fi.Mode().Perm() != c.mode {
				t.Fatalf("refused file now %v, %v; want it left at %o", fi, err, c.mode)
			}
			checkRefusedStands(t, adminDir, root, p)
		})
	}
}

// I-T2 step 4a for every database: a database directory on a filesystem without
// birth times, such as a nested mount below the data root, is refused before
// init-intent is written or either database is created, and on a resume before
// anything more is created. The refusal is retryable.
func TestInitProbesEveryDatabaseFilesystem(t *testing.T) {
	adminDir, root := t.TempDir(), trustedDir(t)
	sub := filepath.Join(root, "ledger")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	trust(t, sub)
	p := admininit.Paths{JobLedger: filepath.Join(sub, "jobs.db"), Authority: filepath.Join(root, "authority.db")}
	restore := admininit.SetNoBirthTimeAt(sub)
	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	wantRefusal(t, err, admininit.ReasonUnsupportedIdentity, false)
	for _, n := range []string{admininit.IntentName, admininit.RefusedName} {
		if exists(t, filepath.Join(adminDir, n)) {
			t.Fatalf("%s written", n)
		}
	}
	if exists(t, p.Authority) || exists(t, p.JobLedger) {
		t.Fatal("a database was created")
	}
	restore()

	crashInit(t, adminDir, root, p, admininit.StepIntent)
	before := readIntent(t, adminDir)
	restore = admininit.SetNoBirthTimeAt(sub)
	_, err = admininit.Init(context.Background(), adminDir, root, p, noID(t))
	restore()
	wantRefusal(t, err, admininit.ReasonUnsupportedIdentity, false)
	if exists(t, p.Authority) || exists(t, p.JobLedger) || exists(t, filepath.Join(adminDir, admininit.RefusedName)) {
		t.Fatal("the resume created a database or recorded a refusal")
	}
	if after := readIntent(t, adminDir); after.Nonce != before.Nonce || len(after.Databases) != 0 {
		t.Fatalf("intent changed: %+v -> %+v", before, after)
	}
	if _, err := admininit.Init(context.Background(), adminDir, root, p, nil); err != nil {
		t.Fatalf("rerun on a supported filesystem: %v", err)
	}
}

// I-T2 step 6: a recorded database replaced by another file, or removed, is
// refused durably on the rerun.
func TestInitRefusesChangedRecordedFile(t *testing.T) {
	t.Run("replaced", func(t *testing.T) {
		adminDir, root, p := layout(t)
		crashInit(t, adminDir, root, p, admininit.StepRecordJobLedger)
		tmp := p.JobLedger + ".new"
		writeFile(t, tmp, nil)
		if err := os.Chmod(tmp, 0o660); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
		_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
		wantRefusal(t, err, admininit.ReasonIdentity, true)
		checkRefusedStands(t, adminDir, root, p)
	})
	t.Run("removed", func(t *testing.T) {
		adminDir, root, p := layout(t)
		crashInit(t, adminDir, root, p, admininit.StepRecordAuthority)
		if err := os.Remove(p.Authority); err != nil {
			t.Fatal(err)
		}
		_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
		wantRefusal(t, err, admininit.ReasonRemoved, true)
		checkRefusedStands(t, adminDir, root, p)
	})
	t.Run("data root moved", func(t *testing.T) {
		adminDir, root, p := layout(t)
		crashInit(t, adminDir, root, p, admininit.StepIntent)
		other := trustedDir(t)
		op := admininit.Paths{JobLedger: filepath.Join(other, "jobs.db"), Authority: filepath.Join(other, "authority.db")}
		_, err := admininit.Init(context.Background(), adminDir, other, op, noID(t))
		wantRefusal(t, err, admininit.ReasonDataRoot, true)
		checkRefusedStands(t, adminDir, other, op)
	})
}

// I-T2 identity tuple: each recorded field is compared. A record whose birth
// time, inode number, mode or group differs from the file, or whose generation
// differs where one was recorded, refuses the resume durably.
func TestInitComparesEveryTupleField(t *testing.T) {
	fields := map[string]func(int64) int64{
		"btime_nsec": func(v int64) int64 { return (v + 1) % 1e9 },
		"btime_sec":  func(v int64) int64 { return v - 1 },
		"ino":        func(v int64) int64 { return v + 1 },
		"mode":       func(v int64) int64 { return v ^ 0o020 },
		"gid":        func(v int64) int64 { return v + 1 },
		"generation": func(v int64) int64 { return v + 1 },
	}
	for field, edit := range fields {
		t.Run(field, func(t *testing.T) {
			adminDir, root, p := layout(t)
			crashInit(t, adminDir, root, p, admininit.StepRecordJobLedger)
			if field == "generation" && readIntent(t, adminDir).db(t, "jobledger").Generation == nil {
				t.Skip("this filesystem reports no inode generation")
			}
			editIntent(t, adminDir, "jobledger", field, edit)
			_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
			wantRefusal(t, err, admininit.ReasonIdentity, true)
		})
	}
}

// P2-TR1/P3-TR8 same-owner churn in one clock tick: right after init records
// the job ledger, the same owner renames new empty 0660 files over the name
// until one lands on the recorded inode number. With the generation ignored (as
// on ext4, where the owner can set it), only the birth time tells the files
// apart. It does because init closes the descriptor only after the creation
// tick: TestInitWaitsPastBirthTimeTick checks the wait that guarantees this even
// when the record's fsync is shorter than a tick.
func TestInitRefusesSameOwnerChurnWithinTick(t *testing.T) {
	defer admininit.SetUseGeneration(false)()
	adminDir, root, p := layout(t)
	var recorded uint64
	restore := admininit.SetAfterStep(func(s admininit.Step) error {
		if s != admininit.StepRecordJobLedger {
			return nil
		}
		return errCrash
	})
	_, err := admininit.Init(context.Background(), adminDir, root, p, nil)
	restore()
	if !errors.Is(err, errCrash) {
		t.Fatalf("first run: %v", err)
	}
	recorded = readIntent(t, adminDir).db(t, "jobledger").Ino
	reused := false
	for i := 0; i < 4000 && !reused; i++ {
		tmp := fmt.Sprintf("%s.churn%d", p.JobLedger, i)
		writeFile(t, tmp, nil)
		if err := os.Chmod(tmp, 0o660); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
		var st unix.Stat_t
		if err := unix.Lstat(p.JobLedger, &st); err != nil {
			t.Fatal(err)
		}
		reused = st.Ino == recorded
	}
	if !reused {
		t.Skip("the filesystem did not reuse the inode number in 4000 files")
	}
	_, err = admininit.Init(context.Background(), adminDir, root, p, noID(t))
	wantRefusal(t, err, admininit.ReasonIdentity, true)
}

// TRUST r4 step 7: the record is durable while init still holds the descriptor
// it created the file with, and the coarse clock is already past the file's
// birth time.
func TestInitRecordsBeforeClosingDescriptor(t *testing.T) {
	adminDir, root, p := layout(t)
	checked := 0
	defer admininit.SetBeforeClose(func(s admininit.Step) {
		name, path := "authority", p.Authority
		if s == admininit.StepRecordJobLedger {
			name, path = "jobledger", p.JobLedger
		}
		rec := readIntent(t, adminDir).db(t, name)
		var ts unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_REALTIME_COARSE, &ts); err != nil {
			t.Fatal(err)
		}
		if now := (testStamp{Sec: ts.Sec, Nsec: uint32(ts.Nsec)}); !rec.btime().before(now) { // #nosec G115 -- tv_nsec is below 1e9.
			t.Errorf("%s: coarse clock %+v not past birth time %+v", name, now, rec.btime())
		}
		if !holdsFD(t, path) {
			t.Errorf("%s: no open descriptor to %s when the record is durable", name, path)
		}
		checked++
	})()
	if _, err := admininit.Init(context.Background(), adminDir, root, p, nil); err != nil {
		t.Fatal(err)
	}
	if checked != 2 {
		t.Fatalf("checked %d records, want 2", checked)
	}
}

// P3-TR8: the step-7 wait is load-bearing only when the file is closed within
// its creation tick. Here fsync of the record usually outlasts a tick, so the
// churn test above passes even without the wait; this test holds the clock on
// the birth-time tick instead and checks that init keeps reading the clock until
// it moves past, for each created file, before the record is closed.
func TestInitWaitsPastBirthTimeTick(t *testing.T) {
	adminDir, root, p := layout(t)
	held := map[testStamp]int{}
	defer admininit.SetTickWait(func(bs int64, bn uint32, ns int64, nn uint32) (int64, uint32) {
		b := testStamp{Sec: bs, Nsec: bn}
		if held[b]++; held[b] <= 3 {
			return bs, bn
		}
		return ns, nn
	})()
	if _, err := admininit.Init(context.Background(), adminDir, root, p, nil); err != nil {
		t.Fatal(err)
	}
	in := readIntent(t, adminDir)
	for _, name := range []string{"authority", "jobledger"} {
		if n := held[in.db(t, name).btime()]; n < 4 {
			t.Fatalf("%s: the wait read the clock %d times for its birth time, want at least 4 (%v)", name, n, held)
		}
	}
}

// A clock that never passes the birth time stops init after a bounded wait,
// with a retryable refusal.
func TestInitStuckClockIsRetryable(t *testing.T) {
	adminDir, root, p := layout(t)
	restore := admininit.SetTickWait(func(bs int64, bn uint32, _ int64, _ uint32) (int64, uint32) { return bs, bn })
	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	restore()
	if !errors.Is(err, admininit.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if exists(t, filepath.Join(adminDir, admininit.RefusedName)) {
		t.Fatal("init-refused written for a stuck clock")
	}
	if _, err := admininit.Init(context.Background(), adminDir, root, p, nil); err != nil {
		t.Fatalf("rerun with a working clock: %v", err)
	}
}

// I-T2 step 8: the database phase binds each file to its recorded tuple before
// it writes. A new empty 0660 file renamed over the recorded authority after it
// was recorded passes the phase's own same-file check, because the phase first
// sees it then; the tuple refuses it durably before any schema is written.
func TestInitPhaseBindsRecordedTuple(t *testing.T) {
	adminDir, root, p := layout(t)
	defer admininit.SetAfterStep(func(s admininit.Step) error {
		if s != admininit.StepRecordJobLedger {
			return nil
		}
		tmp := p.Authority + ".new"
		writeFile(t, tmp, nil)
		if err := os.Chmod(tmp, 0o660); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p.Authority); err != nil {
			t.Fatal(err)
		}
		return nil
	})()
	_, err := admininit.Init(context.Background(), adminDir, root, p, nil)
	wantRefusal(t, err, admininit.ReasonBinding, true)
	if fi, err := os.Stat(p.Authority); err != nil || fi.Size() != 0 {
		t.Fatalf("replacement authority: %v, %v; want it untouched and empty", fi, err)
	}
	checkRefusedStands(t, adminDir, root, p)
}

// I-T2 step 9: a ledger replaced after the database phase's last check and
// before the installation id is recorded is refused durably, and no id is
// recorded for it.
func TestInitRechecksNamesAfterPhase(t *testing.T) {
	adminDir, root, p := layout(t)
	defer admininit.SetAfterStep(func(s admininit.Step) error {
		if s != admininit.StepMarkers {
			return nil
		}
		tmp := p.JobLedger + ".new"
		writeFile(t, tmp, []byte(readFile(t, p.JobLedger)))
		if err := os.Chmod(tmp, 0o660); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p.JobLedger); err != nil {
			t.Fatal(err)
		}
		return nil
	})()
	_, err := admininit.Init(context.Background(), adminDir, root, p, nil)
	wantRefusal(t, err, admininit.ReasonIdentity, true)
	if id := readIntent(t, adminDir).InstallationID; id != "" {
		t.Fatalf("installation id %q recorded for a replaced ledger", id)
	}
	checkRefusedStands(t, adminDir, root, p)
}

// holdsFD reports whether this process has a descriptor open on path.
func holdsFD(t *testing.T, path string) bool {
	t.Helper()
	des, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	for _, de := range des {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", de.Name())); err == nil && target == path {
			return true
		}
	}
	return false
}

// I-T2 step 9 (E9): another installation's ledger written in place over the
// recorded file keeps its identity tuple but carries another installation id,
// which is refused durably.
func TestInitRefusesInstallationIDChangedInPlace(t *testing.T) {
	adminDir, root, p := layout(t)
	if _, err := admininit.Init(context.Background(), adminDir, root, p, nil); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	op := admininit.Paths{JobLedger: filepath.Join(other, "jobs.db"), Authority: filepath.Join(other, "authority.db")}
	if _, err := admininit.ProvisionAndBootstrap(context.Background(), op, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"-wal", "-shm"} {
		if err := os.Remove(p.JobLedger + s); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	// #nosec G306 -- O_TRUNC keeps the inode and its mode.
	if err := os.WriteFile(p.JobLedger, []byte(readFile(t, op.JobLedger)), 0o660); err != nil {
		t.Fatal(err)
	}
	_, err := admininit.Init(context.Background(), adminDir, root, p, noID(t))
	wantRefusal(t, err, admininit.ReasonInstallationID, true)
	checkRefusedStands(t, adminDir, root, p)
}

// I-T2 step 8: a binding refusal of the database phase, here a foreign file
// renamed over the ledger after the step's first check, is recorded, so the
// plain rerun refuses instead of adopting the replacement (F1).
func TestInitBindingRefusalStands(t *testing.T) {
	adminDir, root, p := layout(t)
	staged := filepath.Join(t.TempDir(), "staged.db")
	foreign(t, staged)
	moved := filepath.Join(root, "staged.db")
	if err := os.Rename(staged, moved); err != nil {
		t.Fatal(err)
	}
	defer admininit.SetBeforeStep(func(s admininit.Step) {
		if s != admininit.StepMarkers {
			return
		}
		if err := os.Rename(p.JobLedger, p.JobLedger+".bound"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, p.JobLedger); err != nil {
			t.Fatal(err)
		}
	})()
	_, err := admininit.Init(context.Background(), adminDir, root, p, nil)
	wantRefusal(t, err, admininit.ReasonBinding, true)
	checkNoMarkers(t, p.JobLedger+".bound")
	checkRefusedStands(t, adminDir, root, p)
}

// I-T2 step 4a: a data root on a filesystem without birth times (tmpfs on this
// kernel) is refused before anything is recorded, and the refusal is retryable.
func TestInitRefusesUnsupportedFilesystem(t *testing.T) {
	base, err := os.MkdirTemp("/dev/shm", "sori-admininit-")
	if err != nil {
		t.Skipf("no /dev/shm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, base, 0, unix.STATX_BTIME, &stx); err != nil || stx.Mask&unix.STATX_BTIME != 0 {
		t.Skipf("/dev/shm reports birth times here (%v)", err)
	}
	trust(t, base)
	adminDir := t.TempDir()
	p := admininit.Paths{JobLedger: filepath.Join(base, "jobs.db"), Authority: filepath.Join(base, "authority.db")}
	_, err = admininit.Init(context.Background(), adminDir, base, p, noID(t))
	wantRefusal(t, err, admininit.ReasonUnsupportedIdentity, false)
	checkNothingRecorded(t, adminDir, base)
}
