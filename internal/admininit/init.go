package admininit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/HeaInSeo/sori/internal/adminlock"
)

// Init runs offline init's database phase under the L3 locks (D01.3-r6 C.5.8).
//
// It creates admin.lock in adminDir, the first file init writes, and takes it
// exclusive. It then takes dataRoot/server.lock exclusive without waiting and
// holds both while the rest runs. If a server holds server.lock it returns
// adminlock.ErrServerRunning before either database is opened. Both database
// paths must resolve, through any symlinks, to files inside dataRoot, because
// server.lock guards only that directory. Keys, policy and the activation epoch
// are not written here.
//
// Under the locks it runs the data-root trust steps of SORI-D01-TRUST-r4 §3.2
// (trust.go, intent.go): a recorded refusal stands; an existing activation epoch
// in adminDir refuses as already complete, with nothing written; the data root must be on a
// filesystem with file birth times and pass the G1 preflight; a first init
// refuses any existing database or sidecar name and writes init-intent first;
// a resume checks every recorded file by its identity tuple; each absent
// database is created 0660 group sori and recorded before its schema; the
// database phase (ProvisionAndBootstrap) runs bound to the recorded tuples; and
// both names and the installation id are checked against the record at the end.
// Identity and binding refusals are recorded in init-refused, so a plain rerun
// refuses instead of adopting a replaced file.
//
// dataRoot and both database paths are resolved once, and the resolved paths
// are used for the containment check, for server.lock and for every database
// open. A symlink retargeted after the check therefore cannot move the
// databases away from the root whose server.lock Init holds.
func Init(ctx context.Context, adminDir, dataRoot string, p Paths, newID func() (string, error)) (res Result, err error) {
	if err := checkPaths(p); err != nil {
		return Result{}, err
	}
	realRoot, err := resolveRoot(dataRoot)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	var pinned Paths
	if pinned.JobLedger, err = pinInside(realRoot, dataRoot, p.JobLedger); err != nil {
		return Result{}, err
	}
	if pinned.Authority, err = pinInside(realRoot, dataRoot, p.Authority); err != nil {
		return Result{}, err
	}
	if afterCheck != nil {
		afterCheck()
	}
	locks, err := adminlock.AcquireOffline(ctx, adminDir, realRoot, true)
	if err != nil {
		return Result{}, err
	}
	defer func() { err = errors.Join(err, locks.Close()) }()
	return initLocked(ctx, adminDir, realRoot, pinned, newID)
}

// afterCheck, when set by a test, runs after the paths are checked and before
// the locks are taken and either database is opened.
var afterCheck func()

// resolveRoot returns the data root with every symlink resolved. root must exist.
func resolveRoot(root string) (string, error) {
	root, err := absRaw(root)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("data root: %w", err)
	}
	return realRoot, nil
}

// pinInside resolves path and refuses it unless it lies below realRoot.
func pinInside(realRoot, dataRoot, path string) (string, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if !inside(realRoot, resolved) {
		return "", fmt.Errorf("%w: %s is not inside the data root %s", ErrRefused, path, dataRoot)
	}
	return resolved, nil
}

// resolvePath returns the file SQLite would open at path, with every symlink
// resolved. A lexical check is not enough: with root/link pointing elsewhere,
// root/link/jobs.db and root/link/../jobs.db both name a file outside root, and
// server.lock would not guard it.
//
// An existing path is resolved whole, so a final symlink is followed and a
// dangling one is an error. For an absent path, the raw parent from rawDir is
// resolved and the final name is appended; the parent is not cleaned first,
// because the kernel follows a link before a "..". The final name is never "",
// "." or "..": with an existing parent such a path exists.
func resolvePath(path string) (string, error) {
	path, err := absRaw(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil {
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("database path: %w", err)
		}
		return realPath, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("database path: %w", err)
	}
	dir := rawDir(path)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("database directory: %w", err)
	}
	return filepath.Join(realDir, path[len(dir):]), nil
}

// absRaw makes path absolute without cleaning it, unlike filepath.Abs, which
// would drop "link/.." before the link is resolved.
func absRaw(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd + string(filepath.Separator) + path, nil
}

// inside reports whether path names an entry below root, compared lexically.
func inside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
