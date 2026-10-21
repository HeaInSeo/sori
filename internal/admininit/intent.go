package admininit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The T2 durable steps Init adds around the database phase (TRUST r4 §3.2/§3.3).
const (
	StepIntent          Step = "init-intent"      // C1: intent written, no database yet
	StepCreateAuthority Step = "create-authority" // C2: authority created, not recorded
	StepRecordAuthority Step = "record-authority" // C3: authority recorded, no schema
	StepCreateJobLedger Step = "create-jobledger"
	StepRecordJobLedger Step = "record-jobledger"
	StepRecordID        Step = "record-id" // after the installation id is recorded
)

// The database names in init-intent.
const (
	nameAuthority = "authority"
	nameJobLedger = "jobledger"
)

type database struct {
	name        string
	path        string
	create, rec Step
}

// databases lists both databases in creation order: the authority first, then
// the job ledger, as the database phase provisions them.
func databases(p Paths) []database {
	return []database{
		{name: nameAuthority, path: p.Authority, create: StepCreateAuthority, rec: StepRecordAuthority},
		{name: nameJobLedger, path: p.JobLedger, create: StepCreateJobLedger, rec: StepRecordJobLedger},
	}
}

// initLocked runs TRUST r4 §3.2 steps 2 and 4–9 while Init holds both locks.
// realRoot and p are the resolved data root and database paths.
func initLocked(ctx context.Context, adminDir, realRoot string, p Paths, newID func() (string, error)) (Result, error) {
	root, tr, err := preflightTrust(adminDir, realRoot, p)
	if err != nil {
		return Result{}, err
	}
	rec, err := recordFiles(adminDir, root, p, tr)
	if err != nil {
		return Result{}, recordRefusal(adminDir, err)
	}
	res, err := boundPhase(ctx, p, newID, rec)
	if err != nil {
		return Result{}, recordRefusal(adminDir, err)
	}
	if err := finish(adminDir, p, tr, rec, res); err != nil {
		return Result{}, recordRefusal(adminDir, err)
	}
	return res, nil
}

// preflightTrust is TRUST r4 §3.2 steps 2 to 4. Its refusals are not durable:
// nothing has been attempted, or the operator can fix the layout and rerun.
func preflightTrust(adminDir, realRoot string, p Paths) (rootID, trust, error) {
	// Step 2: a durable refusal stands.
	refused := adminPath(adminDir, RefusedName)
	if _, err := os.Lstat(refused); err == nil {
		return rootID{}, trust{}, refuse(ReasonRefusedStands, "an earlier refusal is recorded in %s; do not rerun: inspect the database files"+
			" (owner, mode, inode, birth time, init state), move them aside and clear the attempt", refused)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return rootID{}, trust{}, fmt.Errorf("admininit: %s: %w", RefusedName, err)
	}
	// Step 3, the activation epoch, is not written by this package yet.

	// Step 4a first: without birth times nothing below can be judged.
	if !identitySupported {
		return rootID{}, trust{}, refuse(ReasonUnsupportedIdentity, "this platform cannot read file birth times; init needs Linux statx")
	}
	root, err := rootIdentity(realRoot)
	if err != nil {
		return rootID{}, trust{}, err
	}
	// Step 4: G1 for the data root and the directories of both databases. G2 for
	// the files is the identity tuple, which also checks owner, group, mode and
	// links, and the first-init absence rule.
	tr, err := lookupTrust()
	if err != nil {
		return rootID{}, trust{}, refuse(ReasonUntrustedDataRoot, "%w", err)
	}
	if err := checkG1(realRoot, p, tr); err != nil {
		return rootID{}, trust{}, err
	}
	if err := checkTargetIdentity(p); err != nil {
		return rootID{}, trust{}, err
	}
	return root, tr, nil
}

// checkTargetIdentity is step 4a for each database: its directory, which may be
// a nested mount below the data root, must report birth times before init-intent
// is written or either database is created (retryable: nothing was attempted).
func checkTargetIdentity(p Paths) error {
	for _, db := range databases(p) {
		dir := filepath.Dir(db.path)
		_, err := probeIdentity(dir)
		if errors.Is(err, errNoBirthTime) {
			return refuse(ReasonUnsupportedIdentity, "%s directory %s: %w; move it to a filesystem that does (xfs, ext4, btrfs)", db.name, dir, err)
		}
		if err != nil {
			return fmt.Errorf("admininit: %s directory: %w", db.name, err)
		}
	}
	return nil
}

// probeIdentity reads the identity of a database directory for step 4a. Tests
// replace it to stand for a nested mount without birth times.
var probeIdentity = pathIdentity

// recordFiles is steps 5 to 7: a first init or a resume, then each absent
// database created and recorded.
func recordFiles(adminDir string, root rootID, p Paths, tr trust) (*intent, error) {
	rec, found, err := readIntent(adminDir)
	if err != nil {
		return nil, err
	}
	if !found {
		err = firstInit(adminDir, root, p, &rec)
	} else {
		err = resume(adminDir, root, p, tr, &rec)
	}
	if err != nil {
		return nil, err
	}
	for _, db := range databases(p) {
		if rec.find(db.name) != nil {
			continue
		}
		if err := createAndRecord(adminDir, db, tr, &rec); err != nil {
			return nil, err
		}
	}
	return &rec, nil
}

// boundPhase is step 8: the database phase, bound to the recorded tuples. A
// refusal of the phase is durable unless it comes from an I/O condition.
func boundPhase(ctx context.Context, p Paths, newID func() (string, error), rec *intent) (Result, error) {
	want := map[string]fileID{}
	for _, db := range databases(p) {
		want[db.path] = rec.find(db.name).fileID
	}
	res, err := provision(ctx, p, newID, want)
	if err != nil && errors.Is(err, ErrRefused) && !retryableIO(err) {
		err = &Refusal{Reason: ReasonBinding, Durable: true, Err: err}
	}
	return res, err
}

// finish is step 9 and the hand-over: both names still name the recorded files,
// the installation id matches or is recorded, and the sidecars are handed over.
func finish(adminDir string, p Paths, tr trust, rec *intent, res Result) error {
	for _, db := range databases(p) {
		if err := checkRecorded(db, rec.find(db.name).fileID); err != nil {
			return err
		}
	}
	if rec.InstallationID != "" && rec.InstallationID != res.InstallationID {
		return refuseDurable(ReasonInstallationID, "job ledger installation id %q, init-intent recorded %q", res.InstallationID, rec.InstallationID)
	}
	if err := handOverSidecars(p, tr); err != nil {
		return err
	}
	if rec.InstallationID == "" {
		rec.InstallationID = res.InstallationID
		if err := writeIntent(adminDir, rec); err != nil {
			return err
		}
	}
	return step(StepRecordID)
}

// rootIdentity identifies the data root, refusing a filesystem without birth
// times (step 4a, retryable: nothing was attempted).
func rootIdentity(realRoot string) (rootID, error) {
	id, err := pathIdentity(realRoot)
	if errors.Is(err, errNoBirthTime) {
		return rootID{}, refuse(ReasonUnsupportedIdentity, "data root %s: %w; move it to a filesystem that does (xfs, ext4, btrfs)", realRoot, err)
	}
	if err != nil {
		return rootID{}, fmt.Errorf("admininit: data root: %w", err)
	}
	return rootID{Path: realRoot, Dev: id.Dev, Ino: id.Ino, BtimeSec: id.BtimeSec, BtimeNsec: id.BtimeNsec}, nil
}

// checkG1 checks the data root, every directory from it down to each database,
// and every ancestor above it (I-T1). The refusal is retryable: nothing was
// created, and a resume still checks each recorded file by its tuple.
func checkG1(realRoot string, p Paths, tr trust) error {
	dirs := []string{realRoot}
	for _, path := range []string{p.Authority, p.JobLedger} {
		for dir := filepath.Dir(path); inside(realRoot, dir); dir = filepath.Dir(dir) {
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dirs {
		if err := checkTrustedDir(dir, tr); err != nil {
			return refuse(ReasonUntrustedDataRoot, "%w", err)
		}
	}
	if err := checkAncestors(realRoot); err != nil {
		return refuse(ReasonUntrustedDataRoot, "%w", err)
	}
	return nil
}

// firstInit is step 5: with no init-intent, both database names and their
// sidecar names must be absent; then init-intent is written before any database.
func firstInit(adminDir string, root rootID, p Paths, rec *intent) error {
	createdAt, err := coarseNow()
	if err != nil {
		return err
	}
	for _, db := range databases(p) {
		for _, name := range append([]string{db.path}, sidecarNames(db.path)...) {
			fi, err := os.Lstat(name)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return fmt.Errorf("admininit: %w", err)
			default:
				return refuseDurable(ReasonPreexisting, "first init but %s already exists (%s, %d bytes); init adopts only files it created", name, fi.Mode().Type(), fi.Size())
			}
		}
	}
	nonce, err := newNonce()
	if err != nil {
		return err
	}
	*rec = intent{Version: intentVersion, Nonce: nonce, CreatedAt: createdAt, DataRoot: root, Databases: []dbRecord{}}
	if err := writeIntent(adminDir, rec); err != nil {
		return err
	}
	return step(StepIntent)
}

// resume is step 6: the data root and every recorded database must match the
// record, and an unrecorded file is accepted only as the create→record cut C2.
func resume(adminDir string, root rootID, p Paths, tr trust, rec *intent) error {
	if rec.DataRoot != root {
		return refuseDurable(ReasonDataRoot, "data root %+v is not the one init-intent recorded (%+v)", root, rec.DataRoot)
	}
	for _, db := range databases(p) {
		if err := resumeOne(adminDir, db, tr, rec); err != nil {
			return err
		}
	}
	return nil
}

// resumeOne is step 6 for one database. The tuple comparison runs before the
// ACL check, so a replaced file is refused durably even if its ACL or owner
// would also fail G2 (P3-TR9).
func resumeOne(adminDir string, db database, tr trust, rec *intent) error {
	r := rec.find(db.name)
	if r != nil && r.Path != db.path {
		return refuse(ReasonIntentPaths, "%s path %s differs from %s in init-intent", db.name, db.path, r.Path)
	}
	obs, err := openIdentity(db.path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && r != nil:
		return refuseDurable(ReasonRemoved, "%s %s was recorded by init-intent and is gone", db.name, db.path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil && r != nil:
		// A symlink (ELOOP) or an unreadable entry at a recorded name.
		return refuseDurable(ReasonIdentity, "%s %s: %w", db.name, db.path, err)
	case err != nil:
		return refuseDurable(ReasonUnrecorded, "%s %s, not recorded by init-intent: %w", db.name, db.path, err)
	case r != nil:
		if !obs.regular() || !r.matches(obs, useGeneration) {
			return refuseDurable(ReasonIdentity, "%s %s is not the file init-intent recorded: have %s, recorded %s", db.name, db.path, obs, r.fileID)
		}
		return untrustedACL(db.path)
	}
	// C2: created by this init after the intent, crash before its record.
	if !c2Cut(obs, tr, rec.CreatedAt) {
		return refuseDurable(ReasonUnrecorded, "%s %s is not recorded by init-intent and is not an empty file this init created"+
			" (%s, intent created %s)", db.name, db.path, obs, rec.CreatedAt)
	}
	if err := untrustedACL(db.path); err != nil {
		return err
	}
	if obs.perm() != 0o660 {
		if obs, err = handOverCreated(db, obs, tr); err != nil {
			return err
		}
	}
	rec.Databases = append(rec.Databases, dbRecord{Name: db.name, Path: db.path, fileID: obs})
	return writeIntent(adminDir, rec)
}

// c2Cut reports whether an unrecorded file is what step 7 leaves at a crash
// between its create and its record: an empty regular file with one link, owner
// the init principal, group sori, born at or after the intent's reference time,
// and mode 0660 or, cut before the hand-over completed, the owner-only mode of
// the O_EXCL create. An owner-only file of the init principal is one no other
// user could have created or written.
func c2Cut(obs fileID, tr trust, createdAt stamp) bool {
	return obs.regular() && obs.Size == 0 && obs.Nlink == 1 && obs.UID == tr.uid && obs.GID == tr.gid &&
		(obs.perm() == 0o660 || obs.perm()&^0o600 == 0) && !createdAt.after(obs.btime())
}

// handOverCreated completes the G4 hand-over of a C2 file cut before it: it
// opens the name, checks that the descriptor holds the file obs saw, gives it
// group sori and mode 0660, and returns its identity read from the descriptor.
// A different file at the name is refused durably.
func handOverCreated(db database, obs fileID, tr trust) (id fileID, err error) {
	path := db.path
	if beforeHandOver != nil {
		if err := beforeHandOver(db.create); err != nil {
			return fileID{}, err
		}
	}
	// #nosec G304 -- a database path Init pinned inside the data root.
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return fileID{}, fmt.Errorf("admininit: hand over %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	held, err := fileIdentity(f)
	if err != nil {
		return fileID{}, fmt.Errorf("admininit: %w", err)
	}
	if !held.regular() || held.Size != 0 || !obs.matches(held, useGeneration) {
		return fileID{}, refuseDurable(ReasonUnrecorded, "%s changed during the hand-over: have %s, saw %s", path, held, obs)
	}
	if err := setGroupMode(f, int(tr.gid), 0o660); err != nil {
		return fileID{}, fmt.Errorf("admininit: hand over %s: %w", path, err)
	}
	if id, err = fileIdentity(f); err != nil {
		return fileID{}, fmt.Errorf("admininit: %w", err)
	}
	if err := errors.Join(f.Sync(), syncDir(filepath.Dir(path))); err != nil {
		return fileID{}, fmt.Errorf("admininit: sync %s: %w", path, err)
	}
	return id, nil
}

// untrustedACL refuses, retryably, a database file with extended ACL entries
// (G2).
func untrustedACL(path string) error {
	if err := fileACL(path); err != nil {
		return refuse(ReasonUntrustedDataRoot, "%w", err)
	}
	return nil
}

// createAndRecord is step 7 for one absent database: O_EXCL create, group sori
// and mode 0660 (G4), the tuple from the descriptor, fsync, the path checked
// against the descriptor, one clock tick, the record made durable, and only then
// the descriptor closed. Holding the descriptor until the record is durable keeps
// the inode from being freed and reused before it is recorded; the tick makes
// any later allocation get a strictly later birth time.
func createAndRecord(adminDir string, db database, tr trust, rec *intent) (err error) {
	f, err := createExcl(db.path)
	if errors.Is(err, fs.ErrExist) || errors.Is(err, errLoop) {
		return refuseDurable(ReasonPreexisting, "create %s: %w", db.path, err)
	}
	if err != nil {
		return fmt.Errorf("admininit: create %s: %w", db.path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if beforeHandOver != nil {
		if err := beforeHandOver(db.create); err != nil {
			return err
		}
	}
	if err := setGroupMode(f, int(tr.gid), 0o660); err != nil {
		return fmt.Errorf("admininit: hand over %s: %w", db.path, err)
	}
	id, err := fileIdentity(f)
	if err != nil {
		return fmt.Errorf("admininit: %w", err)
	}
	if err := errors.Join(f.Sync(), syncDir(filepath.Dir(db.path))); err != nil {
		return fmt.Errorf("admininit: sync %s: %w", db.path, err)
	}
	if err := step(db.create); err != nil {
		return err
	}
	at, err := pathIdentity(db.path)
	if err != nil || !id.matches(at, false) {
		return refuseDurable(ReasonIdentity, "%s no longer names the file init created: %v", db.path, errors.Join(err, mismatch(id, at)))
	}
	if err := waitPast(id.btime()); err != nil {
		return err
	}
	rec.Databases = append(rec.Databases, dbRecord{Name: db.name, Path: db.path, fileID: id})
	if err := writeIntent(adminDir, rec); err != nil {
		return err
	}
	if beforeClose != nil {
		beforeClose(db.rec)
	}
	return step(db.rec)
}

// beforeClose, when set by tests, runs after a database's record is durable and
// before init closes the descriptor it created the file with.
var beforeClose func(Step)

// beforeHandOver, when set by tests, runs after a database's O_EXCL create, or a
// resume's first look at a cut file, and before its group and mode are set; an
// error stands for a crash or a failed chown or chmod there.
var beforeHandOver func(Step) error

func mismatch(want, have fileID) error {
	if want.matches(have, false) {
		return nil
	}
	return fmt.Errorf("have %s, created %s", have, want)
}

// checkRecorded is step 9 for one database: the name still names the recorded
// file, generation included.
func checkRecorded(db database, want fileID) error {
	obs, err := openIdentity(db.path)
	if err != nil {
		return refuseDurable(ReasonIdentity, "%s %s after the database phase: %w", db.name, db.path, err)
	}
	if !obs.regular() || !want.matches(obs, useGeneration) {
		return refuseDurable(ReasonIdentity, "%s %s is not the file init-intent recorded after the database phase: have %s, recorded %s", db.name, db.path, obs, want)
	}
	return nil
}

// handOverSidecars leaves every sidecar of both databases group sori and mode
// 0660 (G4). Init fixes its own sidecars; one the server created must already
// be group sori and group read-write, which it is when the database is 0660 in
// a setgid root, because SQLite gives a new sidecar the database's mode.
func handOverSidecars(p Paths, tr trust) error {
	for _, db := range databases(p) {
		for _, name := range sidecarNames(db.path) {
			if err := handOver(name, tr); err != nil {
				return err
			}
		}
	}
	return nil
}

func handOver(name string, tr trust) (err error) {
	at, err := pathIdentity(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("admininit: %w", err)
	case !at.regular():
		return fmt.Errorf("%w: %s is not a regular file", ErrRefused, name)
	case at.UID != tr.uid:
		if at.GID != tr.gid || at.perm()&0o060 != 0o060 {
			return fmt.Errorf("%w: sidecar %s of uid %d is not read-write for group sori (%s)", ErrRefused, name, at.UID, at)
		}
		return nil
	case at.GID == tr.gid && at.perm() == 0o660:
		return nil
	}
	// #nosec G304 -- a sidecar of a database Init pinned inside the data root.
	f, err := os.OpenFile(name, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return fmt.Errorf("admininit: hand over %s: %w", name, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	held, err := fileIdentity(f)
	if err != nil || held.Dev != at.Dev || held.Ino != at.Ino {
		return fmt.Errorf("%w: sidecar %s changed during the hand-over: %v", ErrRefused, name, err)
	}
	if err := setGroupMode(f, int(tr.gid), 0o660); err != nil {
		return fmt.Errorf("admininit: hand over %s: %w", name, err)
	}
	return nil
}

func sidecarNames(path string) []string {
	out := make([]string, 0, len(sidecarSuffixes))
	for _, s := range sidecarSuffixes {
		out = append(out, path+s)
	}
	return out
}

// recordRefusal writes init-refused for a durable refusal before returning it
// (TRUST r4 §3.2). Other errors pass through. If the record cannot be written,
// both errors are returned and the refusal is not durable.
func recordRefusal(adminDir string, err error) error {
	var r *Refusal
	if !errors.As(err, &r) || !r.Durable {
		return err
	}
	at, cerr := coarseNow()
	if cerr != nil {
		return errors.Join(err, cerr)
	}
	if werr := writeAdminRecord(adminDir, RefusedName, refusedRecord{Reason: r.Reason, At: at, Detail: r.Err.Error()}); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}
