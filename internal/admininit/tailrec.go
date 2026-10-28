package admininit

// This file holds the three admin-tail records of SORI-D01-W021B-FMT-PREP-r2:
// keys/keyring (§3), policy/state with its default policy document (§4) and
// epoch (§5), plus the server.host_id grammar (§8.2). Each record has an
// encoder that produces its canonical bytes (§2.3.1, one trailing LF) and a
// strict reader that returns a typed failure, never a partial value (§2.3).
//
// Version 1 has no optional fields. Where RW-C1 leaves a later shape open
// (non-empty grants or credentials in the policy document, other revocation
// bases, epoch "retired"), the reader refuses it until that shape is defined.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Admin-tail names (FMT-PREP-r2 §2.1).
const (
	KeysDirName     = "keys"
	KeyringName     = "keyring"
	PolicyDirName   = "policy"
	PolicyStateName = "state"
)

// Admin-tail values fixed by FMT-PREP-r2.
const (
	tailVersion         = 1
	policySchemaVersion = 1
	macKeyIDPrefix      = "mk1_"
	macSecretLen        = 32
	keyStateActive      = "ACTIVE"
	keyStateRetired     = "RETIRED"
	revocationBasisNone = "none"
	policyOwnerPrefix   = "unix-uid:"
	policyDigestPrefix  = "sha256:"
	tailTimeLayout      = "2006-01-02T15:04:05Z"
	maxHostIDLen        = 64
)

// TailFailure classifies a refused admin-tail record (FMT-PREP-r2 §2.3).
type TailFailure int

// Kinds of TailFailure.
const (
	TailUnreadable     TailFailure = iota + 1 // the file could not be read
	TailNotCanonical                          // the bytes are not the canonical encoding of a v1 value
	TailUnknownVersion                        // the version key is not 1
	TailInvariant                             // a field or an invariant of the record is broken
)

func (k TailFailure) String() string {
	switch k {
	case TailUnreadable:
		return "UNREADABLE"
	case TailNotCanonical:
		return "NOT_CANONICAL"
	case TailUnknownVersion:
		return "UNKNOWN_VERSION"
	case TailInvariant:
		return "INVARIANT_BROKEN"
	}
	return "TailFailure(" + strconv.Itoa(int(k)) + ")"
}

// TailError is the typed failure of an admin-tail reader.
type TailError struct {
	Record string // KeyringName, PolicyStateName or EpochName
	Kind   TailFailure
	Err    error
}

func (e *TailError) Error() string {
	return fmt.Sprintf("admin tail %s: %s: %v", e.Record, e.Kind, e.Err)
}

func (e *TailError) Unwrap() error { return e.Err }

// ErrHostIDInvalid is returned by ValidateHostID.
var ErrHostIDInvalid = errors.New("server.host_id invalid")

// ValidateHostID checks the server.host_id grammar of FMT-PREP-r2 §8.2: 1 to
// 64 ASCII characters from [A-Za-z0-9._~-]. The value is used as given, with no
// trimming, case folding or fallback.
func ValidateHostID(hostID string) error {
	if len(hostID) == 0 || len(hostID) > maxHostIDLen {
		return fmt.Errorf("%w: length %d, want 1..%d", ErrHostIDInvalid, len(hostID), maxHostIDLen)
	}
	for i := 0; i < len(hostID); i++ {
		c := hostID[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '~' || c == '-'
		if !ok {
			return fmt.Errorf("%w: byte 0x%02x at %d is outside [A-Za-z0-9._~-]", ErrHostIDInvalid, c, i)
		}
	}
	return nil
}

// MACKey is one namespace MAC key of the keyring (FMT-PREP-r2 §3.1).
type MACKey struct {
	KeyID     string // "mk1_" + 32 lowercase hex
	Secret    []byte // exactly 32 bytes
	State     string // ACTIVE or RETIRED
	CreatedAt string // RFC 3339 UTC, seconds, Z
}

// Keyring is keys/keyring (FMT-PREP-r2 §3.1).
type Keyring struct {
	CurrentKeyID string
	Keys         []MACKey
}

// PolicyState is policy/state (FMT-PREP-r2 §4.1, RW-C1 §3.2).
type PolicyState struct {
	PolicyRevision  uint64
	HighWater       uint64
	Digest          string // "sha256:" + 64 lowercase hex of Document
	Document        []byte // the policy document bytes (document_b64url decoded)
	Owner           string // "unix-uid:<decimal euid>"
	RevocationBasis string
	UpdatedAt       string
}

// Epoch is the activation epoch (FMT-PREP-r2 §5.1, C.5.7 r5 step 4).
type Epoch struct {
	InstallationID  string
	ActivationEpoch uint64
	CutCounter      uint64
	HostID          string
	ActivationID    string // 32 lowercase hex
	StartedAt       string
}

// tailTime renders t in the tail time form (UTC, seconds, Z).
func tailTime(t time.Time) string { return t.UTC().Format(tailTimeLayout) }

func validTailTime(s string) bool {
	t, err := time.Parse(tailTimeLayout, s)
	return err == nil && t.Format(tailTimeLayout) == s
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validKeyID(s string) bool {
	return strings.HasPrefix(s, macKeyIDPrefix) && isHex32(s[len(macKeyIDPrefix):])
}

var b64url = base64.RawURLEncoding.Strict()

// decodeB64url decodes an unpadded base64url field and requires that
// re-encoding reproduces the field exactly. The decoder skips CR and LF, which
// a canonical JSON string can carry as escapes, so decoding alone would accept
// them.
func decodeB64url(s string) ([]byte, error) {
	b, err := b64url.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if b64url.EncodeToString(b) != s {
		return nil, errors.New("base64url does not re-encode to the same text")
	}
	return b, nil
}

// defaultPolicyDocument is the template sori-local-default-policy:v1 for
// installation I (FMT-PREP-r2 §4.2, RW-C1 §3.2): canonical, no newline.
func defaultPolicyDocument(installationID string) ([]byte, error) {
	return encodeCanon(map[string]any{
		"credentials":     []any{},
		"grants":          []any{},
		"installation_id": installationID,
		"schema_version":  uint64(policySchemaVersion),
	})
}

// newDefaultPolicyState is the init policy/state: revision 1, high_water 1,
// revocation basis none and the default document for I.
func newDefaultPolicyState(installationID string, euid int, now time.Time) (PolicyState, error) {
	doc, err := defaultPolicyDocument(installationID)
	if err != nil {
		return PolicyState{}, err
	}
	sum := sha256.Sum256(doc)
	return PolicyState{
		PolicyRevision:  1,
		HighWater:       1,
		Digest:          policyDigestPrefix + hex.EncodeToString(sum[:]),
		Document:        doc,
		Owner:           policyOwnerPrefix + strconv.Itoa(euid),
		RevocationBasis: revocationBasisNone,
		UpdatedAt:       tailTime(now),
	}, nil
}

// encodeRecord renders a record value with the trailing LF.
func encodeRecord(v map[string]any) ([]byte, error) {
	b, err := encodeCanon(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func encodeKeyring(k Keyring) ([]byte, error) {
	if err := k.validate(); err != nil {
		return nil, err
	}
	keys := make([]any, 0, len(k.Keys))
	for _, mk := range k.Keys {
		keys = append(keys, map[string]any{
			"created_at":    mk.CreatedAt,
			"key_id":        mk.KeyID,
			"secret_b64url": b64url.EncodeToString(mk.Secret),
			"state":         mk.State,
		})
	}
	return encodeRecord(map[string]any{
		"current_key_id": k.CurrentKeyID,
		"keys":           keys,
		"version":        uint64(tailVersion),
	})
}

func encodePolicyState(s PolicyState) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	return encodeRecord(map[string]any{
		"current": map[string]any{
			"digest":           s.Digest,
			"document_b64url":  b64url.EncodeToString(s.Document),
			"owner":            s.Owner,
			"policy_revision":  s.PolicyRevision,
			"revocation_basis": s.RevocationBasis,
			"updated_at":       s.UpdatedAt,
		},
		"high_water": s.HighWater,
		"version":    uint64(tailVersion),
	})
}

func encodeEpoch(e Epoch) ([]byte, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	return encodeRecord(map[string]any{
		"activation_epoch": e.ActivationEpoch,
		"active_writer": map[string]any{
			"activation_id": e.ActivationID,
			"host_id":       e.HostID,
			"started_at":    e.StartedAt,
		},
		"cut_counter":     e.CutCounter,
		"installation_id": e.InstallationID,
		"version":         uint64(tailVersion),
	})
}

func (k Keyring) validate() error {
	if len(k.Keys) == 0 {
		return errors.New("keyring has no key")
	}
	seen := map[string]bool{}
	currentActive := false
	for i, mk := range k.Keys {
		switch {
		case !validKeyID(mk.KeyID):
			return fmt.Errorf("keys[%d]: key_id %q is not mk1_ + 32 lowercase hex", i, mk.KeyID)
		case seen[mk.KeyID]:
			return fmt.Errorf("keys[%d]: duplicate key_id %s", i, mk.KeyID)
		case len(mk.Secret) != macSecretLen:
			return fmt.Errorf("keys[%d]: secret is %d bytes, want %d", i, len(mk.Secret), macSecretLen)
		case mk.State != keyStateActive && mk.State != keyStateRetired:
			return fmt.Errorf("keys[%d]: state %q", i, mk.State)
		case !validTailTime(mk.CreatedAt):
			return fmt.Errorf("keys[%d]: created_at %q", i, mk.CreatedAt)
		}
		seen[mk.KeyID] = true
		if mk.KeyID == k.CurrentKeyID && mk.State == keyStateActive {
			currentActive = true
		}
	}
	if !currentActive {
		return fmt.Errorf("current_key_id %q does not name an ACTIVE key", k.CurrentKeyID)
	}
	return nil
}

func (s PolicyState) validate() error {
	sum := sha256.Sum256(s.Document)
	switch {
	case s.PolicyRevision < 1:
		return errors.New("policy_revision is 0")
	case s.HighWater != s.PolicyRevision:
		return fmt.Errorf("high_water %d != policy_revision %d", s.HighWater, s.PolicyRevision)
	case s.Digest != policyDigestPrefix+hex.EncodeToString(sum[:]):
		return fmt.Errorf("digest %q does not match the document", s.Digest)
	case !validOwner(s.Owner):
		return fmt.Errorf("owner %q is not unix-uid:<decimal>", s.Owner)
	case s.RevocationBasis != revocationBasisNone:
		return fmt.Errorf("revocation_basis %q is not supported by version 1 of this reader", s.RevocationBasis)
	case !validTailTime(s.UpdatedAt):
		return fmt.Errorf("updated_at %q", s.UpdatedAt)
	}
	_, err := policyDocumentInstallation(s.Document)
	return err
}

func validOwner(s string) bool {
	d, ok := strings.CutPrefix(s, policyOwnerPrefix)
	if !ok || d == "" || len(d) > 1 && d[0] == '0' {
		return false
	}
	n, err := strconv.ParseUint(d, 10, 32)
	return err == nil && strconv.FormatUint(n, 10) == d
}

// policyDocumentInstallation reads a policy document strictly (canonical, no
// newline) and returns its installation_id. Version 1 of this reader accepts
// only the deny-all shape: credentials and grants must be empty.
func policyDocumentInstallation(doc []byte) (string, error) {
	v, err := decodeCanon(doc, false)
	if err != nil {
		return "", fmt.Errorf("policy document: %w", err)
	}
	f, err := fields(v, "policy document", "credentials", "grants", "installation_id", "schema_version")
	if err != nil {
		return "", err
	}
	if n, ok := f["schema_version"].(uint64); !ok || n != policySchemaVersion {
		return "", fmt.Errorf("policy document: unsupported schema_version %v", f["schema_version"])
	}
	for _, name := range []string{"credentials", "grants"} {
		a, ok := f[name].([]any)
		if !ok {
			return "", fmt.Errorf("policy document: %s is not an array", name)
		}
		if len(a) != 0 {
			return "", fmt.Errorf("policy document: non-empty %s is not supported by version 1 of this reader", name)
		}
	}
	id, ok := f["installation_id"].(string)
	if !ok || !isHex32(id) {
		return "", fmt.Errorf("policy document: installation_id %v is not 32 lowercase hex", f["installation_id"])
	}
	return id, nil
}

func (e Epoch) validate() error {
	switch {
	case !isHex32(e.InstallationID):
		return fmt.Errorf("installation_id %q is not 32 lowercase hex", e.InstallationID)
	case e.ActivationEpoch < 1:
		return errors.New("activation_epoch is 0")
	case !isHex32(e.ActivationID):
		return fmt.Errorf("activation_id %q is not 32 lowercase hex", e.ActivationID)
	case !validTailTime(e.StartedAt):
		return fmt.Errorf("started_at %q", e.StartedAt)
	}
	return ValidateHostID(e.HostID)
}

// fields checks that v is an object with exactly the given keys.
func fields(v any, what string, keys ...string) (map[string]any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", what)
	}
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			return nil, fmt.Errorf("%s: missing key %q", what, k)
		}
	}
	if len(obj) != len(keys) {
		for k := range obj {
			found := false
			for _, want := range keys {
				found = found || k == want
			}
			if !found {
				return nil, fmt.Errorf("%s: unknown key %q", what, k)
			}
		}
	}
	return obj, nil
}

// recordTop decodes a record and checks its version before anything else, so
// an unknown version is reported as such and not as an unknown key.
func recordTop(name string, data []byte) (map[string]any, *TailError) {
	v, err := decodeCanon(data, true)
	if err != nil {
		return nil, &TailError{Record: name, Kind: TailNotCanonical, Err: err}
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, &TailError{Record: name, Kind: TailInvariant, Err: errors.New("record is not an object")}
	}
	if ver, ok := obj["version"].(uint64); !ok || ver != tailVersion {
		return nil, &TailError{Record: name, Kind: TailUnknownVersion, Err: fmt.Errorf("version %v", obj["version"])}
	}
	return obj, nil
}

func str(obj map[string]any, k string) (string, error) {
	s, ok := obj[k].(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", k)
	}
	return s, nil
}

func unum(obj map[string]any, k string) (uint64, error) {
	n, ok := obj[k].(uint64)
	if !ok {
		return 0, fmt.Errorf("%s is not an unsigned integer", k)
	}
	return n, nil
}

// parseKeyring is the strict reader of keys/keyring.
func parseKeyring(data []byte) (Keyring, error) {
	bad := func(err error) (Keyring, error) {
		return Keyring{}, &TailError{Record: KeyringName, Kind: TailInvariant, Err: err}
	}
	obj, terr := recordTop(KeyringName, data)
	if terr != nil {
		return Keyring{}, terr
	}
	if _, err := fields(obj, "keyring", "current_key_id", "keys", "version"); err != nil {
		return bad(err)
	}
	var k Keyring
	var err error
	if k.CurrentKeyID, err = str(obj, "current_key_id"); err != nil {
		return bad(err)
	}
	list, ok := obj["keys"].([]any)
	if !ok {
		return bad(errors.New("keys is not an array"))
	}
	for i, e := range list {
		ko, err := fields(e, fmt.Sprintf("keys[%d]", i), "created_at", "key_id", "secret_b64url", "state")
		if err != nil {
			return bad(err)
		}
		var mk MACKey
		var secret string
		if mk.CreatedAt, err = str(ko, "created_at"); err != nil {
			return bad(err)
		}
		if mk.KeyID, err = str(ko, "key_id"); err != nil {
			return bad(err)
		}
		if mk.State, err = str(ko, "state"); err != nil {
			return bad(err)
		}
		if secret, err = str(ko, "secret_b64url"); err != nil {
			return bad(err)
		}
		if mk.Secret, err = decodeB64url(secret); err != nil {
			return bad(fmt.Errorf("keys[%d]: secret_b64url is not unpadded base64url", i))
		}
		k.Keys = append(k.Keys, mk)
	}
	if err := k.validate(); err != nil {
		return bad(err)
	}
	return k, nil
}

// parsePolicyState is the strict reader of policy/state (FMT-PREP-r2 §2.3,
// §4.3 start-time checks). The checks against the epoch's installation id and
// the store's referenced revisions belong to the caller.
func parsePolicyState(data []byte) (PolicyState, error) {
	bad := func(err error) (PolicyState, error) {
		return PolicyState{}, &TailError{Record: PolicyStateName, Kind: TailInvariant, Err: err}
	}
	obj, terr := recordTop(PolicyStateName, data)
	if terr != nil {
		return PolicyState{}, terr
	}
	if _, err := fields(obj, "policy/state", "current", "high_water", "version"); err != nil {
		return bad(err)
	}
	cur, err := fields(obj["current"], "current",
		"digest", "document_b64url", "owner", "policy_revision", "revocation_basis", "updated_at")
	if err != nil {
		return bad(err)
	}
	var s PolicyState
	var doc string
	if s.HighWater, err = unum(obj, "high_water"); err != nil {
		return bad(err)
	}
	if s.PolicyRevision, err = unum(cur, "policy_revision"); err != nil {
		return bad(err)
	}
	for k, dst := range map[string]*string{
		"digest": &s.Digest, "document_b64url": &doc, "owner": &s.Owner,
		"revocation_basis": &s.RevocationBasis, "updated_at": &s.UpdatedAt,
	} {
		if *dst, err = str(cur, k); err != nil {
			return bad(err)
		}
	}
	if s.Document, err = decodeB64url(doc); err != nil {
		return bad(errors.New("document_b64url is not unpadded base64url"))
	}
	if err := s.validate(); err != nil {
		return bad(err)
	}
	return s, nil
}

// parseEpoch is the strict reader of epoch.
func parseEpoch(data []byte) (Epoch, error) {
	bad := func(err error) (Epoch, error) {
		return Epoch{}, &TailError{Record: EpochName, Kind: TailInvariant, Err: err}
	}
	obj, terr := recordTop(EpochName, data)
	if terr != nil {
		return Epoch{}, terr
	}
	if _, err := fields(obj, "epoch", "activation_epoch", "active_writer", "cut_counter", "installation_id", "version"); err != nil {
		return bad(err)
	}
	aw, err := fields(obj["active_writer"], "active_writer", "activation_id", "host_id", "started_at")
	if err != nil {
		return bad(err)
	}
	var e Epoch
	if e.ActivationEpoch, err = unum(obj, "activation_epoch"); err != nil {
		return bad(err)
	}
	if e.CutCounter, err = unum(obj, "cut_counter"); err != nil {
		return bad(err)
	}
	if e.InstallationID, err = str(obj, "installation_id"); err != nil {
		return bad(err)
	}
	if e.ActivationID, err = str(aw, "activation_id"); err != nil {
		return bad(err)
	}
	if e.HostID, err = str(aw, "host_id"); err != nil {
		return bad(err)
	}
	if e.StartedAt, err = str(aw, "started_at"); err != nil {
		return bad(err)
	}
	if err := e.validate(); err != nil {
		return bad(err)
	}
	return e, nil
}
