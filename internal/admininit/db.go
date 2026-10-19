// Package admininit holds the offline init steps of `sori-admin init` (RW-C1 §5).
//
// This file is the database phase (RW-C1 §5.1/§5.1a): it classifies the job ledger
// and the authority database read-only, provisions their known schemas, and then
// writes the job ledger's four init markers once. It does not take the admin or
// server lock, and it does not create keys, policy or the activation epoch; the
// caller holds both locks for the whole run and does those steps afterwards.
package admininit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/HeaInSeo/sori/authority/sqlitestore"
	"github.com/HeaInSeo/sori/jobledger"
)

// ErrRefused is returned when either database is not in a state offline init may
// write to. Nothing has been written when it is returned from the preflight.
var ErrRefused = errors.New("admininit: init refused")

// Paths names the two databases of one data root. They are separate files; no
// transaction covers both.
type Paths struct {
	JobLedger string
	Authority string
}

// Result reports the database phase. Bootstrapped is false when the job ledger
// already had the init markers and Init was skipped.
type Result struct {
	InstallationID string
	Bootstrapped   bool
}

// Step names a point between two durable writes of ProvisionAndBootstrap.
type Step string

// The durable steps of ProvisionAndBootstrap, in order.
const (
	StepAuthoritySchema Step = "authority-schema"
	StepJobLedgerSchema Step = "jobledger-schema"
	StepMarkers         Step = "markers"
)

// afterStep, when set by tests, runs after each durable step. A non-nil error
// stops the run there, as a crash would.
var afterStep func(Step) error

// NewInstallationID returns a new installation id: 16 bytes from the OS CSPRNG as
// 32 lowercase hex digits (RW-C1 §5.1).
func NewInstallationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("admininit: installation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ProvisionAndBootstrap runs the database phase of offline init (RW-C1 §5.1a).
//
// It first inspects both databases read-only and refuses, writing nothing, unless
// the job ledger is PRISTINE, SCHEMA_ONLY or INIT_MARKER_EMPTY and the authority
// database is PRISTINE or SCHEMA_ONLY. A marker-empty ledger next to a pristine
// authority database is refused too: the markers are written only after both
// schemas exist, so that pair means the authority file was removed or replaced.
//
// It then creates each missing known schema through the version-pinned Open and
// checks that the result is SCHEMA_ONLY. Last, unless the ledger is already
// INIT_MARKER_EMPTY, it chooses the installation id with newID and writes the
// markers in Init's single transaction. A rerun after a crash at any step finds
// PRISTINE, SCHEMA_ONLY or INIT_MARKER_EMPTY again and resumes; after the marker
// commit it reuses the recorded id and never calls newID.
//
// newID nil means NewInstallationID.
func ProvisionAndBootstrap(ctx context.Context, p Paths, newID func() (string, error)) (Result, error) {
	if newID == nil {
		newID = NewInstallationID
	}
	if err := checkPaths(p); err != nil {
		return Result{}, err
	}
	ledger, auth, err := preflight(ctx, p)
	if err != nil {
		return Result{}, err
	}

	if auth == sqlitestore.InitPristine {
		if err := provisionAuthority(ctx, p.Authority); err != nil {
			return Result{}, err
		}
		if err := step(StepAuthoritySchema); err != nil {
			return Result{}, err
		}
	}
	if ledger.State == jobledger.InitPristine {
		if err := provisionJobLedger(ctx, p.JobLedger); err != nil {
			return Result{}, err
		}
		if err := step(StepJobLedgerSchema); err != nil {
			return Result{}, err
		}
	}
	if ledger.State == jobledger.InitMarkerEmpty {
		return Result{InstallationID: ledger.InstallationID}, nil
	}

	id, err := newID()
	if err != nil {
		return Result{}, err
	}
	if err := writeMarkers(ctx, p.JobLedger, id); err != nil {
		return Result{}, err
	}
	if err := step(StepMarkers); err != nil {
		return Result{}, err
	}
	return Result{InstallationID: id, Bootstrapped: true}, nil
}

func step(s Step) error {
	if afterStep == nil {
		return nil
	}
	return afterStep(s)
}

// checkPaths refuses an empty path or one database path used for both.
func checkPaths(p Paths) error {
	if p.JobLedger == "" || p.Authority == "" {
		return fmt.Errorf("%w: database path not set", ErrRefused)
	}
	l, err := filepath.Abs(p.JobLedger)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	a, err := filepath.Abs(p.Authority)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if l == a {
		return fmt.Errorf("%w: job ledger and authority share %s", ErrRefused, l)
	}
	return nil
}

// preflight classifies both databases before either is written.
func preflight(ctx context.Context, p Paths) (jobledger.InitInspection, sqlitestore.InitState, error) {
	ledger, err := jobledger.InspectInitState(ctx, p.JobLedger)
	if err != nil {
		return ledger, sqlitestore.InitRefused, fmt.Errorf("%w: job ledger: %w", ErrRefused, err)
	}
	auth, err := sqlitestore.InspectInitState(ctx, p.Authority)
	if err != nil {
		return ledger, auth, fmt.Errorf("%w: authority: %w", ErrRefused, err)
	}
	if ledger.State == jobledger.InitMarkerEmpty && auth != sqlitestore.InitSchemaOnly {
		return ledger, auth, fmt.Errorf("%w: job ledger has init markers but authority is %s", ErrRefused, auth)
	}
	return ledger, auth, nil
}

func provisionAuthority(ctx context.Context, path string) error {
	s, err := sqlitestore.Open(path)
	if err != nil {
		return fmt.Errorf("admininit: provision authority: %w", err)
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("admininit: provision authority: %w", err)
	}
	state, err := sqlitestore.InspectInitState(ctx, path)
	if err != nil {
		return fmt.Errorf("admininit: provision authority: %w", err)
	}
	if state != sqlitestore.InitSchemaOnly {
		return fmt.Errorf("%w: authority is %s after provisioning", ErrRefused, state)
	}
	return nil
}

func provisionJobLedger(ctx context.Context, path string) error {
	s, err := jobledger.Open(path)
	if err != nil {
		return fmt.Errorf("admininit: provision job ledger: %w", err)
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("admininit: provision job ledger: %w", err)
	}
	got, err := jobledger.InspectInitState(ctx, path)
	if err != nil {
		return fmt.Errorf("admininit: provision job ledger: %w", err)
	}
	if got.State != jobledger.InitSchemaOnly {
		return fmt.Errorf("%w: job ledger is %s after provisioning", ErrRefused, got.State)
	}
	return nil
}

// writeMarkers runs Init's marker transaction and checks the result read-only.
func writeMarkers(ctx context.Context, path, id string) error {
	s, err := jobledger.Open(path)
	if err != nil {
		return fmt.Errorf("admininit: init job ledger: %w", err)
	}
	initErr := s.Init(ctx, id)
	if err := errors.Join(initErr, s.Close()); err != nil {
		return fmt.Errorf("admininit: init job ledger: %w", err)
	}
	got, err := jobledger.InspectInitState(ctx, path)
	if err != nil {
		return fmt.Errorf("admininit: init job ledger: %w", err)
	}
	if got.State != jobledger.InitMarkerEmpty || got.InstallationID != id {
		return fmt.Errorf("%w: job ledger is %s/%q after init", ErrRefused, got.State, got.InstallationID)
	}
	return nil
}
