// Package admininit holds the offline init steps of `sori-admin init` (RW-C1 §5).
//
// This file is the database phase (RW-C1 §5.1/§5.1a): it classifies the job ledger
// and the authority database read-only, provisions their known schemas, and then
// writes the job ledger's four init markers once. ProvisionAndBootstrap does not
// take the admin or server lock; Init (init.go) runs it while holding both. Neither
// creates keys, policy or the activation epoch.
package admininit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/HeaInSeo/sori/authority/sqlitestore"
	"github.com/HeaInSeo/sori/jobledger"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ErrRefused is returned when either database is not in a state offline init may
// write to. Nothing has been written when it is returned from the preflight.
var ErrRefused = errors.New("admininit: init refused")

// Paths names the two databases of one data root. They are separate files; no
// transaction covers both.
type Paths struct {
	JobLedger string
	Authority string
}

// Result reports the database phase. Bootstrapped is false when the job ledger
// already had the init markers and Init was skipped.
type Result struct {
	InstallationID string
	Bootstrapped   bool
}

// Step names a point between two durable writes of ProvisionAndBootstrap.
type Step string

// The durable steps of ProvisionAndBootstrap, in order.
const (
	StepAuthoritySchema Step = "authority-schema"
	StepJobLedgerSchema Step = "jobledger-schema"
	StepMarkers         Step = "markers"
)

// afterStep, when set by tests, runs after each durable step. A non-nil error
// stops the run there, as a crash would.
var afterStep func(Step) error

// NewInstallationID returns a new installation id: 16 bytes from the OS CSPRNG as
// 32 lowercase hex digits (RW-C1 §5.1).
func NewInstallationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("admininit: installation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ProvisionAndBootstrap runs the database phase of offline init (RW-C1 §5.1a).
//
// It first inspects both databases read-only and refuses, writing nothing, unless
// the job ledger is PRISTINE, SCHEMA_ONLY or INIT_MARKER_EMPTY and the authority
// database is PRISTINE or SCHEMA_ONLY. A marker-empty ledger next to a pristine
// authority database is refused too: the markers are written only after both
// schemas exist, so that pair means the authority file was removed or replaced.
//
// It then creates each missing known schema through the version-pinned Open and
// checks that the result is SCHEMA_ONLY. Last, unless the ledger is already
// INIT_MARKER_EMPTY, it chooses the installation id with newID and writes the
// markers in Init's single transaction. A rerun after a crash at any step finds
// PRISTINE, SCHEMA_ONLY or INIT_MARKER_EMPTY again and resumes; after the marker
// commit it reuses the recorded id and never calls newID.
//
// Each database file is held open from its first open here, and an open is
// refused once a path stops naming its held file or turns into a symlink, or a
// sidecar is not a regular file (boundFiles).
//
// newID nil means NewInstallationID.
func ProvisionAndBootstrap(ctx context.Context, p Paths, newID func() (string, error)) (res Result, err error) {
	if newID == nil {
		newID = NewInstallationID
	}
	if err := checkPaths(p); err != nil {
		return Result{}, err
	}
	ledger, auth, err := preflight(ctx, p)
	if err != nil {
		return Result{}, err
	}
	if ledger.State == jobledger.InitMarkerEmpty {
		return Result{InstallationID: ledger.InstallationID}, nil
	}
	var files boundFiles
	defer func() { err = errors.Join(err, files.close()) }()

	if auth == sqlitestore.InitPristine {
		if err := files.open(StepAuthoritySchema, p.Authority, func() error { return provisionAuthority(ctx, p.Authority) }); err != nil {
			return Result{}, err
		}
		if err := step(StepAuthoritySchema); err != nil {
			return Result{}, err
		}
	}
	if ledger.State == jobledger.InitPristine {
		if err := files.open(StepJobLedgerSchema, p.JobLedger, func() error { return provisionJobLedger(ctx, p.JobLedger) }); err != nil {
			return Result{}, err
		}
		if err := step(StepJobLedgerSchema); err != nil {
			return Result{}, err
		}
	}

	id, err := newID()
	if err != nil {
		return Result{}, err
	}
	if err := files.open(StepMarkers, p.JobLedger, func() error { return writeMarkers(ctx, p.JobLedger, id) }); err != nil {
		return Result{}, err
	}
	if err := step(StepMarkers); err != nil {
		return Result{}, err
	}
	return Result{InstallationID: id, Bootstrapped: true}, nil
}

// beforeOpen, when set by tests, runs before each durable step opens a
// database.
var beforeOpen func(Step)

// boundFiles holds each database file open from its first open by
// ProvisionAndBootstrap until it returns. SQLite opens a database by path, and
// the preflight only checks the paths: a writer of the directory could put a
// symlink, or another file, at a final name afterwards, and SQLite would follow
// it while server.lock guards only the data root. Each open is therefore
// checked against the held files before and after it runs.
type boundFiles []boundFile

type boundFile struct {
	path string
	f    *os.File
}

// open binds path unless it is bound, then runs fn, which opens the database by
// path, between two checks.
func (files *boundFiles) open(s Step, path string, fn func() error) error {
	if beforeOpen != nil {
		beforeOpen(s)
	}
	if !slices.ContainsFunc(*files, func(b boundFile) bool { return b.path == path }) {
		f, err := bind(path)
		if err != nil {
			return err
		}
		*files = append(*files, boundFile{path: path, f: f})
	}
	if err := files.check(); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return files.check()
}

// bind opens the database file at path. An absent file is created empty with
// O_EXCL, which fails if anything, a symlink too, appeared at the name after the
// preflight; an empty regular file is PRISTINE again, so a rerun after a crash
// resumes. check then refuses an existing file that is a symlink.
func bind(path string) (*os.File, error) {
	// #nosec G304 -- the database path the preflight checked inside the data root.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// #nosec G304 -- as above; check refuses it unless it is a regular file.
		f, err = os.Open(path)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: bind %s: %w", ErrRefused, path, err)
	}
	return f, nil
}

// check refuses unless each path still names its held file through no symlink,
// and none of its SQLite sidecar files is anything but a regular file.
func (files boundFiles) check() error {
	for _, b := range files {
		fi, err := os.Lstat(b.path)
		if err != nil {
			return fmt.Errorf("%w: database %s: %w", ErrRefused, b.path, err)
		}
		held, err := b.f.Stat()
		if err != nil {
			return fmt.Errorf("%w: database %s: %w", ErrRefused, b.path, err)
		}
		if !fi.Mode().IsRegular() || !os.SameFile(fi, held) {
			return fmt.Errorf("%w: %s is no longer the database file init bound", ErrRefused, b.path)
		}
		for _, s := range sidecarSuffixes {
			fi, err := os.Lstat(b.path + s)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return fmt.Errorf("%w: %s: %w", ErrRefused, b.path+s, err)
			case !fi.Mode().IsRegular():
				return fmt.Errorf("%w: %s is not a regular file (%s)", ErrRefused, b.path+s, fi.Mode().Type())
			}
		}
	}
	return nil
}

func (files boundFiles) close() error {
	var err error
	for _, b := range files {
		err = errors.Join(err, b.f.Close())
	}
	return err
}

func step(s Step) error {
	if afterStep == nil {
		return nil
	}
	return afterStep(s)
}

// sidecarSuffixes name the files SQLite keeps next to a database: the WAL, its
// shared-memory index and the rollback journal.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// checkPaths refuses an empty path, two paths that name one database file, and
// a database that is one of the other database's SQLite sidecar files. Opening
// the database that owns the sidecar would treat the other database as a stale
// WAL or journal and remove or overwrite it.
func checkPaths(p Paths) error {
	if p.JobLedger == "" || p.Authority == "" {
		return fmt.Errorf("%w: database path not set", ErrRefused)
	}
	same, err := sameFile(p.JobLedger, p.Authority)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if same {
		return fmt.Errorf("%w: job ledger %s and authority %s are one file", ErrRefused, p.JobLedger, p.Authority)
	}
	for _, s := range sidecarSuffixes {
		for _, pair := range [][2]string{{p.Authority, p.JobLedger + s}, {p.JobLedger, p.Authority + s}} {
			same, err := sameFile(pair[0], pair[1])
			if err != nil {
				return fmt.Errorf("%w: %w", ErrRefused, err)
			}
			if same {
				return fmt.Errorf("%w: database %s is the SQLite file %s", ErrRefused, pair[0], pair[1])
			}
		}
	}
	return nil
}

// sameFile reports whether a and b name one file, also through a hard link or a
// symlinked directory. Two existing files are compared by identity; when neither
// exists, their directories are compared by identity and their names by
// sameName, so that a case- or normalization-insensitive volume, or Win32
// trailing dot and space stripping, cannot alias two absent names. On a volume that tells such names apart this refuses them,
// which is conservative. One existing and one absent path are different files.
//
// The directories are the raw parents from rawDir, not filepath.Dir: Dir cleans
// "link/.." away lexically, while SQLite opens the raw path and the kernel
// follows the link before the "..".
func sameFile(a, b string) (bool, error) {
	ai, aerr := statIfExists(a)
	bi, berr := statIfExists(b)
	if err := errors.Join(aerr, berr); err != nil {
		return false, err
	}
	switch {
	case ai != nil && bi != nil:
		return os.SameFile(ai, bi), nil
	case ai != nil || bi != nil:
		return false, nil
	case !sameName(filepath.Base(a), filepath.Base(b)):
		return false, nil
	}
	ad, aerr := os.Stat(rawDir(a))
	bd, berr := os.Stat(rawDir(b))
	if err := errors.Join(aerr, berr); err != nil {
		// A missing directory leaves the database absent; Open would fail later.
		if errors.Is(aerr, fs.ErrNotExist) || errors.Is(berr, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(ad, bd), nil
}

// sameName reports whether two file names may name one file: their nameKeys are
// equal. A default macOS APFS volume treats a composed "é" and "e" plus a
// combining accent as one name, and is case-insensitive under full Unicode
// folding, so "ß.db" and "ss.db" can name one file. A Win32 path open drops
// trailing dots and spaces, so "jobs.db" and "jobs.db." name one file there.
func sameName(a, b string) bool {
	return nameKey(a) == nameKey(b)
}

// nameKey drops trailing '.' and ' ' as a Win32 open does, then applies Unicode
// canonical caseless matching: NFD, full case folding, and normalization again.
// strings.EqualFold only applies simple folding and keeps "ß" and "ss" apart. The
// key is applied on every OS; names it joins are refused even on a volume that
// tells them apart, which is conservative.
func nameKey(name string) string {
	name = strings.TrimRight(name, ". ")
	return norm.NFC.String(cases.Fold().String(norm.NFD.String(name)))
}

// rawDir returns path up to and including its last separator, without lexical
// cleaning, or the volume name plus "." when path has no separator after it.
// On Windows both '/' and '\' are separators, so "C:/data/jobs.db" and
// "C:\data\JOBS.db" both have a parent in C:\data.
func rawDir(path string) string {
	return rawDirOf(path, filepath.VolumeName(path), os.IsPathSeparator)
}

func rawDirOf(path, volume string, isSep func(uint8) bool) string {
	for i := len(path) - 1; i >= len(volume); i-- {
		if isSep(path[i]) {
			return path[:i+1]
		}
	}
	return volume + "."
}

// statIfExists returns nil info and nil error for an absent path.
func statIfExists(path string) (fs.FileInfo, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return fi, err
}

// preflight classifies both databases before either is written.
func preflight(ctx context.Context, p Paths) (jobledger.InitInspection, sqlitestore.InitState, error) {
	ledger, err := jobledger.InspectInitState(ctx, p.JobLedger)
	if err != nil {
		return ledger, sqlitestore.InitRefused, fmt.Errorf("%w: job ledger: %w", ErrRefused, err)
	}
	auth, err := sqlitestore.InspectInitState(ctx, p.Authority)
	if err != nil {
		return ledger, auth, fmt.Errorf("%w: authority: %w", ErrRefused, err)
	}
	if ledger.State == jobledger.InitMarkerEmpty && auth != sqlitestore.InitSchemaOnly {
		return ledger, auth, fmt.Errorf("%w: job ledger has init markers but authority is %s", ErrRefused, auth)
	}
	return ledger, auth, nil
}

func provisionAuthority(ctx context.Context, path string) error {
	s, err := sqlitestore.Open(path)
	if err != nil {
		return fmt.Errorf("admininit: provision authority: %w", err)
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("admininit: provision authority: %w", err)
	}
	state, err := sqlitestore.InspectInitState(ctx, path)
	if err != nil {
		return fmt.Errorf("admininit: provision authority: %w", err)
	}
	if state != sqlitestore.InitSchemaOnly {
		return fmt.Errorf("%w: authority is %s after provisioning", ErrRefused, state)
	}
	return nil
}

func provisionJobLedger(ctx context.Context, path string) error {
	s, err := jobledger.Open(path)
	if err != nil {
		return fmt.Errorf("admininit: provision job ledger: %w", err)
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("admininit: provision job ledger: %w", err)
	}
	got, err := jobledger.InspectInitState(ctx, path)
	if err != nil {
		return fmt.Errorf("admininit: provision job ledger: %w", err)
	}
	if got.State != jobledger.InitSchemaOnly {
		return fmt.Errorf("%w: job ledger is %s after provisioning", ErrRefused, got.State)
	}
	return nil
}

// writeMarkers runs Init's marker transaction and checks the result read-only.
func writeMarkers(ctx context.Context, path, id string) error {
	s, err := jobledger.Open(path)
	if err != nil {
		return fmt.Errorf("admininit: init job ledger: %w", err)
	}
	initErr := s.Init(ctx, id)
	if err := errors.Join(initErr, s.Close()); err != nil {
		return fmt.Errorf("admininit: init job ledger: %w", err)
	}
	got, err := jobledger.InspectInitState(ctx, path)
	if err != nil {
		return fmt.Errorf("admininit: init job ledger: %w", err)
	}
	if got.State != jobledger.InitMarkerEmpty || got.InstallationID != id {
		return fmt.Errorf("%w: job ledger is %s/%q after init", ErrRefused, got.State, got.InstallationID)
	}
	return nil
}
