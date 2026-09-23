package authority

import (
	"context"
	"fmt"
)

// ResolveRevisionMember (SORI-I3P) resolves one semantic member of an exact accepted
// Revision, together with the Representations attached to that Revision.
//
// Resolution is pinned to (assetID, revID); it never follows an alias, so a later
// alias rebind cannot change the result. It fails closed with typed errors:
//   - an unknown revID, or a revID that belongs to a different asset →
//     ErrRevisionNotFound;
//   - a semanticKey that is not a member of the Revision → ErrMemberNotFound.
//
// The returned Member carries the authoritative DataFormat and Cardinality accepted
// with the Revision. Representations are returned unfiltered in attach order,
// including unhealthy ones, and may be empty: Sori makes no future-use eligibility
// decision here.
func (a *Authority) ResolveRevisionMember(ctx context.Context, assetID AssetID, revID RevisionID, semanticKey string) (Member, []Representation, error) {
	rev, ok, err := a.store.GetRevision(ctx, revID)
	if err != nil {
		return Member{}, nil, err
	}
	if !ok || rev.AssetID != assetID {
		return Member{}, nil, fmt.Errorf("%w: asset %q revision %q", ErrRevisionNotFound, assetID, revID)
	}
	for i := range rev.Manifest.Members {
		if rev.Manifest.Members[i].SemanticKey != semanticKey {
			continue
		}
		reps, err := a.store.ListRepresentations(ctx, revID)
		if err != nil {
			return Member{}, nil, err
		}
		return rev.Manifest.Members[i], reps, nil
	}
	return Member{}, nil, fmt.Errorf("%w: revision %q member %q", ErrMemberNotFound, revID, semanticKey)
}
