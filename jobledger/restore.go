package jobledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// Cut describes a capture cut taken from this store. The admin-control area keeps
// the authoritative cut record and counter (W09); this is the data-side evidence.
type Cut struct {
	CutSeq           uint64
	LastLedgerSeq    int64
	LedgerHeadDigest string
}

// AbsorbCut advances store_cut_seq to the Guard's CutCounter when the admin-control
// counter is exactly one ahead (D01.3-r4 runtime advance). It must run before any
// later request is classified; a larger gap leaves the store read-only.
func (s *Store) AbsorbCut(ctx context.Context, g Guard) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		m, err := readMarker(ctx, tx)
		if err != nil {
			return err
		}
		if m.restoreState != RestoreLive || m.installationID != g.InstallationID || m.activeEpoch != g.ActivationEpoch ||
			g.CutCounter != m.cutSeq+1 {
			return fmt.Errorf("%w: cannot absorb cut %d at store cut %d", ErrReadOnlyHold, g.CutCounter, m.cutSeq)
		}
		if err := setMeta(ctx, tx, metaCutSeq, strconv.FormatUint(g.CutCounter, 10)); err != nil {
			return err
		}
		return appendLedger(ctx, tx, "cut", strconv.FormatUint(g.CutCounter, 10), map[string]any{"store_cut_seq": g.CutCounter})
	})
}

// VerifyCut verifies the ledger (VerifyLedger) and that it still holds the cut's last
// row with the cut's digest. The Cut is evidence retained outside this database (the
// admin-control cut record), so it also catches a rewrite that dropped rows up to and
// including the cut and moved the recorded head back with them. Rows appended after
// the cut are allowed.
func (s *Store) VerifyCut(ctx context.Context, c Cut) error {
	if err := s.VerifyLedger(ctx); err != nil {
		return err
	}
	if c.LastLedgerSeq == 0 {
		return nil
	}
	var digest string
	err := s.db.QueryRowContext(ctx, `SELECT digest FROM ledger WHERE seq = ?`, c.LastLedgerSeq).Scan(&digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: cut ledger seq %d is missing", ErrLedgerIntegrity, c.LastLedgerSeq)
	case err != nil:
		return fmt.Errorf("jobledger: read ledger: %w", err)
	case digest != c.LedgerHeadDigest:
		return fmt.Errorf("%w: cut ledger seq %d digest differs", ErrLedgerIntegrity, c.LastLedgerSeq)
	}
	return nil
}

// Snapshot writes a consistent copy of the store to dst (VACUUM INTO), verifies the
// copy's ledger and returns the cut evidence read back from it. It stands in for the
// backup half of capture-cut.
func (s *Store) Snapshot(ctx context.Context, dst string) (Cut, error) {
	var c Cut
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return c, fmt.Errorf("jobledger: snapshot: %w", err)
	}
	cp, err := sql.Open("sqlite", fileURI(dst)+"?mode=ro")
	if err != nil {
		return c, fmt.Errorf("jobledger: open snapshot: %w", err)
	}
	defer cp.Close()
	m, err := readMarker(ctx, cp)
	if err != nil {
		return c, err
	}
	if err := verifyLedger(ctx, cp); err != nil {
		return c, err
	}
	c.CutSeq = m.cutSeq
	c.LastLedgerSeq, c.LedgerHeadDigest, err = ledgerHead(ctx, cp)
	return c, err
}

// MarkRestored is the restore tool's write into restored data: the store becomes
// RESTORED_INACTIVE with a fresh restore nonce, and stays read-only until an
// activation consumes that nonce. It returns the nonce.
func (s *Store) MarkRestored(ctx context.Context) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("jobledger: restore nonce: %w", err)
	}
	nonce := hex.EncodeToString(b[:])
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := readMarker(ctx, tx); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, metaRestoreState, RestoreRestoredInactive); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, metaRestoreNonce, nonce); err != nil {
			return err
		}
		return appendLedger(ctx, tx, "restored", nonce, map[string]any{"restore_state": RestoreRestoredInactive})
	})
	if err != nil {
		return "", err
	}
	return nonce, nil
}

// Activate makes a restored store LIVE under the Guard (D01.3 C.5 row S9): the Guard
// must name this installation, an epoch exactly one above the store's, the cut the
// store holds, and the restore nonce written by MarkRestored. Restoring the same
// backup again yields a new nonce, so one activation record cannot activate twice.
func (s *Store) Activate(ctx context.Context, g Guard) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		m, err := readMarker(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case m.restoreState != RestoreRestoredInactive:
			return fmt.Errorf("%w: store is %s", ErrActivationRefused, m.restoreState)
		case g.InstallationID != m.installationID:
			return fmt.Errorf("%w: %w", ErrActivationRefused, ErrInstallationMismatch)
		case g.ActivationEpoch != m.activeEpoch+1:
			return fmt.Errorf("%w: guard epoch %d, store epoch %d", ErrActivationRefused, g.ActivationEpoch, m.activeEpoch)
		case g.CutCounter != m.cutSeq:
			return fmt.Errorf("%w: guard cut %d, store cut %d", ErrActivationRefused, g.CutCounter, m.cutSeq)
		case g.ActivationNonce == "" || g.ActivationNonce != m.restoreNonce:
			return fmt.Errorf("%w: restore nonce does not match", ErrActivationRefused)
		}
		for k, v := range map[string]string{metaRestoreState: RestoreLive, metaActiveEpoch: strconv.FormatUint(g.ActivationEpoch, 10)} {
			if err := setMeta(ctx, tx, k, v); err != nil {
				return err
			}
		}
		return appendLedger(ctx, tx, "activate", strconv.FormatUint(g.ActivationEpoch, 10), map[string]any{"activation_epoch": g.ActivationEpoch})
	})
}
