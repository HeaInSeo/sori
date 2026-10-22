//go:build !linux

package admininit

import (
	"errors"
	"os"
)

// identitySupported is false: the T2 identity tuple needs statx birth times,
// which only the Linux build reads. Init refuses with
// ErrUnsupportedFileIdentity before it writes anything (TRUST r4 §3.1).
const identitySupported = false

var (
	errNoIdentity = errors.New("file identity is not supported on this platform")
	errLoop       = errors.New("symlink at the final name")
)

func fileIdentity(*os.File) (fileID, error) { return fileID{}, errNoIdentity }

func pathIdentity(string) (fileID, error) { return fileID{}, errNoIdentity }

func openIdentity(string) (fileID, error) { return fileID{}, errNoIdentity }

func coarseNow() (stamp, error) { return stamp{}, errNoIdentity }

func waitPast(stamp) error { return errNoIdentity }

func setGroupMode(*os.File, int, os.FileMode) error { return errNoIdentity }

func checkTrustedDir(string, trust) error { return errNoIdentity }

func checkAncestors(string) error { return errNoIdentity }

func fileACL(string) error { return errNoIdentity }

func syncDir(string) error { return errNoIdentity }

func createExcl(string) (*os.File, error) { return nil, errNoIdentity }

func retryableIO(error) bool { return false }
