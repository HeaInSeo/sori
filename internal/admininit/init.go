package admininit

import (
	"context"
	"errors"
	"fmt"
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
// database paths must be inside dataRoot, because server.lock guards only that
// directory. Keys, policy and the activation epoch are not written here.
func Init(ctx context.Context, adminDir, dataRoot string, p Paths, newID func() (string, error)) (res Result, err error) {
	if err := checkPaths(p); err != nil {
		return Result{}, err
	}
	for _, path := range []string{p.JobLedger, p.Authority} {
		if !inside(dataRoot, path) {
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

// inside reports whether path names an entry below root, compared lexically.
func inside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
