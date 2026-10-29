//go:build linux

package admininit

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// readAdminFacts is the P-a read of the admin-control area (FMT-PREP-r2 §9.1):
// epoch, keys/keyring and policy/state through the strict §2.3 readers, each
// opened without following a symlink and read-only. It takes no lock and
// writes nothing. With epoch absent nothing else is read, because the start
// row then ignores the keyring and policy/state (§9.2).
func readAdminFacts(adminDir string) adminFacts {
	var af adminFacts
	dir := adminDir
	if dir == "" {
		dir = "."
	}
	adminfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		// The epoch cannot be shown absent: treat it as present and unreadable.
		af.epochPresent = true
		af.epochErr = &TailError{Record: EpochName, Kind: TailUnreadable, Err: fmt.Errorf("open admin area: %w", err)}
		return af
	}
	defer func() { _ = unix.Close(adminfd) }()

	eb, err := readTailAt(adminfd, EpochName)
	if errors.Is(err, unix.ENOENT) {
		return af
	}
	af.epochPresent = true
	if err != nil {
		af.epochErr = &TailError{Record: EpochName, Kind: TailUnreadable, Err: err}
		return af
	}
	if af.epoch, err = parseEpoch(eb); err != nil {
		af.epochErr = err
		return af
	}
	af.keyringErr = readSubRecord(adminfd, KeysDirName, KeyringName, func(b []byte) error {
		_, err := parseKeyring(b)
		return err
	})
	af.policyErr = readSubRecord(adminfd, PolicyDirName, PolicyStateName, func(b []byte) error {
		var err error
		af.policy, err = parsePolicyState(b)
		return err
	})
	return af
}

// readSubRecord reads dir/name below adminfd, neither component followed if a
// symlink, and hands the bytes to parse.
func readSubRecord(adminfd int, dir, name string, parse func([]byte) error) error {
	dfd, err := unix.Openat(adminfd, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &TailError{Record: name, Kind: TailUnreadable, Err: fmt.Errorf("open %s/: %w", dir, err)}
	}
	defer func() { _ = unix.Close(dfd) }()
	b, err := readTailAt(dfd, name)
	if err != nil {
		return &TailError{Record: name, Kind: TailUnreadable, Err: err}
	}
	return parse(b)
}
