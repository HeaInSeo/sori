// Package opsfacade is the internal bridge between the sori.ops.v1 JobService
// meaning (D01.4) and the jobledger store. It is not served: there is no gRPC
// server, Discover or CLI wiring here (W02.1a scope only).
//
// The facade reuses jobledger.Store Accept, Lookup and GetJob as they are and
// fixes only the call order and the wire projection. Every input the contract
// leaves to later accepted design (credential and principal extraction,
// namespace token MAC, server-verified request meaning of a frozen input,
// current authorization, cursor bytes, startup mode) comes in through a port.
// A nil port fails closed. No allow-all port exists outside tests.
package opsfacade

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"

	opsv1 "github.com/HeaInSeo/sori/api/sori/ops/v1"
	"github.com/HeaInSeo/sori/jobledger"
)

// ContractRevision is the only contract revision the facade speaks. It is not
// advertised anywhere: Discover is not part of this package.
const ContractRevision = 1

// Ledger is the subset of *jobledger.Store the facade uses.
type Ledger interface {
	Accept(ctx context.Context, g jobledger.Guard, req jobledger.AcceptRequest) (jobledger.Decision, error)
	Lookup(ctx context.Context, g jobledger.Guard, req jobledger.AcceptRequest) (jobledger.Decision, error)
	GetJob(ctx context.Context, id string) (jobledger.Job, bool, error)
}

// StartupState is what the server's startup checks concluded.
type StartupState struct {
	Writability opsv1.Writability
	Guard       jobledger.Guard
}

// Startup reports the current startup state.
type Startup interface {
	State(ctx context.Context) (StartupState, error)
}

// Caller is the authenticated caller. Principal comes from the verified
// transport credential, never from a request message.
type Caller struct {
	Principal string
}

// Operation names the facade call being authorized.
type Operation string

// Operations.
const (
	OpSubmitJob      Operation = "SubmitJob"
	OpLookupJobByKey Operation = "LookupJobByKey"
	OpGetJob         Operation = "GetJob"
)

// AuthDecision is a current authorization answer. The zero value is UNKNOWN.
type AuthDecision int

// Authorization decisions.
const (
	AuthUnknown AuthDecision = iota
	AuthAllow
	AuthDeny
)

// AuthRequest is one authorization question. Spec is set for SubmitJob and
// LookupJobByKey, JobID for GetJob.
type AuthRequest struct {
	Operation Operation
	Spec      *opsv1.JobSpec
	JobID     string
}

// Authorizer answers from current policy. Past acceptance is not an answer.
type Authorizer interface {
	Authorize(ctx context.Context, c Caller, r AuthRequest) (AuthDecision, error)
}

// Namespaces verifies an opaque namespace token for the caller.
type Namespaces interface {
	Verify(ctx context.Context, c Caller, token string) (jobledger.NamespaceRef, error)
}

// VerifiedMeaning is the server-computed request meaning of a spec, computed
// from server-verified state (for LOCAL_IMPORT, the frozen input), not from the
// client digest.
type VerifiedMeaning struct {
	SHA256 []byte
	Kind   string
}

// Meanings computes the server-side request meaning. A refusal the contract
// types (MEMBER_KIND_REJECTED, TRANSFER_NOT_FROZEN, FIELD_UNKNOWN,
// FIELD_INVALID) is returned as an *Error with that reason; any other error
// means the meaning cannot be verified now and maps to RO_HOLD.
type Meanings interface {
	Meaning(ctx context.Context, c Caller, spec *opsv1.JobSpec) (VerifiedMeaning, error)
}

// CursorVerdict classifies an opaque cursor. The zero value is INVALID.
type CursorVerdict int

// Cursor verdicts.
const (
	CursorInvalid CursorVerdict = iota
	CursorValid
	CursorExpired
)

// Cursors classifies a cursor against the caller and the current read basis.
type Cursors interface {
	Classify(ctx context.Context, c Caller, cursor string) (CursorVerdict, error)
}

// Facade holds the ledger and the ports. The zero value refuses everything.
type Facade struct {
	Ledger     Ledger
	Startup    Startup
	Authorizer Authorizer
	Namespaces Namespaces
	Meanings   Meanings
	Cursors    Cursors
}

// Error is a typed failure carrying the contract's ErrorReason.
type Error struct {
	Reason opsv1.ErrorReason
}

func (e *Error) Error() string { return "opsfacade: " + e.Reason.String() }

func fail(r opsv1.ErrorReason) error { return &Error{Reason: r} }

// ReasonOf returns the ErrorReason of err, or UNSPECIFIED.
func ReasonOf(err error) opsv1.ErrorReason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return opsv1.ErrorReason_ERROR_REASON_UNSPECIFIED
}

// ErrNotFound is returned by GetJob for a missing Job and, with the same
// value, for a Job the caller may not see.
var ErrNotFound = errors.New("opsfacade: job not found")

// errInternal hides ledger detail from the wire.
var errInternal = errors.New("opsfacade: internal error")

// SubmitResult is a SubmitJob answer.
type SubmitResult struct {
	Outcome opsv1.SubmitOutcome
	Job     *opsv1.Job
}

// LookupResult is a LookupJobByKey answer. Job is set only for FOUND.
type LookupResult struct {
	Result opsv1.KeyLookupResult
	Job    *opsv1.Job
}

// prepared is the outcome of the shared preamble.
type prepared struct {
	guard jobledger.Guard
	req   jobledger.AcceptRequest
}

// prepare runs the D01.4-r3 §1.2 check order before any ledger access:
// authenticated caller, header and startup mode, structure, namespace token,
// current authorization, server-verified meaning and the client cross-check.
func (f *Facade) prepare(ctx context.Context, c Caller, op Operation, h *opsv1.RequestHeader,
	key *opsv1.OperationKey, spec *opsv1.JobSpec) (prepared, error) {
	if c.Principal == "" {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_AUTH_REQUIRED)
	}
	st, err := f.startup(ctx, h, op == OpSubmitJob)
	if err != nil {
		return prepared{}, err
	}
	if key == nil || spec == nil || key.GetIdempotencyKey() == "" {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_FIELD_UNSPECIFIED)
	}
	if !validIdempotencyKey(key.GetIdempotencyKey()) {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID)
	}
	if f.Namespaces == nil {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_NAMESPACE_INVALID)
	}
	ns, err := f.Namespaces.Verify(ctx, c, key.GetNamespaceId())
	if err != nil {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_NAMESPACE_INVALID)
	}
	if err := f.authorize(ctx, c, AuthRequest{Operation: op, Spec: spec}); err != nil {
		return prepared{}, err
	}
	if f.Meanings == nil {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_RO_HOLD)
	}
	m, err := f.Meanings.Meaning(ctx, c, spec)
	if err != nil {
		return prepared{}, meaningFailure(err)
	}
	if len(m.SHA256) != 32 || m.Kind == "" {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_RO_HOLD)
	}
	if !bytes.Equal(m.SHA256, key.GetRequestMeaningSha256()) {
		return prepared{}, fail(opsv1.ErrorReason_ERROR_REASON_MEANING_DIGEST_MISMATCH)
	}
	return prepared{guard: st.Guard, req: jobledger.AcceptRequest{
		Namespace: ns, Principal: c.Principal, IdempotencyKey: key.GetIdempotencyKey(),
		MeaningDigest: "sha256:" + hex.EncodeToString(m.SHA256), Kind: m.Kind,
	}}, nil
}

// maxIdempotencyKeyBytes is the common.proto bound on OperationKey.idempotency_key.
const maxIdempotencyKeyBytes = 128

// validIdempotencyKey reports whether k is 1..128 bytes of [A-Za-z0-9._~-],
// the form common.proto defines for OperationKey.idempotency_key.
func validIdempotencyKey(k string) bool {
	if len(k) == 0 || len(k) > maxIdempotencyKeyBytes {
		return false
	}
	for i := 0; i < len(k); i++ {
		switch c := k[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '.', c == '_', c == '~', c == '-':
		default:
			return false
		}
	}
	return true
}

// meaningReasons are the typed verifier refusals the contract defines for a
// JobSpec the server cannot accept as given. A Meanings port that returns one
// of them as an *Error keeps that reason.
var meaningReasons = map[opsv1.ErrorReason]bool{
	opsv1.ErrorReason_ERROR_REASON_MEMBER_KIND_REJECTED: true,
	opsv1.ErrorReason_ERROR_REASON_TRANSFER_NOT_FROZEN:  true,
	opsv1.ErrorReason_ERROR_REASON_FIELD_UNKNOWN:        true,
	opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID:        true,
}

// meaningFailure maps a Meanings port error. A typed verifier refusal keeps
// its reason; any other error (unavailable state, untyped or out-of-set
// reason) is RO_HOLD.
func meaningFailure(err error) error {
	var e *Error
	if errors.As(err, &e) && meaningReasons[e.Reason] {
		return fail(e.Reason)
	}
	return fail(opsv1.ErrorReason_ERROR_REASON_RO_HOLD)
}

func (f *Facade) startup(ctx context.Context, h *opsv1.RequestHeader, mutation bool) (StartupState, error) {
	if h.GetContractRevision() != ContractRevision {
		return StartupState{}, fail(opsv1.ErrorReason_ERROR_REASON_UNSUPPORTED_CONTRACT)
	}
	if f.Ledger == nil || f.Startup == nil {
		return StartupState{}, fail(opsv1.ErrorReason_ERROR_REASON_RO_HOLD)
	}
	st, err := f.Startup.State(ctx)
	if err != nil {
		return StartupState{}, fail(opsv1.ErrorReason_ERROR_REASON_RO_HOLD)
	}
	inst := h.GetInstallation()
	if inst.GetInstallationId() == "" || inst.GetInstallationId() != st.Guard.InstallationID {
		return StartupState{}, fail(opsv1.ErrorReason_ERROR_REASON_INSTALLATION_MISMATCH)
	}
	if !mutation {
		return st, nil
	}
	if st.Writability != opsv1.Writability_WRITABILITY_RW {
		return StartupState{}, fail(opsv1.ErrorReason_ERROR_REASON_RO_HOLD)
	}
	if inst.GetActivationEpoch() != st.Guard.ActivationEpoch {
		return StartupState{}, fail(opsv1.ErrorReason_ERROR_REASON_FENCE_STALE)
	}
	return st, nil
}

func (f *Facade) authorize(ctx context.Context, c Caller, r AuthRequest) error {
	if c.Principal == "" {
		return fail(opsv1.ErrorReason_ERROR_REASON_AUTH_REQUIRED)
	}
	if f.Authorizer == nil {
		return fail(opsv1.ErrorReason_ERROR_REASON_AUTH_PROVIDER_UNKNOWN)
	}
	d, err := f.Authorizer.Authorize(ctx, c, r)
	switch {
	case err != nil:
		return fail(opsv1.ErrorReason_ERROR_REASON_AUTH_PROVIDER_UNKNOWN)
	case d == AuthAllow:
		return nil
	case d == AuthDeny:
		return fail(opsv1.ErrorReason_ERROR_REASON_AUTH_DENIED)
	default:
		return fail(opsv1.ErrorReason_ERROR_REASON_AUTH_PROVIDER_UNKNOWN)
	}
}

// SubmitJob accepts a Job through jobledger.Store.Accept.
func (f *Facade) SubmitJob(ctx context.Context, c Caller, r *opsv1.SubmitJobRequest) (SubmitResult, error) {
	p, err := f.prepare(ctx, c, OpSubmitJob, r.GetHeader(), r.GetKey(), r.GetSpec())
	if err != nil {
		return SubmitResult{}, err
	}
	d, err := f.Ledger.Accept(ctx, p.guard, p.req)
	if err != nil {
		return SubmitResult{}, ledgerError(err)
	}
	var outcome opsv1.SubmitOutcome
	switch d.Outcome {
	case jobledger.OutcomeNew:
		outcome = opsv1.SubmitOutcome_SUBMIT_OUTCOME_NEW
	case jobledger.OutcomeExisting:
		outcome = opsv1.SubmitOutcome_SUBMIT_OUTCOME_EXISTING
	default:
		return SubmitResult{}, errInternal
	}
	if d.Job == nil {
		return SubmitResult{}, errInternal
	}
	job, err := ProjectJob(*d.Job, true)
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Outcome: outcome, Job: job}, nil
}

// LookupJobByKey classifies a key through jobledger.Store.Lookup. Only FOUND
// carries a Job; the ledger's Decision.Job of a CONFLICT is never projected.
func (f *Facade) LookupJobByKey(ctx context.Context, c Caller, r *opsv1.LookupJobByKeyRequest) (LookupResult, error) {
	p, err := f.prepare(ctx, c, OpLookupJobByKey, r.GetHeader(), r.GetKey(), r.GetSpec())
	if err != nil {
		return LookupResult{}, err
	}
	d, err := f.Ledger.Lookup(ctx, p.guard, p.req)
	if err != nil {
		return LookupResult{}, ledgerError(err)
	}
	switch d.Outcome {
	case jobledger.OutcomeExisting:
		if d.Job == nil {
			return LookupResult{}, errInternal
		}
		job, err := ProjectJob(*d.Job, true)
		if err != nil {
			return LookupResult{}, err
		}
		return LookupResult{Result: opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_FOUND, Job: job}, nil
	case jobledger.OutcomeNew:
		return LookupResult{Result: opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_NOT_FOUND}, nil
	case jobledger.OutcomeConflict:
		return LookupResult{Result: opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_CONFLICT}, nil
	case jobledger.OutcomeHold:
		return LookupResult{Result: opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_RECONCILE_HOLD}, nil
	case jobledger.OutcomeInvalid:
		return LookupResult{}, fail(opsv1.ErrorReason_ERROR_REASON_NAMESPACE_INVALID)
	default:
		return LookupResult{}, errInternal
	}
}

// GetJob reads a Job through jobledger.Store.GetJob. A denied caller gets the
// same ErrNotFound as a missing Job.
func (f *Facade) GetJob(ctx context.Context, c Caller, r *opsv1.GetJobRequest) (*opsv1.Job, error) {
	if _, err := f.startup(ctx, r.GetHeader(), false); err != nil {
		return nil, err
	}
	if r.GetJobId() == "" {
		return nil, fail(opsv1.ErrorReason_ERROR_REASON_FIELD_UNSPECIFIED)
	}
	if err := f.authorize(ctx, c, AuthRequest{Operation: OpGetJob, JobID: r.GetJobId()}); err != nil {
		if ReasonOf(err) == opsv1.ErrorReason_ERROR_REASON_AUTH_DENIED {
			return nil, ErrNotFound
		}
		return nil, err
	}
	j, ok, err := f.Ledger.GetJob(ctx, r.GetJobId())
	switch {
	case err != nil:
		return nil, errInternal
	case !ok:
		return nil, ErrNotFound
	}
	// A read by id does not prove the key mapping, so acceptance stays UNKNOWN.
	return ProjectJob(j, false)
}

// CheckCursor accepts only a cursor the port classifies as valid. Every other
// cursor, including any cursor when no port is configured, is
// CURSOR_INVALID_RELIST: never a silent continuation or an empty page.
func (f *Facade) CheckCursor(ctx context.Context, c Caller, cursor string) error {
	if f.Cursors == nil {
		return fail(opsv1.ErrorReason_ERROR_REASON_CURSOR_INVALID_RELIST)
	}
	v, err := f.Cursors.Classify(ctx, c, cursor)
	if err != nil || v != CursorValid {
		return fail(opsv1.ErrorReason_ERROR_REASON_CURSOR_INVALID_RELIST)
	}
	return nil
}

func ledgerError(err error) error {
	switch {
	case errors.Is(err, jobledger.ErrIdempotencyConflict):
		return fail(opsv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_CONFLICT)
	case errors.Is(err, jobledger.ErrNamespaceInvalid):
		return fail(opsv1.ErrorReason_ERROR_REASON_NAMESPACE_INVALID)
	case errors.Is(err, jobledger.ErrReconcileHold):
		return fail(opsv1.ErrorReason_ERROR_REASON_RECONCILE_HOLD)
	case errors.Is(err, jobledger.ErrFenceStale):
		return fail(opsv1.ErrorReason_ERROR_REASON_FENCE_STALE)
	case errors.Is(err, jobledger.ErrInstallationMismatch):
		return fail(opsv1.ErrorReason_ERROR_REASON_INSTALLATION_MISMATCH)
	case errors.Is(err, jobledger.ErrReadOnlyHold), errors.Is(err, jobledger.ErrNotInitialized):
		return fail(opsv1.ErrorReason_ERROR_REASON_RO_HOLD)
	default:
		return errInternal
	}
}

var phases = map[jobledger.Phase]opsv1.JobPhase{
	jobledger.PhaseQueued:          opsv1.JobPhase_JOB_PHASE_QUEUED,
	jobledger.PhaseRunning:         opsv1.JobPhase_JOB_PHASE_RUNNING,
	jobledger.PhaseCancelRequested: opsv1.JobPhase_JOB_PHASE_CANCEL_REQUESTED,
	jobledger.PhaseSucceeded:       opsv1.JobPhase_JOB_PHASE_SUCCEEDED,
	jobledger.PhaseFailed:          opsv1.JobPhase_JOB_PHASE_FAILED,
	jobledger.PhaseCancelled:       opsv1.JobPhase_JOB_PHASE_CANCELLED,
	jobledger.PhaseHold:            opsv1.JobPhase_JOB_PHASE_HOLD,
}

var kinds = map[string]opsv1.JobKind{
	"LOCAL_IMPORT": opsv1.JobKind_JOB_KIND_LOCAL_IMPORT,
	"OCI_ACQUIRE":  opsv1.JobKind_JOB_KIND_OCI_ACQUIRE,
	"EXPORT":       opsv1.JobKind_JOB_KIND_EXPORT,
}

// ProjectJob maps a stored Job to the wire Job. mappingProven says the read
// went through the durable key mapping; only then is acceptance ACCEPTED.
// The ledger stores no effect or verification evidence, so both are UNKNOWN,
// whatever the phase or terminal receipt says. The legacy LastError string is
// not mapped onto a typed reason. An unknown kind or phase is an error.
func ProjectJob(j jobledger.Job, mappingProven bool) (*opsv1.Job, error) {
	phase, ok := phases[j.Phase]
	if !ok {
		return nil, errInternal
	}
	kind, ok := kinds[j.Kind]
	if !ok {
		return nil, errInternal
	}
	acceptance := opsv1.AcceptanceState_ACCEPTANCE_STATE_UNKNOWN
	if mappingProven {
		acceptance = opsv1.AcceptanceState_ACCEPTANCE_STATE_ACCEPTED
	}
	return &opsv1.Job{
		JobId:                  j.JobID,
		Kind:                   kind,
		Phase:                  phase,
		Acceptance:             acceptance,
		Effect:                 opsv1.EffectState_EFFECT_STATE_UNKNOWN,
		Verification:           opsv1.VerificationState_VERIFICATION_STATE_UNKNOWN,
		RevisionId:             j.RevisionID,
		CreatedActivationEpoch: j.CreatedEpoch,
		RecordVersion:          j.RecordVersion,
	}, nil
}
