package admininit

// This file is the data-root trust part of offline init, from SORI-D01-TRUST-r4
// as accepted with layout L-A, option (a) and T2:
//
//   - I-T1, G1/G2 preflight: the data root is owned by the init principal
//     (sori-admin), group sori, mode exactly 2770 without sticky and without
//     extended or default ACLs; every ancestor is owned by root and not
//     writable by group, other or an ACL entry.
//   - I-T2, installation identity: <admin>/init-intent records the data root and
//     each database init created by an identity tuple with a mandatory birth
//     time, and <admin>/init-refused makes an identity or binding refusal stand
//     until an operator clears it. A plain rerun never adopts a file this init
//     did not create.
//   - I-G4, hand-over: init creates both databases 0660 group sori and leaves
//     every sidecar 0660 group sori, so the server, which runs as sori, can open
//     them.
//
// P2′ (any process as uid sori) is in the server's trust domain and P3 (other
// members of group sori) is excluded by the deployment profile. Same-uid
// in-place writes and held descriptors are outside the model (TRUST r4 §3.5).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
)

// Names of the T2 records in the admin area.
const (
	IntentName  = "init-intent"
	RefusedName = "init-refused"
)

// EpochName is the activation epoch in the admin area, written last by a
// complete init (D01.3-r6 C.5.7 r5). Init only checks whether the name exists.
const EpochName = "epoch"

// Refusal reasons. A durable reason is recorded in init-refused before Init
// returns it, and every later run refuses with ReasonRefusedStands.
const (
	// Not durable: nothing was attempted, or the operator can fix and rerun.
	ReasonRefusedStands       = "INIT_REFUSED_STANDS"
	ReasonAlreadyComplete     = "INIT_ALREADY_COMPLETE"
	ReasonUnsupportedIdentity = "UNSUPPORTED_FILE_IDENTITY"
	ReasonUntrustedDataRoot   = "UNTRUSTED_DATA_ROOT"
	ReasonIntentPaths         = "INTENT_PATHS_DIFFER"

	// Durable.
	ReasonPreexisting    = "PREEXISTING_DATABASE"
	ReasonDataRoot       = "DATA_ROOT_MISMATCH"
	ReasonRemoved        = "DATABASE_REMOVED"
	ReasonIdentity       = "IDENTITY_MISMATCH"
	ReasonUnrecorded     = "UNRECORDED_FILE"
	ReasonBinding        = "BINDING_REFUSED"
	ReasonInstallationID = "INSTALLATION_ID_MISMATCH"
)

// Refusal is an init refusal with its reason. It matches ErrRefused.
type Refusal struct {
	Reason  string
	Durable bool
	Err     error
}

func (r *Refusal) Error() string { return fmt.Sprintf("%s: %s: %v", ErrRefused, r.Reason, r.Err) }

func (r *Refusal) Unwrap() []error { return []error{ErrRefused, r.Err} }

func refuse(reason string, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Err: fmt.Errorf(format, args...)}
}

func refuseDurable(reason string, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Durable: true, Err: fmt.Errorf(format, args...)}
}

// errNoBirthTime: the filesystem does not report a birth time (statx
// STATX_BTIME), so no identity tuple can be built.
var errNoBirthTime = errors.New("the filesystem does not report a birth time")

// stamp is a realtime instant: seconds and nanoseconds.
type stamp struct {
	Sec  int64  `json:"sec"`
	Nsec uint32 `json:"nsec"`
}

func (s stamp) after(t stamp) bool {
	return s.Sec > t.Sec || (s.Sec == t.Sec && s.Nsec > t.Nsec)
}

func (s stamp) String() string { return fmt.Sprintf("%d.%09d", s.Sec, s.Nsec) }

// fileID is the identity tuple of TRUST r4 §3.1. Nlink and Size are observed,
// not recorded: a match needs Nlink == 1, and the C2 cut needs Size == 0.
type fileID struct {
	Dev        uint64  `json:"dev"`
	Ino        uint64  `json:"ino"`
	BtimeSec   int64   `json:"btime_sec"`
	BtimeNsec  uint32  `json:"btime_nsec"`
	Generation *uint32 `json:"generation,omitempty"`
	UID        uint32  `json:"uid"`
	GID        uint32  `json:"gid"`
	Mode       uint32  `json:"mode"`
	Nlink      uint32  `json:"-"`
	Size       uint64  `json:"-"`
}

func (id fileID) btime() stamp { return stamp{Sec: id.BtimeSec, Nsec: id.BtimeNsec} }

// regular reports whether the tuple's mode is a regular file (S_IFREG).
func (id fileID) regular() bool { return id.Mode&0o170000 == 0o100000 }

// perm returns the tuple's permission and special bits.
func (id fileID) perm() uint32 { return id.Mode & 0o7777 }

// matches reports whether observed is the file rec recorded: every tuple field
// equal, the generation too when rec has one, and exactly one link. A generation
// recorded but not readable now is a mismatch.
func (rec fileID) matches(observed fileID, withGeneration bool) bool {
	if rec.Dev != observed.Dev || rec.Ino != observed.Ino || rec.btime() != observed.btime() ||
		rec.UID != observed.UID || rec.GID != observed.GID || rec.Mode != observed.Mode || observed.Nlink != 1 {
		return false
	}
	if withGeneration && rec.Generation != nil {
		return observed.Generation != nil && *observed.Generation == *rec.Generation
	}
	return true
}

func (id fileID) String() string {
	gen := "none"
	if id.Generation != nil {
		gen = strconv.FormatUint(uint64(*id.Generation), 10)
	}
	return fmt.Sprintf("dev=%d ino=%d btime=%s generation=%s uid=%d gid=%d mode=%o nlink=%d size=%d",
		id.Dev, id.Ino, id.btime(), gen, id.UID, id.GID, id.Mode, id.Nlink, id.Size)
}

// rootID identifies the data root directory.
type rootID struct {
	Path      string `json:"path"`
	Dev       uint64 `json:"dev"`
	Ino       uint64 `json:"ino"`
	BtimeSec  int64  `json:"btime_sec"`
	BtimeNsec uint32 `json:"btime_nsec"`
}

// dbRecord is one database init created.
type dbRecord struct {
	Name string `json:"name"`
	Path string `json:"path"`
	fileID
}

// intent is the init-intent record (TRUST r4 §3.1, with P3-TR6's reference
// time).
type intent struct {
	Version int    `json:"version"`
	Nonce   string `json:"intent_nonce"`
	// CreatedAt is CLOCK_REALTIME_COARSE read just before the first-init absence
	// check, the clock the kernel stamps birth times from. A file the C2 cut
	// accepts must have a birth time at or after it.
	CreatedAt      stamp      `json:"intent_created_at"`
	DataRoot       rootID     `json:"data_root"`
	Databases      []dbRecord `json:"databases"`
	InstallationID string     `json:"installation_id,omitempty"`
}

const intentVersion = 1

func (in *intent) find(name string) *dbRecord {
	for i := range in.Databases {
		if in.Databases[i].Name == name {
			return &in.Databases[i]
		}
	}
	return nil
}

// refusedRecord is the init-refused record.
type refusedRecord struct {
	Reason   string `json:"reason"`
	At       stamp  `json:"at"`
	Detail   string `json:"detail"`
	Observed string `json:"observed,omitempty"`
}

// trust is the expected principal and group of the data root: the init
// principal's uid and the gid of group sori.
type trust struct {
	uid, gid uint32
}

// TrustGroup is the group of the data root, the databases and their sidecars.
const TrustGroup = "sori"

// lookupTrust returns the init principal (this process's effective uid) and
// group sori. Tests replace it.
var lookupTrust = func() (trust, error) {
	g, err := user.LookupGroup(TrustGroup)
	if err != nil {
		return trust{}, fmt.Errorf("group %s: %w", TrustGroup, err)
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return trust{}, fmt.Errorf("group %s: gid %q: %w", TrustGroup, g.Gid, err)
	}
	return trust{uid: uint32(os.Geteuid()), gid: uint32(gid)}, nil // #nosec G115 -- a uid fits 32 bits.
}

// Test overrides; production uses the defaults. useGeneration false makes the
// identity tuple ignore the inode generation, as on ext4, where a file's owner
// can set it (TRUST r4 §3.1). ancestorOwner and ancestorStop bound the G1
// ancestor check, which in production wants root and ends after "/"; a test
// stops it below ancestorStop.
var (
	useGeneration        = true
	ancestorOwner uint32 = 0
	ancestorStop         = ""
)

// onTickWait, when set by tests, sees each clock reading of the step-7 wait for
// birth time t and may replace it, to hold the clock on t's tick.
var onTickWait func(t, now stamp) stamp

// adminPath appends name to the admin area without cleaning it, as adminlock
// does for admin.lock.
func adminPath(adminDir, name string) string {
	if adminDir == "" {
		return name
	}
	return adminDir + string(os.PathSeparator) + name
}

// readIntent reads init-intent. found is false if there is none.
func readIntent(adminDir string) (rec intent, found bool, err error) {
	// #nosec G304 -- fixed record name inside the configured admin area.
	b, err := os.ReadFile(adminPath(adminDir, IntentName))
	if errors.Is(err, fs.ErrNotExist) {
		return intent{}, false, nil
	}
	if err != nil {
		return intent{}, false, fmt.Errorf("admininit: read %s: %w", IntentName, err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return intent{}, false, fmt.Errorf("admininit: %s is not a valid record: %w", IntentName, err)
	}
	if rec.Version != intentVersion {
		return intent{}, false, fmt.Errorf("admininit: %s has version %d, want %d", IntentName, rec.Version, intentVersion)
	}
	return rec, true, nil
}

// writeAdminRecord writes v as name in the admin area durably: a temporary file,
// fsync, rename over name, fsync of the directory (RW-C1 §5.2), mode 0640.
func writeAdminRecord(adminDir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := adminPath(adminDir, "."+name+".tmp")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("admininit: write %s: %w", name, err)
	}
	// #nosec G302 G304 -- fixed record name inside the configured admin area;
	// RW-C1 §5.2 and D01.4 §6 give admin files mode 0640, group sori.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return fmt.Errorf("admininit: write %s: %w", name, err)
	}
	_, werr := f.Write(append(b, '\n'))
	if err := errors.Join(werr, f.Chmod(0o640), f.Sync(), f.Close()); err != nil {
		return fmt.Errorf("admininit: write %s: %w", name, err)
	}
	if err := os.Rename(tmp, adminPath(adminDir, name)); err != nil {
		return fmt.Errorf("admininit: write %s: %w", name, err)
	}
	dir := adminDir
	if dir == "" {
		dir = "."
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("admininit: write %s: sync admin area: %w", name, err)
	}
	return nil
}

func writeIntent(adminDir string, rec *intent) error {
	return writeAdminRecord(adminDir, IntentName, rec)
}

// newNonce returns 16 CSPRNG bytes as hex.
func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("admininit: intent nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
