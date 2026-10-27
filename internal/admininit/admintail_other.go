//go:build !linux

package admininit

import (
	"context"
	"errors"
)

// The admin tail needs the Linux descriptor operations of tailfs_linux.go. On
// other platforms Init refuses at step 4a (identitySupported is false) before
// any of these run.

func preflightAdminArea(string, uint32) error { return nil }

func writeAdminTail(context.Context, string, string, Paths, trust, string) error {
	return errNoIdentity
}

// Status is the read-only readback of a completed init.
type Status struct {
	InstallationID  string
	ActivationEpoch uint64
	CutCounter      uint64
	HostID          string
	CurrentKeyID    string
	Keys            []StatusKey
}

// StatusKey is a key of the keyring without its secret.
type StatusKey struct {
	KeyID string
	State string
}

// ErrNotInitialized is returned by ReadStatus when epoch is absent.
var ErrNotInitialized = errors.New("admininit: init has not completed: epoch is absent")

// ReadStatus is not supported on this platform.
func ReadStatus(context.Context, string) (Status, error) { return Status{}, errNoIdentity }
