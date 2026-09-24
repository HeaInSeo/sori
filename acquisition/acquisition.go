// Package acquisition implements the SORI-I4A first bounded acquisition slice: OCI
// registry pull-by-digest as the first reference transport.
//
// The flow for one logical acquisition operation is
//
//	pin subject → (durable checkpoint) → stage → verify → atomic accept handoff
//
// and every step is recorded in a CheckpointStore so a crash/retry either resumes from
// a recoverable checkpoint or surfaces an explicit UNKNOWN (PhaseAcceptPending).
//
// Invariants:
//   - The immutable acquisition subject is the OCI manifest digest. It is pinned into
//     the operation record BEFORE any transfer and is the frozen subject proof; a
//     mutable tag (including "latest") is never authority — at most an observed-version
//     evidence string.
//   - The Endpoint (registry/repository/plain-http) and its CredentialRef are mutable
//     availability coordinates. They are NOT part of the operation identity, so an
//     endpoint/credential rotation on retry continues the same operation.
//   - Staged bytes (manifest and every member blob) must hash to the pinned digests;
//     any mismatch fails closed and nothing is accepted.
//   - Transfer success alone is never an accepted Revision: only the authority's
//     atomic AcceptRevision produces one, under a publication RequestID that is minted
//     once per operation and kept distinct from the acquisition OperationID.
//
// Scope boundary: this slice deliberately does NOT add HTTPS/FTP/object-store/
// shared-FS transports, UI, or any production DB/topology choice. CheckpointStore is
// an abstraction; MemoryCheckpointStore is a reference implementation only.
package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/HeaInSeo/sori/authority"
)

// OperationID is the durable identity of one logical acquisition. Retrying the same
// logical acquisition MUST reuse the same OperationID; it never mints a second
// operation or a second Revision.
type OperationID string

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrInvalidRequest reports a structurally invalid acquisition request, including
	// a subject that is not a digest (tags are never authority).
	ErrInvalidRequest = errors.New("acquisition: invalid request")
	// ErrOperationConflict reports an OperationID re-used for a different logical
	// acquisition (different asset/source/subject/members/observed version).
	ErrOperationConflict = errors.New("acquisition: operation id conflicts with a prior acquisition")
	// ErrDigestMismatch reports staged content (manifest or member blob) that does not
	// match its pinned digest. The acquisition fails closed; nothing is accepted.
	ErrDigestMismatch = errors.New("acquisition: staged content does not match pinned digest")
	// ErrSubjectInvalid reports a pinned subject whose manifest is unsupported or does
	// not close over the declared members.
	ErrSubjectInvalid = errors.New("acquisition: pinned subject does not satisfy the declared member contract")
	// ErrStagedInvalid reports that a previously staged copy failed re-verification on
	// resume. The operation is reset to PhasePinned so a retry re-transfers.
	ErrStagedInvalid = errors.New("acquisition: staged copy failed re-verification")
	// ErrCheckpointStale reports an optimistic-concurrency conflict on a checkpoint
	// update (another worker advanced the operation).
	ErrCheckpointStale = errors.New("acquisition: checkpoint version is stale")
	// ErrCredentialUnavailable reports a CredentialRef that could not be resolved.
	ErrCredentialUnavailable = errors.New("acquisition: credential reference could not be resolved")
)

// CredentialRef is a reference/locator for a credential (e.g. a secret name). It is
// never a secret value and is never part of the immutable subject identity.
type CredentialRef string

// CredentialResolver resolves a CredentialRef to a registry credential at transfer
// time. It is supplied by the embedding platform; credential values are never stored
// in checkpoints or provenance.
type CredentialResolver func(ctx context.Context, ref CredentialRef) (auth.Credential, error)

// Endpoint is the mutable availability coordinate an OCI subject is pulled from. It
// may change between retries of the same operation (mirror/credential rotation).
type Endpoint struct {
	// Repository is the OCI repository reference without tag/digest, e.g.
	// "registry.example.com/ns/dataset".
	Repository string
	// PlainHTTP selects plain HTTP (test/lab registries only).
	PlainHTTP bool
	// Credential is an optional credential reference; empty means anonymous.
	Credential CredentialRef
}

// MemberDecl declares one expected member of the subject: the manifest layer whose
// org.opencontainers.image.title annotation equals SemanticKey, plus its authoritative
// semantic declarations. The content proof is filled from the verified layer digest.
type MemberDecl struct {
	SemanticKey string
	Role        string
	DataFormat  string
	Cardinality authority.Cardinality
}

// Request is one acquisition attempt for a logical acquisition operation.
type Request struct {
	OperationID OperationID
	AssetID     authority.AssetID
	// SourceCoordinate is the stable, logical upstream coordinate recorded as
	// provenance. It must not encode the endpoint/credential in use.
	SourceCoordinate string
	// Subject is the pinned OCI manifest digest — the frozen acquisition subject.
	Subject digest.Digest
	// ObservedVersion is optional evidence (e.g. the tag the digest was observed
	// under). It is never authority. When empty the pinned digest is recorded.
	ObservedVersion string
	// UpstreamBuilder, when non-empty, is recorded as a known upstream builder;
	// otherwise the upstream builder is recorded as explicitly UNKNOWN.
	UpstreamBuilder string
	Members         []MemberDecl
	// Endpoint is where the subject is pulled from on THIS attempt.
	Endpoint Endpoint
}

func (r Request) validate() error {
	if strings.TrimSpace(string(r.OperationID)) == "" {
		return fmt.Errorf("%w: empty operation id", ErrInvalidRequest)
	}
	if r.AssetID == "" {
		return fmt.Errorf("%w: empty asset id", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.SourceCoordinate) == "" {
		return fmt.Errorf("%w: empty source coordinate", ErrInvalidRequest)
	}
	if err := r.Subject.Validate(); err != nil {
		return fmt.Errorf("%w: subject must be a pinned manifest digest (tags are not authority): %v", ErrInvalidRequest, err)
	}
	if strings.TrimSpace(r.Endpoint.Repository) == "" || strings.ContainsAny(r.Endpoint.Repository, "@") {
		return fmt.Errorf("%w: endpoint repository must be a bare repository reference", ErrInvalidRequest)
	}
	return validateMemberDecls(r.Members)
}

func validateMemberDecls(members []MemberDecl) error {
	if len(members) == 0 {
		return fmt.Errorf("%w: no declared members", ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if strings.TrimSpace(m.SemanticKey) == "" || strings.TrimSpace(m.Role) == "" {
			return fmt.Errorf("%w: member missing semantic key/role", ErrInvalidRequest)
		}
		if _, dup := seen[m.SemanticKey]; dup {
			return fmt.Errorf("%w: duplicate member %q", ErrInvalidRequest, m.SemanticKey)
		}
		seen[m.SemanticKey] = struct{}{}
	}
	return nil
}

// observedVersion returns the observed-version evidence to record.
func (r Request) observedVersion() string {
	if strings.TrimSpace(r.ObservedVersion) != "" {
		return r.ObservedVersion
	}
	return r.Subject.String()
}

// operationFingerprint canonicalizes the identity-bearing facts of a logical
// acquisition. The Endpoint and CredentialRef are deliberately EXCLUDED so a mirror
// or credential rotation continues the same operation.
func operationFingerprint(r Request) string {
	members := append([]MemberDecl(nil), r.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].SemanticKey < members[j].SemanticKey })
	payload := struct {
		Asset           string       `json:"asset"`
		Source          string       `json:"source"`
		Subject         string       `json:"subject"`
		ObservedVersion string       `json:"observedVersion"`
		UpstreamBuilder string       `json:"upstreamBuilder"`
		Members         []MemberDecl `json:"members"`
	}{
		Asset:           string(r.AssetID),
		Source:          r.SourceCoordinate,
		Subject:         r.Subject.String(),
		ObservedVersion: r.observedVersion(),
		UpstreamBuilder: r.UpstreamBuilder,
		Members:         members,
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// PublicationRequestID derives the authority publication RequestID for an
// acquisition operation. It is namespaced so it can never collide with an
// OperationID-shaped RequestID used by a non-acquisition (I1M) publisher, and it is
// minted once per operation and persisted in the checkpoint.
func PublicationRequestID(id OperationID) authority.RequestID {
	return authority.RequestID("sori-i4a-publication/" + string(id))
}
