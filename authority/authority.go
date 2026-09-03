// Package authority implements the SORI-I1M "Acceptance + Minimum Semantic
// Manifest Spine": the smallest durable Sori Asset Authority that can accept one
// genuinely valid immutable Asset Revision.
//
// Scope boundary (SORI-I1M §8): this package intentionally does NOT model
// representations/attachments, acquisition/staging, Pipeline/DagEdit contracts,
// lifecycle status taxonomy, rebinding policy, a full validation/attestation
// subsystem, UI, or any public API/schema/ID-allocator/hash-algorithm. It also
// makes NO service/DB/topology commitment: durability is expressed only through the
// Store abstraction.
//
// Identity note (SORI-I1M §10): AssetID / RevisionID / RequestID and the OriginKind
// enum are INTERNAL, opaque handles. They are deliberately not a public identifier
// algorithm or wire schema and may change without external impact. The internal
// comparison Fingerprint (fingerprint.go) is acceptance evidence only and never
// becomes the public Revision-ID/hash.
package authority

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AssetID is the internal, stable Asset identity — distinct from any alias, name,
// or version (SORI-I1M §3). It is an opaque handle owned by the caller/platform;
// this package never derives it from a public algorithm.
type AssetID string

// RevisionID is the immutable identity of one accepted Asset Revision. It is an
// opaque, store-assigned token, NOT a public identifier and NOT the Fingerprint.
type RevisionID string

// RequestID is a durable PublicationOperation identity. The same RequestID retried
// after a lost response must reconcile to the same outcome (idempotency).
type RequestID string

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrInvalidManifest reports a structurally invalid acceptance request.
	ErrInvalidManifest = errors.New("authority: invalid semantic manifest")
	// ErrRequestConflict reports the same RequestID re-used with a different
	// asset/identity-bearing fingerprint — accepted fail-closed, never overwritten.
	ErrRequestConflict = errors.New("authority: request id conflicts with a prior acceptance")
	// ErrAliasBindingConflict reports the same alias-binding operation id re-used
	// for a different logical binding.
	ErrAliasBindingConflict = errors.New("authority: alias binding operation id conflicts with a prior binding")
	// ErrRevisionNotFound reports an unknown RevisionID.
	ErrRevisionNotFound = errors.New("authority: revision not found")
)

// AcceptRequest is one publication operation requesting acceptance of an immutable
// Asset Revision under AssetID.
type AcceptRequest struct {
	RequestID RequestID
	AssetID   AssetID
	Manifest  SemanticManifest
}

// Revision is an accepted, immutable Asset Revision. Its fields are never mutated
// after acceptance (SORI-I1M §8).
type Revision struct {
	RevisionID  RevisionID
	AssetID     AssetID
	RequestID   RequestID
	Fingerprint string // internal comparison evidence only; not a public id
	Manifest    SemanticManifest
	AcceptedAt  time.Time
}

// BindRequest is an operation-identified request to (re)bind an alias to a Revision.
// BindRequestID gives the binding its own durable identity so an ack-loss retry
// converges to the same append-only history event without duplication (SORI-I1M §4).
type BindRequest struct {
	BindRequestID RequestID
	Alias         string
	AssetID       AssetID
	RevisionID    RevisionID
}

// BindEvent is one entry in the append-only alias→Revision binding history.
type BindEvent struct {
	BindRequestID RequestID
	Alias         string
	AssetID       AssetID
	RevisionID    RevisionID
	Sequence      int
	BoundAt       time.Time
}

// Authority is the embeddable Sori Asset Authority facade. It performs pure request
// validation and delegates the atomic acceptance/reconcile transaction and the
// durable append-only alias history to a Store.
type Authority struct {
	store Store
}

// New constructs an Authority over the given durable Store.
func New(store Store) *Authority {
	return &Authority{store: store}
}

// AcceptRevision validates the request, computes the internal identity-bearing
// fingerprint, and atomically accepts (or idempotently reconciles) exactly one
// immutable Revision. Reconcile semantics (delegated to the Store, applied
// atomically with the commit):
//   - same RequestID + same asset + same fingerprint → the same Revision;
//   - same RequestID + a different asset/fingerprint → ErrRequestConflict.
//
// A structurally invalid request (missing members/proofs/origin-required
// provenance) is rejected fail-closed before any store mutation.
func (a *Authority) AcceptRevision(ctx context.Context, req AcceptRequest) (Revision, error) {
	if err := validateAcceptRequest(req); err != nil {
		return Revision{}, err
	}
	fingerprint := computeFingerprint(req.AssetID, req.Manifest)
	return a.store.AcceptRevision(ctx, req, fingerprint)
}

// BindAlias atomically appends an operation-identified alias→Revision binding to the
// durable append-only history. A retry with the same BindRequestID and the same
// logical binding returns the original event (no duplicate history); the same
// BindRequestID with a different binding is rejected fail-closed.
func (a *Authority) BindAlias(ctx context.Context, req BindRequest) (BindEvent, error) {
	if req.BindRequestID == "" || req.Alias == "" || req.AssetID == "" || req.RevisionID == "" {
		return BindEvent{}, ErrInvalidManifest
	}
	return a.store.BindAlias(ctx, req)
}

// GetRevision returns an accepted Revision by id (SORI-I1M §8 allows only an
// internal test-only exact lookup; no Pipeline contract).
func (a *Authority) GetRevision(ctx context.Context, id RevisionID) (Revision, bool, error) {
	return a.store.GetRevision(ctx, id)
}

// AliasHistory returns the append-only binding history for an alias, oldest first.
func (a *Authority) AliasHistory(ctx context.Context, alias string) ([]BindEvent, error) {
	return a.store.AliasHistory(ctx, alias)
}

// AttachRepresentation (SORI-I2R) attaches a physical Representation to an accepted
// immutable Revision as a durable append-only relation. It is idempotent by
// AttachOperationID; the same operation re-used with a different immutable relation
// (representation) semantics fails closed (ErrAttachConflict) rather than rewriting.
//
// The Revision must already be accepted (authority truth is never inferred from a
// physical push), and the representation MUST prove semantic member equivalence to the
// Revision's logical member contract (same semantic keys + same authoritative
// content-proof digests) — a path/tag/locator alone is insufficient. The accepted
// Revision is never mutated by an attach.
func (a *Authority) AttachRepresentation(ctx context.Context, req AttachRequest) (Representation, error) {
	if err := validateAttachRequest(req); err != nil {
		return Representation{}, err
	}
	rev, ok, err := a.store.GetRevision(ctx, req.RevisionID)
	if err != nil {
		return Representation{}, err
	}
	if !ok || rev.AssetID != req.AssetID {
		return Representation{}, fmt.Errorf("%w: %q", ErrRevisionNotFound, req.RevisionID)
	}
	if !membersEquivalent(req.MemberProofs, rev.Manifest.Members) {
		return Representation{}, ErrMemberEquivalence
	}
	fingerprint := computeRepresentationFingerprint(req.Format, req.MemberProofs)
	return a.store.AttachRepresentation(ctx, req, fingerprint)
}

// SetRepresentationLocators replaces a Representation's mutable availability
// coordinates (registry/tag/path/replica). This never changes the Representation's or
// the Revision's semantic identity.
func (a *Authority) SetRepresentationLocators(ctx context.Context, id RepresentationID, locators []Locator) error {
	return a.store.SetRepresentationLocators(ctx, id, locators)
}

// SetRepresentationHealth sets a Representation's mutable availability flag. Losing or
// degrading a representation never mutates or retracts the accepted Revision.
func (a *Authority) SetRepresentationHealth(ctx context.Context, id RepresentationID, healthy bool) error {
	return a.store.SetRepresentationHealth(ctx, id, healthy)
}

// GetRepresentation returns an attached Representation by id.
func (a *Authority) GetRepresentation(ctx context.Context, id RepresentationID) (Representation, bool, error) {
	return a.store.GetRepresentation(ctx, id)
}

// ListRepresentations returns the Representations attached to a Revision, in attach
// order. A Revision with zero Representations remains valid authority truth (it is
// simply not implied runnable/available).
func (a *Authority) ListRepresentations(ctx context.Context, revID RevisionID) ([]Representation, error) {
	return a.store.ListRepresentations(ctx, revID)
}
