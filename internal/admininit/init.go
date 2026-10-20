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
// holds both while ProvisionAndBootstrap runs. If a server holds server.lock it
// returns adminlock.ErrServerRunning before either database is opened. Both
// database paths must resolve, through any symlinks, to files inside dataRoot,
// because server.lock guards only that directory. Keys, policy and the
// activation epoch are not written here.
func Init(ctx context.Context, adminDir, dataRoot string, p Paths, newID func() (string, error)) (res Result, err error) {
	if err := checkPaths(p); err != nil {
		return Result{}, err
	}
	for _, path := range []string{p.JobLedger, p.Authority} {
		ok, err := resolvedInside(dataRoot, path)
		if err != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrRefused, err)
		}
		if !ok {
			return Result{}, fmt.Errorf("%w: %s is not inside the data root %s", ErrRefused, path, dataRoot)
		}
	}
	locks, err := adminlock.AcquireOffline(ctx, adminDir, dataRoot, true)
	if err != nil {
		return Result{}, err
	}
	defer func() { err = errors.Join(err, locks.Close()) }()
	return ProvisionAndBootstrap(ctx, p, newID)
}

// resolvedInside reports whether the file SQLite would open at path lies below
// root once symlinks are resolved. A lexical check is not enough: with
// root/link pointing elsewhere, root/link/jobs.db and root/link/../jobs.db
// both name a file outside root, and server.lock would not guard it.
//
// root must exist. An existing path is resolved whole, so a final symlink is
// followed and a dangling one is an error. For an absent path, the raw parent
// from rawDir is resolved and the final name is appended; the parent is not
// cleaned first, because the kernel follows a link before a "..". The final
// name is never "", "." or "..": with an existing parent such a path exists.
func resolvedInside(root, path string) (bool, error) {
	root, err := absRaw(root)
	if err != nil {
		return false, err
	}
	if path, err = absRaw(path); err != nil {
		return false, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, fmt.Errorf("data root: %w", err)
	}
	var realPath string
	if _, err := os.Lstat(path); err == nil {
		if realPath, err = filepath.EvalSymlinks(path); err != nil {
			return false, fmt.Errorf("database path: %w", err)
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		dir := rawDir(path)
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return false, fmt.Errorf("database directory: %w", err)
		}
		realPath = filepath.Join(realDir, path[len(dir):])
	} else {
		return false, fmt.Errorf("database path: %w", err)
	}
	return inside(realRoot, realPath), nil
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
