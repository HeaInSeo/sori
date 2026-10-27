//go:build linux

package admininit

// This file wires the admin tail into offline init (FMT-PREP-r2 §6, §7, §5.2;
// RW-C1 §5.2): the step-4 admin-area preflight, step 10 after the installation
// id is recorded (10a keys/keyring, 10b policy/state, 10c epoch durably last),
// the §5.2 pre-write check, and the read-only Status.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HeaInSeo/sori/internal/adminlock"
	"github.com/HeaInSeo/sori/jobledger"
)

// maxTailRecord bounds a tail record read (FMT-PREP-r2 §2.3 reader item 1).
const maxTailRecord = 64 << 10

// topWhitelisted reports whether name may exist directly in the admin area
// before epoch (FMT-PREP-r2 §7 top-level whitelist, C.5.7 r5 condition 1).
// keys and policy are checked separately.
func topWhitelisted(name string) bool {
	switch name {
	case adminlock.AdminLockName, IntentName, "." + IntentName + ".tmp",
		RefusedName, "." + RefusedName + ".tmp", "." + EpochName + ".tmp":
		return true
	}
	return strings.HasPrefix(name, "."+adminlock.AdminLockName+".tmp-")
}

// preflightAdminArea is the step-4 admin-tail preflight (FMT-PREP-r2 §7): the
// admin area holds only names init creates (ADMIN_AREA_NOT_EMPTY otherwise),
// and keys/ and policy/, if present, are directories owned by euid, not
// symlinks (ADMIN_AREA_INVALID otherwise). Group, mode and contents of keys/
// and policy/ are left to the §2.2 converge rule. Nothing is written.
func preflightAdminArea(adminDir string, euid uint32) error {
	dir := adminDir
	if dir == "" {
		dir = "."
	}
	f, err := os.Open(dir) // #nosec G304 -- the configured admin area.
	if err != nil {
		return fmt.Errorf("admininit: open admin area: %w", err)
	}
	names, err := f.Readdirnames(-1)
	if err := errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("admininit: list admin area: %w", err)
	}
	for _, n := range names {
		if n == KeysDirName || n == PolicyDirName {
			var st unix.Stat_t
			if err := unix.Lstat(adminPath(adminDir, n), &st); err != nil {
				return fmt.Errorf("admininit: stat %s: %w", n, err)
			}
			if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != euid {
				return invalid("%s is not a directory owned by uid %d (mode %#o, uid %d)", n, euid, st.Mode, st.Uid)
			}
			continue
		}
		if !topWhitelisted(n) {
			return refuse(ReasonAdminAreaNotEmpty, "the admin area holds %q, which init does not create; remove it and rerun", n)
		}
	}
	return nil
}

// Steps of the admin tail, seen by the afterStep hook once each is durable.
const (
	StepKeyring     Step = "keyring"      // 10a durable
	StepPolicyState Step = "policy-state" // 10b durable
	StepEpoch       Step = "epoch"        // 10c durable
)

// tailNow is the clock of the tail records. Tests replace it.
var tailNow = time.Now

// writeAdminTail is step 10 (FMT-PREP-r2 §6): 10a the keys/ converge and a
// newly generated keyring, 10b the policy/ converge and the deny-all
// policy/state for I, then the §5.2 pre-write check and 10c epoch, each fully
// durable before the next. It runs under both L3 locks after finish recorded
// I in init-intent. A rerun runs all of it again: the keyring of an earlier
// attempt is replaced and never read (§3.2).
func writeAdminTail(ctx context.Context, adminDir, hostID string, p Paths, tr trust, id string) (err error) {
	dir := adminDir
	if dir == "" {
		dir = "."
	}
	adminfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("admininit: open admin area: %w", err)
	}
	defer func() { _ = unix.Close(adminfd) }()
	now := tailNow()

	// 10a.
	kfd, err := convergeSubdir(adminfd, KeysDirName, tr.uid, tr.gid)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(kfd) }()
	kb, err := newKeyringBytes(now)
	if err != nil {
		return err
	}
	if err := writeTailFile(kfd, KeyringName, kb, tr.gid); err != nil {
		return err
	}
	if err := step(StepKeyring); err != nil {
		return err
	}

	// 10b.
	pfd, err := convergeSubdir(adminfd, PolicyDirName, tr.uid, tr.gid)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(pfd) }()
	ps, err := newDefaultPolicyState(id, int(tr.uid), now)
	if err != nil {
		return err
	}
	pb, err := encodePolicyState(ps)
	if err != nil {
		return err
	}
	if err := writeTailFile(pfd, PolicyStateName, pb, tr.gid); err != nil {
		return err
	}
	if err := step(StepPolicyState); err != nil {
		return err
	}

	// §5.2, then 10c.
	if err := preEpochCheck(ctx, adminDir, adminfd, kfd, pfd, p, id); err != nil {
		return err
	}
	act, err := randomHex16()
	if err != nil {
		return fmt.Errorf("admininit: activation id: %w", err)
	}
	eb, err := encodeEpoch(Epoch{
		InstallationID: id, ActivationEpoch: 1, CutCounter: 0,
		HostID: hostID, ActivationID: act, StartedAt: tailTime(now),
	})
	if err != nil {
		return err
	}
	if err := writeTailFile(adminfd, EpochName, eb, tr.gid); err != nil {
		return err
	}
	return step(StepEpoch)
}

// newKeyringBytes generates the init keyring (FMT-PREP-r2 §3.2): exactly one
// new ACTIVE key from the OS CSPRNG, current.
func newKeyringBytes(now time.Time) ([]byte, error) {
	kid, err := randomHex16()
	if err != nil {
		return nil, fmt.Errorf("admininit: key id: %w", err)
	}
	secret := make([]byte, macSecretLen)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("admininit: key secret: %w", err)
	}
	keyID := macKeyIDPrefix + kid
	return encodeKeyring(Keyring{
		CurrentKeyID: keyID,
		Keys:         []MACKey{{KeyID: keyID, Secret: secret, State: keyStateActive, CreatedAt: tailTime(now)}},
	})
}

func randomHex16() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// preEpochCheck is the FMT-PREP-r2 §5.2 pre-write check. Any failure refuses
// ADMIN_TAIL_INVALID (not durable) and epoch is not written.
func preEpochCheck(ctx context.Context, adminDir string, adminfd, kfd, pfd int, p Paths, id string) error {
	bad := func(format string, args ...any) error {
		return refuse(ReasonAdminTailInvalid, format, args...)
	}
	var st unix.Stat_t
	switch err := unix.Fstatat(adminfd, EpochName, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		return bad("%s appeared before init wrote it", EpochName)
	case !errors.Is(err, unix.ENOENT):
		return fmt.Errorf("admininit: stat %s: %w", EpochName, err)
	}
	kb, err := readTailAt(kfd, KeyringName)
	if err != nil {
		return bad("re-read %s/%s: %w", KeysDirName, KeyringName, err)
	}
	if _, err := parseKeyring(kb); err != nil {
		return bad("%w", err)
	}
	pb, err := readTailAt(pfd, PolicyStateName)
	if err != nil {
		return bad("re-read %s/%s: %w", PolicyDirName, PolicyStateName, err)
	}
	ps, err := parsePolicyState(pb)
	if err != nil {
		return bad("%w", err)
	}
	docID, err := policyDocumentInstallation(ps.Document)
	if err != nil {
		return bad("%w", err)
	}
	if docID != id {
		return bad("policy document names installation %q, init has %q", docID, id)
	}
	rec, found, err := readIntent(adminDir)
	if err != nil || !found {
		return bad("re-read %s: found=%v: %v", IntentName, found, err)
	}
	if rec.InstallationID != id {
		return bad("%s records installation %q, init has %q", IntentName, rec.InstallationID, id)
	}
	ins, err := jobledger.InspectInitState(ctx, p.JobLedger)
	if err != nil {
		return bad("re-inspect job ledger: %w", err)
	}
	if ins.State != jobledger.InitMarkerEmpty || ins.InstallationID != id {
		return bad("job ledger is %s{%s}, want %s{%s}", ins.State, ins.InstallationID, jobledger.InitMarkerEmpty, id)
	}
	return nil
}

// readTailAt reads name in dfd without following a symlink, at most
// maxTailRecord bytes; a regular file only.
func readTailAt(dfd int, name string) ([]byte, error) {
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name) // #nosec G115 -- a file descriptor is non-negative.
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTailRecord+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxTailRecord {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxTailRecord)
	}
	return b, nil
}

// Status is the read-only readback of a completed init (FMT-PREP-r2 §6 C6g,
// §3.3). It never contains a key secret.
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

// ReadStatus reads epoch and keys/keyring through the strict readers while it
// holds admin.lock shared, and writes nothing. With epoch absent it returns
// ErrNotInitialized. A tail record that fails its reader is returned as a
// *TailError.
func ReadStatus(ctx context.Context, adminDir string) (st Status, err error) {
	lock, err := adminlock.AcquireAdminShared(ctx, adminDir)
	if err != nil {
		return Status{}, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	dir := adminDir
	if dir == "" {
		dir = "."
	}
	adminfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Status{}, fmt.Errorf("admininit: open admin area: %w", err)
	}
	defer func() { _ = unix.Close(adminfd) }()
	eb, err := readTailAt(adminfd, EpochName)
	if errors.Is(err, unix.ENOENT) {
		return Status{}, ErrNotInitialized
	}
	if err != nil {
		return Status{}, &TailError{Record: EpochName, Kind: TailUnreadable, Err: err}
	}
	e, err := parseEpoch(eb)
	if err != nil {
		return Status{}, err
	}
	kfd, err := unix.Openat(adminfd, KeysDirName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Status{}, &TailError{Record: KeyringName, Kind: TailUnreadable, Err: err}
	}
	defer func() { _ = unix.Close(kfd) }()
	kb, err := readTailAt(kfd, KeyringName)
	if err != nil {
		return Status{}, &TailError{Record: KeyringName, Kind: TailUnreadable, Err: err}
	}
	k, err := parseKeyring(kb)
	if err != nil {
		return Status{}, err
	}
	st = Status{
		InstallationID: e.InstallationID, ActivationEpoch: e.ActivationEpoch,
		CutCounter: e.CutCounter, HostID: e.HostID, CurrentKeyID: k.CurrentKeyID,
	}
	for _, key := range k.Keys {
		st.Keys = append(st.Keys, StatusKey{KeyID: key.KeyID, State: key.State})
	}
	return st, nil
}
