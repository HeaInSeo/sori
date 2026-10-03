// Package authoritytest is a backend-neutral conformance suite for authority.Store.
//
// Every Store backend (the in-memory reference and any future durable adapter) must
// pass the same suite, so the acceptance/reconcile, append-only alias history and
// Representation relation semantics are proven once and reused rather than
// re-derived per backend. The suite makes NO storage product/topology choice
// (SP-09/J2 remain open): it only drives the authority.Store contract through the
// production authority.Authority facade.
package authoritytest

import (
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

// Harness binds the suite to one Store backend.
type Harness struct {
	// New returns an empty Store. It is called once per case; backends that need
	// on-disk state should allocate it under t.TempDir().
	New func(t *testing.T) authority.Store
	// Reopen returns a NEW Store instance over the same durable state as s (for
	// example: close s and open the same database file again). A nil Reopen marks
	// durability as NOT IMPLEMENTED for the backend: the Reopen cases are skipped
	// with that explicit reason instead of being silently treated as passed.
	Reopen func(t *testing.T, s authority.Store) authority.Store
}

// conformanceCase is one contract check. It returns an error instead of failing t
// directly so the suite itself can be mutation-tested against deliberately broken
// stores.
type conformanceCase struct {
	name string
	run  func(t *testing.T, h Harness) error
}

// contractCases do not depend on durability.
var contractCases = []conformanceCase{
	{"AcceptRevision/IdempotentRetry", acceptIdempotentRetry},
	{"AcceptRevision/RequestConflict", acceptRequestConflict},
	{"AcceptRevision/DistinctRequests", acceptDistinctRequests},
	{"AcceptRevision/ConcurrentSameRequestID", acceptConcurrentSameRequest},
	{"AcceptRevision/ConcurrentSameRequestIDDifferentContent", acceptConcurrentConflictingRequest},
	{"AcceptRevision/MultiMember", acceptMultiMember},
	{"BindAlias/AppendOnlyHistoryOrder", bindAppendOnlyHistory},
	{"BindAlias/IdempotentRetryAndConflict", bindIdempotentAndConflict},
	{"BindAlias/SameBindRequestIDDifferentAliasOrAsset", bindIdentityConflict},
	{"BindAlias/ConcurrentSameBindRequestID", bindConcurrentSameRequest},
	{"BindAlias/ConcurrentSameBindRequestIDDifferentRevision", bindConcurrentConflictingRequest},
	{"AttachRepresentation/IdempotentRetryAndConflict", attachIdempotentAndConflict},
	{"AttachRepresentation/SameOperationIDDifferentRevision", attachCrossRevisionConflict},
	{"AttachRepresentation/ConcurrentSameOperationID", attachConcurrentSameOperation},
	{"AttachRepresentation/ConcurrentSameOperationIDDifferentFormat", attachConcurrentConflictingOperation},
	{"AttachRepresentation/MemberEquivalence", attachMemberEquivalence},
	{"AttachRepresentation/MultiMember", attachMultiMember},
	{"AttachRepresentation/ListOrder", attachListOrder},
	{"Representation/AvailabilityPreservesIdentity", representationAvailability},
	{"DeepCopy/Revision", deepCopyRevision},
	{"DeepCopy/Representation", deepCopyRepresentation},
	{"DeepCopy/AliasHistory", deepCopyAliasHistory},
}

// reopenCases require a real Reopen (Harness.Reopen != nil).
var reopenCases = []conformanceCase{
	{"Reopen/Revision", reopenRevision},
	{"Reopen/AliasHistory", reopenAliasHistory},
	{"Reopen/Representation", reopenRepresentation},
}

// Run executes the full conformance suite against h.
func Run(t *testing.T, h Harness) {
	t.Helper()
	if h.New == nil {
		t.Fatal("authoritytest: Harness.New is required")
	}
	for _, c := range contractCases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(t, h); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, c := range reopenCases {
		t.Run(c.name, func(t *testing.T) {
			if h.Reopen == nil {
				t.Skip("NOT IMPLEMENTED: backend has no Reopen (no durability claim)")
			}
			if err := c.run(t, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}
