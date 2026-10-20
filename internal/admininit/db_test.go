package admininit_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/HeaInSeo/sori/authority/sqlitestore"
	"github.com/HeaInSeo/sori/internal/admininit"
	"github.com/HeaInSeo/sori/jobledger"
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func dataRoot(t *testing.T) admininit.Paths {
	t.Helper()
	dir := t.TempDir()
	return admininit.Paths{JobLedger: filepath.Join(dir, "jobs.db"), Authority: filepath.Join(dir, "authority.db")}
}

// countingID returns installation ids "…01", "…02", … and counts the calls.
func countingID(calls *int) func() (string, error) {
	return func() (string, error) {
		*calls++
		return "0000000000000000000000000000000" + string(rune('0'+*calls)), nil
	}
}

func noID(t *testing.T) func() (string, error) {
	return func() (string, error) {
		t.Error("newID called")
		return "", errors.New("newID called")
	}
}

// checkInitialized asserts the end state of a completed database phase.
func checkInitialized(t *testing.T, p admininit.Paths, id string) {
	t.Helper()
	ctx := context.Background()
	got, err := jobledger.InspectInitState(ctx, p.JobLedger)
	if err != nil || got.State != jobledger.InitMarkerEmpty || got.InstallationID != id {
		t.Fatalf("job ledger = %v %q, %v; want INIT_MARKER_EMPTY %q", got.State, got.InstallationID, err, id)
	}
	auth, err := sqlitestore.InspectInitState(ctx, p.Authority)
	if err != nil || auth != sqlitestore.InitSchemaOnly {
		t.Fatalf("authority = %v, %v; want SCHEMA_ONLY", auth, err)
	}
	s, err := jobledger.Open(p.JobLedger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if seq, digest, err := s.LedgerHead(ctx); err != nil || seq != 0 || digest != "" {
		t.Fatalf("ledger head = %d %q, %v; want empty ledger", seq, digest, err)
	}
}

func TestProvisionAndBootstrapFromPristine(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(t *testing.T, p admininit.Paths){
		"absent": func(*testing.T, admininit.Paths) {},
		"empty files": func(t *testing.T, p admininit.Paths) {
			writeFile(t, p.JobLedger, nil)
			writeFile(t, p.Authority, nil)
		},
		"schema only": func(t *testing.T, p admininit.Paths) {
			provision(t, p)
		},
		"authority schema only": func(t *testing.T, p admininit.Paths) {
			s, err := sqlitestore.Open(p.Authority)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		},
		"job ledger schema only": func(t *testing.T, p admininit.Paths) {
			s, err := jobledger.Open(p.JobLedger)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			p := dataRoot(t)
			setup(t, p)
			res, err := admininit.ProvisionAndBootstrap(ctx, p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Bootstrapped || !idPattern.MatchString(res.InstallationID) {
				t.Fatalf("result = %+v; want bootstrapped with a 32-hex id", res)
			}
			checkInitialized(t, p, res.InstallationID)
		})
	}
}

func TestRerunReusesInstallationID(t *testing.T) {
	ctx := context.Background()
	p := dataRoot(t)
	first, err := admininit.ProvisionAndBootstrap(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		again, err := admininit.ProvisionAndBootstrap(ctx, p, noID(t))
		if err != nil {
			t.Fatal(err)
		}
		if again.Bootstrapped || again.InstallationID != first.InstallationID {
			t.Fatalf("rerun = %+v; want reuse of %q", again, first.InstallationID)
		}
	}
	checkInitialized(t, p, first.InstallationID)
}

// TestSameNameInOtherDirectory checks that the alias check compares files, not
// names: one base name in two directories is two databases.
func TestSameNameInOtherDirectory(t *testing.T) {
	p := admininit.Paths{
		JobLedger: filepath.Join(t.TempDir(), "sori.db"),
		Authority: filepath.Join(t.TempDir(), "sori.db"),
	}
	res, err := admininit.ProvisionAndBootstrap(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkInitialized(t, p, res.InstallationID)
}

func TestNewInstallationIDsDiffer(t *testing.T) {
	a, err := admininit.NewInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := admininit.NewInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if !idPattern.MatchString(a) || !idPattern.MatchString(b) || a == b {
		t.Fatalf("ids %q, %q; want two different 32-hex ids", a, b)
	}
}

// TestCrashResume stops the run after each durable step, as a crash would, and
// checks that a rerun completes. Before the marker commit no id is observable, so
// the rerun chooses one; after it, the rerun reuses the committed id.
func TestCrashResume(t *testing.T) {
	ctx := context.Background()
	crash := errors.New("crash")
	const id1 = "00000000000000000000000000000001"
	cases := []struct {
		step         admininit.Step
		ledgerAbsent bool
		// crashCalls is the number of ids chosen before the crash: none before
		// the marker transaction.
		crashCalls int
		crashState jobledger.InitState
	}{
		{step: admininit.StepAuthoritySchema, ledgerAbsent: true, crashCalls: 0, crashState: jobledger.InitPristine},
		{step: admininit.StepJobLedgerSchema, crashCalls: 0, crashState: jobledger.InitSchemaOnly},
		{step: admininit.StepMarkers, crashCalls: 1, crashState: jobledger.InitMarkerEmpty},
	}
	for _, tc := range cases {
		t.Run(string(tc.step), func(t *testing.T) {
			p := dataRoot(t)
			calls := 0
			restore := admininit.SetAfterStep(func(s admininit.Step) error {
				if s == tc.step {
					return crash
				}
				return nil
			})
			_, err := admininit.ProvisionAndBootstrap(ctx, p, countingID(&calls))
			restore()
			if !errors.Is(err, crash) {
				t.Fatalf("first run err = %v; want crash", err)
			}
			if _, err := os.Lstat(p.JobLedger); errors.Is(err, fs.ErrNotExist) != tc.ledgerAbsent {
				t.Fatalf("job ledger stat after crash: %v (want absent %v)", err, tc.ledgerAbsent)
			}
			if got, err := jobledger.InspectInitState(ctx, p.JobLedger); err != nil || got.State != tc.crashState {
				t.Fatalf("job ledger after crash = %v, %v; want %v", got.State, err, tc.crashState)
			}
			if auth, err := sqlitestore.InspectInitState(ctx, p.Authority); err != nil || auth != sqlitestore.InitSchemaOnly {
				t.Fatalf("authority after crash = %v, %v; want SCHEMA_ONLY", auth, err)
			}
			if calls != tc.crashCalls {
				t.Fatalf("%d ids chosen before the crash; want %d", calls, tc.crashCalls)
			}
			res, err := admininit.ProvisionAndBootstrap(ctx, p, countingID(&calls))
			if err != nil {
				t.Fatal(err)
			}
			// Either way exactly one id is ever chosen: the rerun chooses it before
			// the marker commit and reuses it after.
			if res.InstallationID != id1 || calls != 1 {
				t.Fatalf("rerun = %+v after %d id calls; want %q after 1", res, calls, id1)
			}
			if res.Bootstrapped != (tc.step != admininit.StepMarkers) {
				t.Fatalf("rerun bootstrapped = %v", res.Bootstrapped)
			}
			checkInitialized(t, p, id1)
		})
	}
}

func TestIDFailureWritesNoMarker(t *testing.T) {
	ctx := context.Background()
	p := dataRoot(t)
	boom := errors.New("no entropy")
	if _, err := admininit.ProvisionAndBootstrap(ctx, p, func() (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v; want %v", err, boom)
	}
	if got, err := jobledger.InspectInitState(ctx, p.JobLedger); err != nil || got.State != jobledger.InitSchemaOnly {
		t.Fatalf("job ledger = %v, %v; want SCHEMA_ONLY", got.State, err)
	}
	res, err := admininit.ProvisionAndBootstrap(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkInitialized(t, p, res.InstallationID)
}

// TestRefusedWritesNothing checks that a refusal of either database leaves both
// databases exactly as they were, including an absent one.
func TestRefusedWritesNothing(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(t *testing.T, p admininit.Paths) admininit.Paths{
		"foreign job ledger, pristine authority": func(t *testing.T, p admininit.Paths) admininit.Paths {
			foreign(t, p.JobLedger)
			return p
		},
		"pristine job ledger, foreign authority": func(t *testing.T, p admininit.Paths) admininit.Paths {
			foreign(t, p.Authority)
			return p
		},
		"schema-only job ledger, foreign authority": func(t *testing.T, p admininit.Paths) admininit.Paths {
			s, err := jobledger.Open(p.JobLedger)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			foreign(t, p.Authority)
			return p
		},
		"job ledger with content": func(t *testing.T, p admininit.Paths) admininit.Paths {
			provision(t, p)
			exec(t, p.JobLedger, `INSERT INTO ledger_meta(key, value) VALUES ('restore_nonce', 'n')`)
			return p
		},
		"authority with content": func(t *testing.T, p admininit.Paths) admininit.Paths {
			provision(t, p)
			exec(t, p.Authority, `INSERT INTO meta(key, value) VALUES ('extra', 'x')`)
			return p
		},
		"markers with pristine authority": func(t *testing.T, p admininit.Paths) admininit.Paths {
			if _, err := admininit.ProvisionAndBootstrap(context.Background(), p, nil); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				if err := os.Remove(p.Authority + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
			}
			return p
		},
		"one path for both": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p.Authority = filepath.Join(filepath.Dir(p.JobLedger), ".", filepath.Base(p.JobLedger))
			return p
		},
		"hard link to one empty file": func(t *testing.T, p admininit.Paths) admininit.Paths {
			writeFile(t, p.JobLedger, nil)
			if err := os.Link(p.JobLedger, p.Authority); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"symlinked directory, absent file": func(t *testing.T, p admininit.Paths) admininit.Paths {
			link := filepath.Join(t.TempDir(), "root")
			if err := os.Symlink(filepath.Dir(p.JobLedger), link); err != nil {
				t.Fatal(err)
			}
			p.Authority = filepath.Join(link, filepath.Base(p.JobLedger))
			return p
		},
		"symlinked directory, empty file": func(t *testing.T, p admininit.Paths) admininit.Paths {
			writeFile(t, p.JobLedger, nil)
			link := filepath.Join(t.TempDir(), "root")
			if err := os.Symlink(filepath.Dir(p.JobLedger), link); err != nil {
				t.Fatal(err)
			}
			p.Authority = filepath.Join(link, filepath.Base(p.JobLedger))
			return p
		},
		// filepath.Dir would clean "link/.." away; the kernel follows the link first.
		"symlinked directory then .., absent authority": func(t *testing.T, p admininit.Paths) admininit.Paths {
			p.Authority = dotDotAlias(t, p.JobLedger)
			return p
		},
		"symlinked directory then .., absent job ledger": func(t *testing.T, p admininit.Paths) admininit.Paths {
			p.JobLedger = dotDotAlias(t, p.Authority)
			return p
		},
		// A case-insensitive volume names one file; a case-sensitive one is refused too.
		"names differ only in case, both absent": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p.Authority = filepath.Join(filepath.Dir(p.JobLedger), strings.ToUpper(filepath.Base(p.JobLedger)))
			return p
		},
		// A normalization-insensitive volume (default macOS APFS) names one file.
		"composed and decomposed names, both absent": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			return normalizationAlias(p, composed, decomposed)
		},
		"decomposed and composed names, both absent": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			return normalizationAlias(p, decomposed, composed)
		},
		"composed upper case and decomposed names, both absent": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			return normalizationAlias(p, strings.ToUpper(composed), decomposed)
		},
		"job ledger path unset": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p.JobLedger = ""
			return p
		},
		"authority path unset": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p.Authority = ""
			return p
		},
		"in-memory job ledger": func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p.JobLedger = ":memory:"
			return p
		},
	}
	// Opening the database that owns a sidecar would remove or overwrite the other
	// database as a stale WAL or journal.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		cases["authority at the job ledger's "+suffix] = func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p.Authority = p.JobLedger + suffix
			return p
		}
		cases["job ledger at the authority's "+suffix] = func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p.JobLedger = p.Authority + suffix
			return p
		}
		cases["decomposed authority at the composed job ledger's "+suffix] = func(_ *testing.T, p admininit.Paths) admininit.Paths {
			p = normalizationAlias(p, composed, decomposed)
			p.Authority += suffix
			return p
		}
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			base := dataRoot(t)
			p := setup(t, base)
			before := snapshot(t, base)
			if _, err := admininit.ProvisionAndBootstrap(ctx, p, noID(t)); !errors.Is(err, admininit.ErrRefused) {
				t.Fatalf("err = %v; want ErrRefused", err)
			}
			checkUnchanged(t, before, snapshot(t, base))
		})
	}
}

// composed and decomposed spell one name: "é" as U+00E9, and as "e" plus the
// combining acute accent U+0301.
const (
	composed   = "café.db"
	decomposed = "café.db"
)

// normalizationAlias names the job ledger ledgerName and the authority authName,
// both in the job ledger's directory.
func normalizationAlias(p admininit.Paths, ledgerName, authName string) admininit.Paths {
	dir := filepath.Dir(p.JobLedger)
	return admininit.Paths{JobLedger: filepath.Join(dir, ledgerName), Authority: filepath.Join(dir, authName)}
}

// TestNormalizationDistinctNames checks that the name rule only joins canonically
// equivalent spellings: "café.db" and "cafe.db" are two databases.
func TestNormalizationDistinctNames(t *testing.T) {
	p := normalizationAlias(dataRoot(t), composed, "cafe.db")
	res, err := admininit.ProvisionAndBootstrap(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkInitialized(t, p, res.InstallationID)
}

// dotDotAlias returns "<other>/link/../<dir name>/<file name>" for path, with link
// a symlink to path's directory. It names path's file, but its lexical parent
// <other>/<dir name> does not exist. The path is joined by hand because
// filepath.Join would clean the "..".
func dotDotAlias(t *testing.T, path string) string {
	t.Helper()
	dir := filepath.Dir(path)
	other := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(other, "link")); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	return other + sep + "link" + sep + ".." + sep + filepath.Base(dir) + sep + filepath.Base(path)
}

func provision(t *testing.T, p admininit.Paths) {
	t.Helper()
	a, err := sqlitestore.Open(p.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := jobledger.Open(p.JobLedger)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// foreign writes an SQLite file that holds some other application's table.
func foreign(t *testing.T, path string) {
	t.Helper()
	exec(t, path, `CREATE TABLE other_app (x)`)
}

func exec(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshot reads every file of the data root, keyed by name.
func snapshot(t *testing.T, p admininit.Paths) map[string]string {
	t.Helper()
	dir := filepath.Dir(p.JobLedger)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// checkUnchanged asserts that every file is byte-identical and that no database
// was created. A read-only inspection of a WAL database may leave SQLite's
// shared-memory index (-shm) and an empty -wal, which are bookkeeping, not content.
func checkUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for name, was := range before {
		if now, ok := after[name]; !ok || now != was {
			t.Errorf("%s changed or removed", name)
		}
	}
	for name, now := range after {
		if _, ok := before[name]; ok {
			continue
		}
		bookkeeping := strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-wal") && now == ""
		if !bookkeeping {
			t.Errorf("created %s with %d bytes", name, len(now))
		}
	}
}
