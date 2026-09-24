package acquisition

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/HeaInSeo/sori/authority"
)

// Acquirer runs the I4A OCI pull-by-digest acquisition flow against a durable
// CheckpointStore and hands verified staged content to the authority for atomic
// acceptance.
type Acquirer struct {
	// Authority receives the atomic acceptance handoff. Required.
	Authority *authority.Authority
	// Checkpoints persists operation identity and progress. Required.
	Checkpoints CheckpointStore
	// StagingRoot is the local directory verified staged OCI layouts are
	// published into. Required.
	StagingRoot string
	// Credentials resolves Endpoint.Credential references. Required only when an
	// endpoint carries a non-empty CredentialRef.
	Credentials CredentialResolver
	// HTTPClient optionally overrides the registry HTTP client (tests). When nil
	// the registryutil retrying client is used.
	HTTPClient *http.Client
}

// Result reports the operation checkpoint and, once accepted, the Revision.
type Result struct {
	Operation Operation
	// Revision is set only when Operation.Phase == PhaseAccepted.
	Revision authority.Revision
}

// Accepted reports whether this operation produced an accepted Revision.
func (r Result) Accepted() bool { return r.Operation.Phase == PhaseAccepted }

// Acquire runs (or resumes) the logical acquisition identified by
// req.OperationID. It is safe to call repeatedly with the same request: an
// already-accepted operation returns its Revision without any transfer, a staged
// operation resumes at verification/acceptance, and an operation whose acceptance
// outcome is UNKNOWN reconciles through its persisted publication RequestID. On
// error the returned Result still carries the latest durable checkpoint.
func (a *Acquirer) Acquire(ctx context.Context, req Request) (Result, error) {
	if err := a.check(); err != nil {
		return Result{}, err
	}
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	// Pin the subject durably BEFORE any transfer: the operation record (with the
	// pinned manifest digest and the publication RequestID) is the first write.
	op, _, err := a.Checkpoints.Begin(ctx, newOperation(req))
	if err != nil {
		return Result{}, err
	}

	resumedStaged := op.Phase == PhaseStaged
	if op.Phase == PhasePinned {
		if op, err = a.stage(ctx, op, req.Endpoint); err != nil {
			return Result{Operation: op}, err
		}
	}
	if op.Phase == PhaseStaged {
		if op, err = a.handoff(ctx, op, resumedStaged); err != nil {
			return Result{Operation: op}, err
		}
	}
	if op.Phase == PhaseAcceptPending {
		if op, err = a.accept(ctx, op); err != nil {
			return Result{Operation: op}, err
		}
	}
	if op.Phase != PhaseAccepted {
		return Result{Operation: op}, fmt.Errorf("acquisition: operation %q in unexpected phase %q", op.ID, op.Phase)
	}
	rev, ok, err := a.Authority.GetRevision(ctx, op.RevisionID)
	if err != nil {
		return Result{Operation: op}, err
	}
	if !ok {
		return Result{Operation: op}, fmt.Errorf("%w: accepted operation %q references %q", authority.ErrRevisionNotFound, op.ID, op.RevisionID)
	}
	return Result{Operation: op, Revision: rev}, nil
}

func (a *Acquirer) check() error {
	if a == nil || a.Authority == nil || a.Checkpoints == nil || strings.TrimSpace(a.StagingRoot) == "" {
		return fmt.Errorf("%w: acquirer requires Authority, Checkpoints and StagingRoot", ErrInvalidRequest)
	}
	return nil
}

func newOperation(req Request) Operation {
	return Operation{
		ID:                   req.OperationID,
		Fingerprint:          operationFingerprint(req),
		AssetID:              req.AssetID,
		SourceCoordinate:     req.SourceCoordinate,
		Subject:              req.Subject,
		ObservedVersion:      req.observedVersion(),
		UpstreamBuilder:      req.UpstreamBuilder,
		Members:              append([]MemberDecl(nil), req.Members...),
		PublicationRequestID: PublicationRequestID(req.OperationID),
		Phase:                PhasePinned,
	}
}

// stage transfers the pinned subject from ep into a verified staged copy and
// checkpoints PhaseStaged. A failed attempt is recorded and leaves PhasePinned.
func (a *Acquirer) stage(ctx context.Context, op Operation, ep Endpoint) (Operation, error) {
	op.Attempts++
	stagedPath, members, err := a.transfer(ctx, op, ep)
	// Record the outcome even if ctx was canceled: the checkpoint must stay truthful.
	recordCtx := context.WithoutCancel(ctx)
	if err != nil {
		op.LastError = err.Error()
		if saved, uerr := a.Checkpoints.Update(recordCtx, op); uerr == nil {
			op = saved
		}
		return op, err
	}
	op.Phase = PhaseStaged
	op.StagedPath = stagedPath
	op.StagedMembers = members
	op.LastError = ""
	return a.saveOr(recordCtx, op)
}

// saveOr persists op and returns the stored record; on failure it returns op
// unchanged (the durable checkpoint is still the previous one) with the error.
func (a *Acquirer) saveOr(ctx context.Context, op Operation) (Operation, error) {
	saved, err := a.Checkpoints.Update(ctx, op)
	if err != nil {
		return op, err
	}
	return saved, nil
}

// handoff moves a staged operation to PhaseAcceptPending. A resumed staged copy is
// re-verified first; if it no longer matches the pinned digests the operation is
// reset to PhasePinned (fail closed) so a retry re-transfers.
func (a *Acquirer) handoff(ctx context.Context, op Operation, reverify bool) (Operation, error) {
	if reverify {
		members, err := verifyStaged(op.StagedPath, op.Subject, op.Members)
		if err == nil && !sameMembers(members, op.StagedMembers) {
			err = fmt.Errorf("%w: staged member proofs changed", ErrDigestMismatch)
		}
		if err != nil {
			removeStaged(op.StagedPath)
			op.Phase = PhasePinned
			op.StagedPath = ""
			op.StagedMembers = nil
			op.LastError = err.Error()
			if saved, uerr := a.Checkpoints.Update(context.WithoutCancel(ctx), op); uerr == nil {
				op = saved
			}
			return op, fmt.Errorf("%w: %w", ErrStagedInvalid, err)
		}
	}
	pending := op
	pending.Phase = PhaseAcceptPending
	saved, err := a.Checkpoints.Update(ctx, pending)
	if err != nil {
		return op, err
	}
	return saved, nil
}

// accept performs the atomic acceptance handoff under the operation's persisted
// publication RequestID. Until the Accepted checkpoint is written the outcome stays
// PhaseAcceptPending (explicit UNKNOWN); a retry reconciles to the same Revision.
func (a *Acquirer) accept(ctx context.Context, op Operation) (Operation, error) {
	rev, err := a.Authority.AcceptRevision(ctx, acceptRequest(op))
	if err != nil {
		op.LastError = err.Error()
		if saved, uerr := a.Checkpoints.Update(context.WithoutCancel(ctx), op); uerr == nil {
			op = saved
		}
		return op, err
	}
	accepted := op
	accepted.Phase = PhaseAccepted
	accepted.RevisionID = rev.RevisionID
	accepted.LastError = ""
	saved, err := a.Checkpoints.Update(context.WithoutCancel(ctx), accepted)
	if err != nil {
		// The Revision may be committed but the checkpoint is not: the operation
		// remains PhaseAcceptPending (UNKNOWN) and a retry reconciles.
		return op, fmt.Errorf("acquisition: record acceptance of %q: %w", op.ID, err)
	}
	return saved, nil
}

// acceptRequest builds the I4A-profile ExternalImport acceptance request. The
// pinned manifest digest is the ObservedChecksum (frozen subject proof); endpoint and
// credential never enter the manifest.
func acceptRequest(op Operation) authority.AcceptRequest {
	return authority.AcceptRequest{
		RequestID: op.PublicationRequestID,
		AssetID:   op.AssetID,
		Profile:   authority.ProfileI4AOCIDigest,
		Manifest: authority.SemanticManifest{
			Origin:  authority.OriginExternalImport,
			Members: append([]authority.Member(nil), op.StagedMembers...),
			Provenance: authority.Provenance{
				SourceCoordinate:     op.SourceCoordinate,
				ObservedVersion:      op.ObservedVersion,
				ObservedChecksum:     op.Subject.String(),
				UpstreamBuilderKnown: op.UpstreamBuilder != "",
				UpstreamBuilder:      op.UpstreamBuilder,
			},
		},
	}
}

func sameMembers(a, b []authority.Member) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// IsFailClosed reports whether err is an integrity failure (the subject could not be
// proven), as opposed to a transient transfer/availability failure.
func IsFailClosed(err error) bool {
	return errors.Is(err, ErrDigestMismatch) || errors.Is(err, ErrSubjectInvalid) || errors.Is(err, ErrStagedInvalid)
}
