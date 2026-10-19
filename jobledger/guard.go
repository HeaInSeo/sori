package jobledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrSchemaVersion: the database was written by another schema version, or is
	// not an empty or ledger database. The store never migrates implicitly.
	ErrSchemaVersion = errors.New("jobledger: unsupported schema version")
	// ErrTransientPath: the path names an in-memory or private temporary database.
	ErrTransientPath = errors.New("jobledger: path names a non-durable database")
	// ErrNotInitialized: the store has no installation marker yet (see Init).
	ErrNotInitialized = errors.New("jobledger: store not initialized")
	// ErrInstallationMismatch: the Guard or namespace names another installation.
	ErrInstallationMismatch = errors.New("jobledger: installation mismatch")
	// ErrFenceStale: the Guard's activation epoch is older than the store's, so the
	// caller is a replaced writer. Nothing was changed.
	ErrFenceStale = errors.New("jobledger: stale activation epoch")
	// ErrReadOnlyHold: the store is not the live store of the Guard's epoch and cut
	// (restored and not activated, a newer epoch than the store, or a capture cut not
	// yet absorbed). Protected mutations are refused; reads still work.
	ErrReadOnlyHold = errors.New("jobledger: store is read-only pending reconcile")
	// ErrReconcileHold: the key cannot be classified as new or existing from this
	// store (D01.2 B.2.3 N0/N4H/N5/N7/N8).
	ErrReconcileHold = errors.New("jobledger: reconcile hold")
	// ErrNamespaceInvalid: the namespace does not belong to this installation and
	// principal (N1).
	ErrNamespaceInvalid = errors.New("jobledger: namespace invalid")
	// ErrIdempotencyConflict: the key is already mapped to a different request meaning.
	ErrIdempotencyConflict = errors.New("jobledger: idempotency key conflicts with a prior request")
	// ErrVersionConflict: the record's version is not the expected version.
	ErrVersionConflict = errors.New("jobledger: record version conflict")
	// ErrNotFound: no such record.
	ErrNotFound = errors.New("jobledger: not found")
	// ErrAlreadyTerminal: the Job already has a terminal receipt.
	ErrAlreadyTerminal = errors.New("jobledger: job already terminal")
	// ErrInvalidTransition: the requested Job phase change is not allowed.
	ErrInvalidTransition = errors.New("jobledger: invalid job transition")
	// ErrDescriptorConflict: a prepared package or binding already records a
	// different descriptor, mapping or revision; the recorded one is never replaced.
	ErrDescriptorConflict = errors.New("jobledger: recorded descriptor conflicts")
	// ErrActivationRefused: activation preconditions do not hold.
	ErrActivationRefused = errors.New("jobledger: activation refused")
	// ErrLedgerIntegrity: the hash-chained ledger does not verify.
	ErrLedgerIntegrity = errors.New("jobledger: ledger integrity failure")
	// ErrInitRefused: InspectInitState found a store that is not pristine, schema-only
	// or exactly marker-empty, so offline init must not touch it.
	ErrInitRefused = errors.New("jobledger: store refused for init")
)

// Guard is the fence a caller reads from the admin-control area before a request
// batch (D01.3 C.5). That area is outside this database and is never written by a
// restore, so a restored ledger cannot bring back an older epoch on its own.
type Guard struct {
	InstallationID  string
	ActivationEpoch uint64
	// CutCounter is the non-rollback capture-cut counter.
	CutCounter uint64
	// ActivationNonce is the restore nonce the current activation consumed. It is
	// checked only by Activate.
	ActivationNonce string
}

// usable reports whether the Guard could be read at all (N0 when false).
func (g Guard) usable() bool { return g.InstallationID != "" && g.ActivationEpoch > 0 }

// marker is the store's own installation/restore marker.
type marker struct {
	installationID string
	restoreState   string
	activeEpoch    uint64
	cutSeq         uint64
	restoreNonce   string
}

func readMarker(ctx context.Context, q queryRower) (marker, error) {
	var m marker
	vals := map[string]*string{metaInstallationID: &m.installationID, metaRestoreState: &m.restoreState, metaRestoreNonce: &m.restoreNonce}
	for k, dst := range vals {
		v, _, err := getMeta(ctx, q, k)
		if err != nil {
			return m, err
		}
		*dst = v
	}
	if m.installationID == "" {
		return m, ErrNotInitialized
	}
	var err error
	if m.activeEpoch, err = metaUint(ctx, q, metaActiveEpoch); err != nil {
		return m, err
	}
	m.cutSeq, err = metaUint(ctx, q, metaCutSeq)
	return m, err
}

func metaUint(ctx context.Context, q queryRower, key string) (uint64, error) {
	v, _, err := getMeta(ctx, q, key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("jobledger: %s = %q: %w", key, v, err)
	}
	return n, nil
}

// liveCurrent is D01.2-N4-r3 live_current(store) for the data side: the store is
// LIVE, of this installation, at the Guard's epoch and has absorbed every cut.
func (m marker) liveCurrent(g Guard) bool {
	return g.usable() && m.installationID == g.InstallationID && m.restoreState == RestoreLive &&
		m.activeEpoch == g.ActivationEpoch && m.cutSeq == g.CutCounter
}

// fence checks a protected mutation against the Guard (D01.3 r1 §5.5).
func fence(ctx context.Context, q queryRower, g Guard) (marker, error) {
	m, err := readMarker(ctx, q)
	if err != nil {
		return m, err
	}
	switch {
	case !g.usable():
		return m, fmt.Errorf("%w: admin-control guard unusable", ErrReadOnlyHold)
	case g.InstallationID != m.installationID:
		return m, fmt.Errorf("%w: guard %q, store %q", ErrInstallationMismatch, g.InstallationID, m.installationID)
	case g.ActivationEpoch < m.activeEpoch:
		return m, fmt.Errorf("%w: guard epoch %d, store epoch %d", ErrFenceStale, g.ActivationEpoch, m.activeEpoch)
	case !m.liveCurrent(g):
		return m, fmt.Errorf("%w: store %s epoch %d cut %d, guard epoch %d cut %d",
			ErrReadOnlyHold, m.restoreState, m.activeEpoch, m.cutSeq, g.ActivationEpoch, g.CutCounter)
	}
	return m, nil
}

// Init writes the installation marker of a fresh store: LIVE at activation epoch 1
// and cut 0 (D01.3 C.5.7, data side of `sori-admin init`). It refuses a store that
// already has a marker.
//
// It is a marker-only bootstrap (RW-C1 §5.1): one transaction checks that the store
// is exactly this version's schema, has no content and no marker besides
// schema_version, then writes the four markers and nothing else. It appends no
// ledger row and no ledger head. A store with any other content, object or marker
// is refused unchanged. A rerun after a lost acknowledgement does not call Init:
// InspectInitState reports INIT_MARKER_EMPTY with the recorded installation id.
// Ledgers written by older versions, which start with an "init" row, stay readable.
func (s *Store) Init(ctx context.Context, installationID string) error {
	if installationID == "" {
		return fmt.Errorf("%w: empty installation id", ErrNotInitialized)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, ok, err := getMeta(ctx, tx, metaInstallationID)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: store already initialized", ErrActivationRefused)
		}
		// Marker-only bootstrap (RW-C1 §5.1): the store must be exactly this
		// version's schema with no content and no marker but schema_version. This is
		// the only mutation that appends no ledger row, so the ledger stays empty and
		// its head stays (0, "").
		if err := checkSchemaObjects(ctx, tx); err != nil {
			return fmt.Errorf("%w: %w", ErrActivationRefused, err)
		}
		if err := checkNoContent(ctx, tx); err != nil {
			return fmt.Errorf("%w: %w", ErrActivationRefused, err)
		}
		meta, err := readMeta(ctx, tx)
		if err != nil {
			return err
		}
		if len(meta) != 1 || meta[metaSchemaVersion] != strconv.Itoa(SchemaVersion) {
			return fmt.Errorf("%w: store has markers other than schema_version", ErrActivationRefused)
		}
		for k, v := range initMarkers(installationID) {
			if err := setMeta(ctx, tx, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// initMarkers are the exact markers Init writes besides schema_version.
func initMarkers(installationID string) map[string]string {
	return map[string]string{
		metaInstallationID: installationID, metaRestoreState: RestoreLive, metaActiveEpoch: "1", metaCutSeq: "0",
	}
}

// newID returns "<prefix>_" + a 256-bit CSPRNG value in lowercase base32 without
// padding (D01.2 B.4).
func newID(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("jobledger: random id: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return prefix + "_" + strings.ToLower(enc), nil
}
