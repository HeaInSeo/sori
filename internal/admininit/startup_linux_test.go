//go:build linux

package admininit_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/jobledger"
)

// fsState is every name below each dir with its type, mode, owner, size,
// times and, for a regular file, the SHA-256 of its bytes. Two equal states
// show that nothing below the dirs was created, removed, changed or chmodded.
func fsState(t *testing.T, dirs ...string) string {
	t.Helper()
	var b strings.Builder
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			st, _ := fi.Sys().(*syscall.Stat_t)
			fmt.Fprintf(&b, "%s %s uid=%d gid=%d size=%d ino=%d mtime=%d.%d ctime=%d.%d",
				path, fi.Mode(), st.Uid, st.Gid, fi.Size(), st.Ino, st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec, st.Ctim.Nsec)
			if fi.Mode().IsRegular() {
				data, err := os.ReadFile(path) // #nosec G304 -- test directories.
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
	}
	return b.String()
}

// classifyNoWrite runs ClassifyStart and fails the test if it changed anything
// in the admin area or the data root.
func classifyNoWrite(t *testing.T, adminDir, root, hostID string, p admininit.Paths, last *admininit.PolicyObservation) admininit.StartClass {
	t.Helper()
	before := fsState(t, adminDir, root)
	c := admininit.ClassifyStart(context.Background(), adminDir, root, hostID, p, last)
	if after := fsState(t, adminDir, root); after != before {
		t.Fatalf("ClassifyStart wrote:\n%s\nwant\n%s", after, before)
	}
	return c
}

func wantRow(t *testing.T, c admininit.StartClass, row admininit.StartRow) {
	t.Helper()
	if c.Row != row || c.Writable != (row == admininit.RowS3) {
		t.Fatalf("classified %s writable=%v (%s), want %s writable=%v", c.Row, c.Writable, c.Reason, row, row == admininit.RowS3)
	}
}

// initialized runs a complete init for host h1 and returns its layout and id.
func initialized(t *testing.T) (adminDir, root string, p admininit.Paths, id string) {
	t.Helper()
	adminDir, root, p = layout(t)
	res, err := admininit.Init(context.Background(), adminDir, root, testHostID, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return adminDir, root, p, res.InstallationID
}

func corrupt(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test admin area.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte(" "), data...), 0o640); err != nil { // #nosec G306 -- test admin area.
		t.Fatal(err)
	}
}

// removeDB removes a database file and its SQLite sidecars.
func removeDB(t *testing.T, path string) {
	t.Helper()
	for _, s := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + s); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src) // #nosec G304 -- test data root.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClassifyStartFreshIsS1: no init at all, pristine paths: S1, RO, and the
// classifier creates neither database nor any admin file.
func TestClassifyStartFreshIsS1(t *testing.T) {
	adminDir, root, p := layout(t)
	wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS1)
	for _, path := range []string{p.JobLedger, p.Authority} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s exists after classify: %v", path, err)
		}
	}
}

// TestClassifyStartCompleteInitIsS3: a complete init for h1 started as h1 is
// S3, the only writable row, and reports the policy observation.
func TestClassifyStartCompleteInitIsS3(t *testing.T) {
	adminDir, root, p, _ := initialized(t)
	c := classifyNoWrite(t, adminDir, root, testHostID, p, nil)
	wantRow(t, c, admininit.RowS3)
	_, ps, _, err := admininit.ReadTail(adminDir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Policy == nil || c.Policy.Revision != 1 || c.Policy.Digest != ps.Digest {
		t.Fatalf("policy observation %+v, want revision 1 digest %s", c.Policy, ps.Digest)
	}
}

// TestClassifyStartRows: each implemented row and the S12 mapping of an
// unimplemented one, on real files and real SQLite, with no write.
func TestClassifyStartRows(t *testing.T) {
	t.Run("S0 invalid host id", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		for _, h := range []string{"", "h 1", strings.Repeat("h", 65)} {
			wantRow(t, classifyNoWrite(t, adminDir, root, h, p, nil), admininit.RowS0)
		}
	})
	t.Run("S0 invalid epoch", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		corrupt(t, filepath.Join(adminDir, admininit.EpochName))
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS0)
	})
	t.Run("S0 epoch symlink", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		e := filepath.Join(adminDir, admininit.EpochName)
		if err := os.Rename(e, e+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(e+".real", e); err != nil {
			t.Fatal(err)
		}
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS0)
	})
	t.Run("S0 invalid keyring", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		corrupt(t, filepath.Join(adminDir, admininit.KeysDirName, admininit.KeyringName))
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS0)
	})
	t.Run("S6 invalid policy state", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		corrupt(t, filepath.Join(adminDir, admininit.PolicyDirName, admininit.PolicyStateName))
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS6)
	})
	t.Run("S1 epoch absent with init marker, tail ignored", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		dropEpoch(t, adminDir)
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS1)
		corrupt(t, filepath.Join(adminDir, admininit.KeysDirName, admininit.KeyringName))
		corrupt(t, filepath.Join(adminDir, admininit.PolicyDirName, admininit.PolicyStateName))
		wantRow(t, classifyNoWrite(t, adminDir, root, "", p, nil), admininit.RowS1)
	})
	t.Run("S2 epoch absent store with content", func(t *testing.T) {
		adminDir, root, p, id := initialized(t)
		dropEpoch(t, adminDir)
		s, err := jobledger.Open(p.JobLedger)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AllocateNamespace(context.Background(), jobledger.Guard{InstallationID: id, ActivationEpoch: 1}, "p1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS2)
	})
	t.Run("S1c epoch with pristine data root", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		for _, path := range []string{p.JobLedger, p.Authority} {
			removeDB(t, path)
		}
		c := classifyNoWrite(t, adminDir, root, testHostID, p, nil)
		wantRow(t, c, admininit.RowS1c)
		if !strings.Contains(c.Reason, root) {
			t.Fatalf("S1c reason %q does not name the data root %s", c.Reason, root)
		}
		for _, path := range []string{p.JobLedger, p.Authority} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("%s exists after classify: %v", path, err)
			}
		}
	})
	t.Run("S1c epoch with empty files", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		for _, path := range []string{p.JobLedger, p.Authority} {
			removeDB(t, path)
			writeEmpty(t, path)
		}
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS1c)
	})
	t.Run("S1c epoch with replaced unmarked ledger", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		// A schema-only ledger from a fresh, never-initialized data root.
		_, _, other := layout(t)
		s, err := jobledger.Open(other.JobLedger)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		copyFile(t, other.JobLedger, p.JobLedger)
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS1c)
	})
	t.Run("S1c epoch with authority removed", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		if err := os.Remove(p.Authority); err != nil {
			t.Fatal(err)
		}
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS1c)
	})
	t.Run("S1d another installation's volume is never RW", func(t *testing.T) {
		// K1d: B has the same host, epoch 1 and cut 0 as A.
		adminDir, root, p, idA := initialized(t)
		_, _, pB, idB := initialized(t)
		if idA == idB {
			t.Fatal("two inits chose one installation id")
		}
		copyFile(t, pB.JobLedger, p.JobLedger)
		copyFile(t, pB.Authority, p.Authority)
		c := classifyNoWrite(t, adminDir, root, testHostID, p, nil)
		wantRow(t, c, admininit.RowS1d)
		if !strings.Contains(c.Reason, idB) || !strings.Contains(c.Reason, idA) {
			t.Fatalf("S1d reason %q does not name both installations", c.Reason)
		}
	})
	t.Run("S12 other host (S7 not implemented)", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		wantRow(t, classifyNoWrite(t, adminDir, root, "h2", p, nil), admininit.RowS12)
	})
	t.Run("S12 unknown schema", func(t *testing.T) {
		adminDir, root, p := layout(t)
		db, err := sql.Open("sqlite", p.JobLedger)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE foreign_table (a INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS12)
	})
	t.Run("S12 stray sidecar", func(t *testing.T) {
		adminDir, root, p := layout(t)
		writeEmpty(t, p.JobLedger+"-wal")
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS12)
	})
}

// crashImage returns a data root holding a crash image of an initialized
// installation's data root: the authority database, and the job ledger's main
// file and non-empty -wal copied while a writer still holds the store open with
// one committed, not checkpointed transaction. With shm the writer's -shm index
// is copied too, as a stale index beside the image.
func crashImage(t *testing.T, shm bool) (adminDir, root string, p admininit.Paths) {
	t.Helper()
	adminDir, src, srcPaths, id := initialized(t)
	_, root, p = layout(t)
	s, err := jobledger.Open(srcPaths.JobLedger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if _, err := s.AllocateNamespace(context.Background(), jobledger.Guard{InstallationID: id, ActivationEpoch: 1}, "p1"); err != nil {
		t.Fatal(err)
	}
	sidecars := []string{"", "-wal"}
	if shm {
		sidecars = append(sidecars, "-shm")
	}
	for _, sfx := range sidecars {
		copyFile(t, srcPaths.JobLedger+sfx, p.JobLedger+sfx)
	}
	copyFile(t, srcPaths.Authority, p.Authority)
	if fi, err := os.Stat(p.JobLedger + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("crash image of %s has no WAL content: %v", src, err)
	}
	return adminDir, root, p
}

// TestClassifyStartCrashImageNoWrite: a data root whose job ledger has
// committed pages only in its -wal (a crash image) cannot be read without
// SQLite's WAL recovery, which creates or rewrites the -shm index. The
// classifier must not write: it holds the start as S12 RO/HOLD and leaves the
// main file, the -wal and any -shm byte- and metadata-identical (pair F1).
func TestClassifyStartCrashImageNoWrite(t *testing.T) {
	t.Run("no -shm", func(t *testing.T) {
		adminDir, root, p := crashImage(t, false)
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS12)
		if _, err := os.Lstat(p.JobLedger + "-shm"); !os.IsNotExist(err) {
			t.Fatalf("%s-shm exists after classify: %v", p.JobLedger, err)
		}
	})
	t.Run("stale -shm", func(t *testing.T) {
		adminDir, root, p := crashImage(t, true)
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS12)
	})
	t.Run("-journal", func(t *testing.T) {
		adminDir, root, p, _ := initialized(t)
		writeEmpty(t, p.JobLedger+"-journal")
		wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, nil), admininit.RowS12)
	})
}

// TestClassifyStartLastObservedPolicy: the carried RW-C1 §3.2 runtime checks
// (FMT-PREP-r2 §4.3): high_water below the last observed revision, or the same
// revision with another digest, is S6, never RW.
func TestClassifyStartLastObservedPolicy(t *testing.T) {
	adminDir, root, p, _ := initialized(t)
	first := classifyNoWrite(t, adminDir, root, testHostID, p, nil)
	wantRow(t, first, admininit.RowS3)
	wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p, first.Policy), admininit.RowS3)
	wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p,
		&admininit.PolicyObservation{Revision: 2, Digest: first.Policy.Digest}), admininit.RowS6)
	wantRow(t, classifyNoWrite(t, adminDir, root, testHostID, p,
		&admininit.PolicyObservation{Revision: 1, Digest: "sha256:" + strings.Repeat("0", 64)}), admininit.RowS6)
}
