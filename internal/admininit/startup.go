package admininit

// This file is the first fail-closed server start classifier slice of
// SORI-D01-W021B-FMT-PREP-r2 §9.3 (P-a/P-b/P-c, admission
// SORI-NORMAL-STARTUP-20261028-391159f9): rows S0, S6, S1, S2, S1c, S1d and S3.
// Every other state, including the not yet implemented S9 activation and the
// rows S4, S5a, S5b, S7 and S10, is S12 RO/HOLD. Only S3 is writable.
//
// The classifier reads; it never writes. It takes no lock (the server process
// of P-d, not part of this slice, holds L2 around it), never opens a database
// for writing, never creates a pristine path, writes no marker, WAL, schema or
// admin file and never falls back to a memory store. No file or SQLite sidecar
// (-wal, -shm, -journal) is created, changed or removed: the job ledger is read
// only immutable, and a ledger that needs WAL or journal recovery to be read
// (a crash image) is not read at all but held as S12 RO/HOLD. The data root's
// marker, schema and content facts are read before anything the server could
// later write.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/HeaInSeo/sori/jobledger"
)

// StartRow is a C.5.4 start row.
type StartRow string

// Start rows this slice classifies. RowS12 also stands for every row the slice
// does not implement.
const (
	RowS0  StartRow = "S0"  // admin-control epoch/host-id/keyring invalid (RO/HOLD)
	RowS6  StartRow = "S6"  // policy currentness UNKNOWN (RO/HOLD)
	RowS1  StartRow = "S1"  // epoch absent, store content-empty, unmarked or init marker only (RO until init completes)
	RowS2  StartRow = "S2"  // epoch absent, store not content-empty (RO/HOLD)
	RowS1c StartRow = "S1c" // epoch present, store has no restore_state (RO/HOLD, data-root diagnostic)
	RowS1d StartRow = "S1d" // epoch present, store marker names another installation (RO/HOLD)
	RowS3  StartRow = "S3"  // live current store of this installation, host, epoch and cut (RW)
	RowS12 StartRow = "S12" // unsupported or unknown state (RO/HOLD)
)

// PolicyObservation is the last (policy_revision, digest) of policy/state a
// server process observed (FMT-PREP-r2 §4.3 carried RW-C1 §3.2 runtime checks).
// A fresh process has none.
type PolicyObservation struct {
	Revision uint64
	Digest   string
}

// StartClass is the result of ClassifyStart.
type StartClass struct {
	Row StartRow
	// Writable is true only for S3.
	Writable bool
	// Reason says why the row was chosen. For S1c it names the data root.
	Reason string
	// Policy is the policy/state observation this classification read, set
	// whenever policy/state passed its reader (S1c, S1d, S3, S12 after it). The
	// caller keeps it as the last observation for the next read.
	Policy *PolicyObservation
}

// adminFacts are the P-a facts read from the admin-control area.
type adminFacts struct {
	epochPresent bool
	epochErr     error // epoch present but unreadable or invalid
	epoch        Epoch
	keyringErr   error
	policyErr    error
	policy       PolicyState
}

// storeFacts are the P-b facts of the data root's job ledger.
type storeFacts struct {
	err          error // the store could not be read with certainty
	facts        jobledger.StartFacts
	authPristine bool // the authority database file is absent or empty
}

// ClassifyStart classifies a server start (FMT-PREP-r2 §9.2, §9.3) for the
// admin-control area adminDir, the data root dataRoot with databases p and the
// explicit server.host_id hostID. last is the previous policy observation of
// this process, nil for a fresh one. It writes nothing.
func ClassifyStart(ctx context.Context, adminDir, dataRoot, hostID string, p Paths, last *PolicyObservation) StartClass {
	// Data-root facts first: before any admin read the server could act on and
	// before anything could open a database for writing.
	sf := readStoreFacts(ctx, p)
	af := readAdminFacts(adminDir)
	return classifyRow(af, sf, dataRoot, hostID, last)
}

func readStoreFacts(ctx context.Context, p Paths) storeFacts {
	if p.JobLedger == "" || p.Authority == "" {
		return storeFacts{err: fmt.Errorf("%w: database path not set", ErrRefused)}
	}
	f, err := jobledger.InspectStartFacts(ctx, p.JobLedger)
	if err != nil {
		return storeFacts{err: err}
	}
	// The authority database carries no markers. Only its pristine state is a
	// start fact here: with epoch present a pristine authority beside a marked
	// ledger means the data root was replaced or lost a file (S1c). It is read
	// from the file's metadata only: sqlitestore.InspectInitState opens a
	// non-empty file through SQLite's read-only WAL path, which can create or
	// map -wal/-shm files, and the classifier must not write.
	//
	// Slice limit (FMT-PREP-r2 §9.1 P-b asks content-emptiness of both
	// databases): this slice reads only the authority file's existence and
	// size. A non-empty authority file is taken as not pristine; its schema
	// and content-emptiness are not read.
	fi, err := os.Lstat(p.Authority)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return storeFacts{facts: f, authPristine: true}
	case err != nil:
		return storeFacts{err: fmt.Errorf("authority %s: %w", p.Authority, err)}
	case !fi.Mode().IsRegular():
		return storeFacts{err: fmt.Errorf("authority %s is not a regular file (%s)", p.Authority, fi.Mode().Type())}
	}
	return storeFacts{facts: f, authPristine: fi.Size() == 0}
}

func hold(row StartRow, format string, args ...any) StartClass {
	return StartClass{Row: row, Reason: fmt.Sprintf(format, args...)}
}

// classifyRow is P-c: a pure function over the P-a and P-b facts, in the C.5.4
// order S0, S6, S1, S2, S1c, S1d, S3 with every other row as S12.
func classifyRow(af adminFacts, sf storeFacts, dataRoot, hostID string, last *PolicyObservation) StartClass {
	if !af.epochPresent {
		return classifyNoEpoch(sf, dataRoot)
	}
	// S0: epoch, host id or keyring invalid (central D01 choice, §9.4).
	switch {
	case af.epochErr != nil:
		return hold(RowS0, "epoch invalid: %v", af.epochErr)
	case ValidateHostID(hostID) != nil:
		return hold(RowS0, "%v", ValidateHostID(hostID))
	case af.keyringErr != nil:
		return hold(RowS0, "keyring invalid: %v", af.keyringErr)
	}
	// S6: policy currentness UNKNOWN.
	if af.policyErr != nil {
		return hold(RowS6, "policy/state invalid: %v", af.policyErr)
	}
	docID, err := policyDocumentInstallation(af.policy.Document)
	if err != nil {
		return hold(RowS6, "policy document invalid: %v", err)
	}
	if docID != af.epoch.InstallationID {
		return hold(RowS6, "policy document names installation %s, epoch %s", docID, af.epoch.InstallationID)
	}
	if last != nil {
		switch {
		case af.policy.HighWater < last.Revision:
			return hold(RowS6, "policy high_water %d is below the last observed revision %d", af.policy.HighWater, last.Revision)
		case af.policy.PolicyRevision == last.Revision && af.policy.Digest != last.Digest:
			return hold(RowS6, "policy revision %d has digest %s, last observed %s", last.Revision, af.policy.Digest, last.Digest)
		}
	}
	obs := &PolicyObservation{Revision: af.policy.PolicyRevision, Digest: af.policy.Digest}
	c := classifyWithEpoch(af.epoch, sf, dataRoot, hostID)
	c.Policy = obs
	return c
}

// classifyNoEpoch is §9.2 with epoch absent. The keyring, policy/state and the
// keys/ and policy/ directories are ignored whatever their state.
func classifyNoEpoch(sf storeFacts, dataRoot string) StartClass {
	f := sf.facts
	switch {
	case sf.err != nil:
		return hold(RowS12, "data root %s: store unreadable: %v", dataRoot, sf.err)
	case f.Pristine:
		return hold(RowS1, "epoch absent; data root %s store pristine", dataRoot)
	case !f.SchemaKnown:
		return hold(RowS12, "epoch absent; data root %s store schema unknown", dataRoot)
	case !f.ContentEmpty:
		return hold(RowS2, "epoch absent; data root %s store not content-empty", dataRoot)
	case !f.Marked:
		return hold(RowS1, "epoch absent; data root %s store content-empty and unmarked", dataRoot)
	case f.InitMarkerOnly:
		return hold(RowS1, "epoch absent; data root %s store content-empty with the init marker", dataRoot)
	}
	// Content-empty with another marker (RESTORED_INACTIVE: S5a) is not
	// implemented in this slice.
	return hold(RowS12, "epoch absent; data root %s store content-empty with marker %s epoch %d cut %d (unsupported row)",
		dataRoot, f.RestoreState, f.ActiveEpoch, f.CutSeq)
}

// classifyWithEpoch is S1c, S1d and S3 once the admin-control area is valid.
func classifyWithEpoch(e Epoch, sf storeFacts, dataRoot, hostID string) StartClass {
	f := sf.facts
	switch {
	case sf.err != nil:
		return hold(RowS12, "data root %s: store unreadable: %v", dataRoot, sf.err)
	case f.Pristine || sf.authPristine:
		return hold(RowS1c, "epoch present but data root %s is pristine (job ledger pristine=%v, authority pristine=%v); no init, no memory store",
			dataRoot, f.Pristine, sf.authPristine)
	case !f.SchemaKnown:
		return hold(RowS12, "data root %s store schema unknown", dataRoot)
	case !f.Marked || f.RestoreState == "":
		return hold(RowS1c, "epoch present but data root %s store has no restore_state (unmarked, content-empty=%v); no init, no memory store",
			dataRoot, f.ContentEmpty)
	case f.InstallationID != e.InstallationID:
		return hold(RowS1d, "data root %s store belongs to installation %s, epoch names %s", dataRoot, f.InstallationID, e.InstallationID)
	case f.RestoreState == jobledger.RestoreLive && e.HostID == hostID &&
		f.ActiveEpoch == e.ActivationEpoch && f.CutSeq == e.CutCounter:
		return StartClass{Row: RowS3, Writable: true, Reason: fmt.Sprintf(
			"live current: installation %s host %s epoch %d cut %d", e.InstallationID, hostID, e.ActivationEpoch, e.CutCounter)}
	}
	return hold(RowS12, "data root %s store %s epoch %d cut %d, admin epoch %d cut %d host %s, server host %s (unsupported row)",
		dataRoot, f.RestoreState, f.ActiveEpoch, f.CutSeq, e.ActivationEpoch, e.CutCounter, e.HostID, hostID)
}
