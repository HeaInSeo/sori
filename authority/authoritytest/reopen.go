package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

// An accepted Revision, its RequestID reconcile and its conflict survive a reopen.
func reopenRevision(t *testing.T, h Harness) error {
	s := h.New(t)
	rev, err := accept(authority.New(s), "req-1", digestOne)
	if err != nil {
		return err
	}
	profiled, err := acceptUnderEachProfile(authority.New(s))
	if err != nil {
		return err
	}
	s = h.Reopen(t, s)
	a := authority.New(s)
	if err := profileOnlyRetries(a, profiled); err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	if err := checkRevisionUnchanged(s, rev); err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	retry, err := accept(a, "req-1", digestOne)
	if err != nil {
		return fmt.Errorf("retry after reopen: %w", err)
	}
	if err := sameRevision(retry, rev); err != nil {
		return fmt.Errorf("retry after reopen: %w", err)
	}
	// The restored idempotency record must still exclude Presentation from the match.
	presentationOnly := acceptReq("req-1", assetA, digestOne)
	presentationOnly.Manifest.Presentation = map[string]string{"title": "conformance-retitled"}
	retry, err = a.AcceptRevision(context.Background(), presentationOnly)
	if err != nil {
		return fmt.Errorf("presentation-only retry after reopen: %w", err)
	}
	if err := sameRevision(retry, rev); err != nil {
		return fmt.Errorf("presentation-only retry after reopen: %w", err)
	}
	if _, err := a.AcceptRevision(context.Background(), acceptReq("req-1", assetA, digestTwo)); !errors.Is(err, authority.ErrRequestConflict) {
		return fmt.Errorf("conflict after reopen: err = %v, want ErrRequestConflict", err)
	}
	// The restored idempotency record must still compare the full fingerprint.
	if err := fingerprintOnlyConflicts(a); err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	next, err := accept(a, "req-2", digestOne)
	if err != nil {
		return err
	}
	// The restored ID allocator must not hand out any persisted RevisionID; re-reading
	// every earlier Revision also catches a reused ID that overwrote one of them.
	for _, prior := range []authority.Revision{rev, profiled[authority.ProfileI4AOCIDigest], profiled[authority.ProfileUnspecified]} {
		if next.RevisionID == prior.RevisionID {
			return fmt.Errorf("revision id %q reused after reopen", prior.RevisionID)
		}
		if err := checkRevisionUnchanged(s, prior); err != nil {
			return fmt.Errorf("after new accept after reopen: %w", err)
		}
	}
	return checkRevisionUnchanged(s, next)
}

// acceptUnderEachProfile accepts one external-import Revision under each profile and
// returns them keyed by the profile they were accepted under.
func acceptUnderEachProfile(a *authority.Authority) (map[authority.AcceptProfile]authority.Revision, error) {
	out := map[authority.AcceptProfile]authority.Revision{}
	for _, p := range []authority.AcceptProfile{authority.ProfileI4AOCIDigest, authority.ProfileUnspecified} {
		req := authority.AcceptRequest{RequestID: profileRequestID(p), AssetID: assetA, Manifest: externalManifest(digestOne), Profile: p}
		rev, err := acceptAs(a, req)
		if err != nil {
			return nil, fmt.Errorf("accept under %s: %w", p, err)
		}
		out[p] = rev
	}
	return out, nil
}

// profileOnlyRetries retries each Revision from acceptUnderEachProfile with only the
// profile switched, so a restored idempotency record that keeps the profile is caught.
func profileOnlyRetries(a *authority.Authority, accepted map[authority.AcceptProfile]authority.Revision) error {
	for first, other := range map[authority.AcceptProfile]authority.AcceptProfile{
		authority.ProfileI4AOCIDigest: authority.ProfileUnspecified,
		authority.ProfileUnspecified:  authority.ProfileI4AOCIDigest,
	} {
		req := authority.AcceptRequest{RequestID: profileRequestID(first), AssetID: assetA, Manifest: externalManifest(digestOne), Profile: other}
		retry, err := a.AcceptRevision(context.Background(), req)
		if err != nil {
			return fmt.Errorf("profile-only retry %s -> %s: %w", first, other, err)
		}
		if err := sameRevision(retry, accepted[first]); err != nil {
			return fmt.Errorf("profile-only retry %s -> %s: %w", first, other, err)
		}
	}
	return nil
}

func profileRequestID(p authority.AcceptProfile) authority.RequestID {
	return authority.RequestID("req-profile-" + p.String())
}

// Alias history, bind reconcile/conflict and Sequence monotonicity survive a reopen.
func reopenAliasHistory(t *testing.T, h Harness) error {
	s := h.New(t)
	r1, r2, err := twoRevisions(authority.New(s))
	if err != nil {
		return err
	}
	events, err := bindSequence(authority.New(s), r1, r2)
	if err != nil {
		return err
	}
	s = h.Reopen(t, s)
	a := authority.New(s)
	retry, err := bind(a, "bind-2", r2)
	if err != nil {
		return fmt.Errorf("bind retry after reopen: %w", err)
	}
	if err := sameBindEvent(retry, events[1]); err != nil {
		return fmt.Errorf("bind retry after reopen: %w", err)
	}
	if _, err := bind(a, "bind-2", r1); !errors.Is(err, authority.ErrAliasBindingConflict) {
		return fmt.Errorf("bind conflict after reopen: err = %v, want ErrAliasBindingConflict", err)
	}
	// The restored bind record must still compare Alias and AssetID, each on its own.
	otherAlias := authority.BindRequest{BindRequestID: "bind-2", Alias: aliasOther, AssetID: r2.AssetID, RevisionID: r2.RevisionID}
	if _, err := a.BindAlias(context.Background(), otherAlias); !errors.Is(err, authority.ErrAliasBindingConflict) {
		return fmt.Errorf("same bind id, different alias after reopen: err = %v, want ErrAliasBindingConflict", err)
	}
	otherAsset := authority.BindRequest{BindRequestID: "bind-2", Alias: aliasLatest, AssetID: assetB, RevisionID: r2.RevisionID}
	if _, err := a.BindAlias(context.Background(), otherAsset); !errors.Is(err, authority.ErrAliasBindingConflict) {
		return fmt.Errorf("same bind id, different asset after reopen: err = %v, want ErrAliasBindingConflict", err)
	}
	if other, err := s.AliasHistory(context.Background(), aliasOther); err != nil || len(other) != 0 {
		return fmt.Errorf("conflicting alias history after reopen = %+v, err=%v; want empty", other, err)
	}
	next, err := bind(a, "bind-4", r2)
	if err != nil {
		return err
	}
	if next.Sequence <= events[len(events)-1].Sequence {
		return fmt.Errorf("sequence %d after reopen not greater than %d", next.Sequence, events[len(events)-1].Sequence)
	}
	hist, err := s.AliasHistory(context.Background(), aliasLatest)
	if err != nil {
		return fmt.Errorf("alias history: %w", err)
	}
	return sameHistory(hist, append(events, next))
}

// Representation identity, availability, attach reconcile/conflict and list order
// survive a reopen.
func reopenRepresentation(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	ctx := context.Background()
	rev, rep, err := acceptAndAttach(a)
	if err != nil {
		return err
	}
	if err := a.SetRepresentationLocators(ctx, rep.RepresentationID, []authority.Locator{locatorB}); err != nil {
		return fmt.Errorf("set locators: %w", err)
	}
	if err := a.SetRepresentationHealth(ctx, rep.RepresentationID, false); err != nil {
		return fmt.Errorf("set health: %w", err)
	}
	// Same members as rev, so only the RevisionID distinguishes an attach to it.
	rev2, err := accept(a, "req-2", digestOne)
	if err != nil {
		return err
	}
	s = h.Reopen(t, s)
	a = authority.New(s)
	got, err := getRepresentation(s, rep.RepresentationID)
	if err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	if err := sameRepresentationIdentity(got, rep); err != nil {
		return fmt.Errorf("after reopen: %w", err)
	}
	if got.Healthy || !reflect.DeepEqual(got.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("availability lost on reopen: healthy=%v locators=%+v", got.Healthy, got.Locators)
	}
	// A retry with a new locator reconciles without touching the restored availability.
	retry, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatOne, locatorC))
	if err != nil {
		return fmt.Errorf("attach retry after reopen: %w", err)
	}
	if err := sameRepresentationIdentity(retry, rep); err != nil {
		return fmt.Errorf("attach retry after reopen: %w", err)
	}
	if retry.Healthy || !reflect.DeepEqual(retry.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("attach retry after reopen returned request availability: healthy=%v locators=%+v", retry.Healthy, retry.Locators)
	}
	if got, err = getRepresentation(s, rep.RepresentationID); err != nil {
		return fmt.Errorf("after attach retry: %w", err)
	}
	if got.Healthy || !reflect.DeepEqual(got.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("attach retry after reopen overwrote availability: healthy=%v locators=%+v", got.Healthy, got.Locators)
	}
	if _, err := a.AttachRepresentation(ctx, attachReq("attach-1", rev, formatTwo)); !errors.Is(err, authority.ErrAttachConflict) {
		return fmt.Errorf("attach conflict after reopen: err = %v, want ErrAttachConflict", err)
	}
	if err := attachCrossRevisionConflictAfterReopen(a, s, rev2, rep); err != nil {
		return err
	}
	return freshAttachAfterReopen(a, s, rev, rep)
}

// freshAttachAfterReopen commits a new attach (new operation and format) to rev after
// reopen: the restored ID allocator must not hand out rep's RepresentationID, and both
// representations must stay intact and listed in attach order.
func freshAttachAfterReopen(a *authority.Authority, s authority.Store, rev authority.Revision, rep authority.Representation) error {
	fresh, err := a.AttachRepresentation(context.Background(), attachReq("attach-2", rev, formatTwo, locatorA))
	if err != nil {
		return fmt.Errorf("fresh attach after reopen: %w", err)
	}
	if fresh.RepresentationID == rep.RepresentationID {
		return fmt.Errorf("representation id %q reused after reopen", rep.RepresentationID)
	}
	got, err := getRepresentation(s, rep.RepresentationID)
	if err != nil {
		return fmt.Errorf("after fresh attach: %w", err)
	}
	if err := sameRepresentationIdentity(got, rep); err != nil {
		return fmt.Errorf("after fresh attach: %w", err)
	}
	if got.Healthy || !reflect.DeepEqual(got.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("fresh attach after reopen changed availability: healthy=%v locators=%+v", got.Healthy, got.Locators)
	}
	if got, err = getRepresentation(s, fresh.RepresentationID); err != nil {
		return fmt.Errorf("fresh attach: %w", err)
	}
	if err := sameRepresentationIdentity(got, fresh); err != nil {
		return fmt.Errorf("fresh attach: %w", err)
	}
	reps, err := listRepresentations(s, rev.RevisionID)
	if err != nil {
		return err
	}
	if len(reps) != 2 {
		return fmt.Errorf("relations after fresh attach = %d, want 2", len(reps))
	}
	if err := sameRepresentationIdentity(reps[0], rep); err != nil {
		return fmt.Errorf("list order after fresh attach: %w", err)
	}
	if err := sameRepresentationIdentity(reps[1], fresh); err != nil {
		return fmt.Errorf("list order after fresh attach: %w", err)
	}
	return nil
}

// attachCrossRevisionConflictAfterReopen reuses the restored attach operation of rep
// on rev2, whose members (and so fingerprint) match: the restored operation record
// must still compare the RevisionID, so it fails closed and changes nothing.
func attachCrossRevisionConflictAfterReopen(a *authority.Authority, s authority.Store, rev2 authority.Revision, rep authority.Representation) error {
	_, err := a.AttachRepresentation(context.Background(), attachReq(rep.AttachOperationID, rev2, rep.Format, locatorA))
	if !errors.Is(err, authority.ErrAttachConflict) {
		return fmt.Errorf("same op, different revision after reopen: err = %v, want ErrAttachConflict", err)
	}
	if reps, err := listRepresentations(s, rev2.RevisionID); err != nil || len(reps) != 0 {
		return fmt.Errorf("conflicting attach after reopen left %d relations on second revision (err=%v), want 0", len(reps), err)
	}
	got, err := getRepresentation(s, rep.RepresentationID)
	if err != nil {
		return fmt.Errorf("after cross-revision conflict: %w", err)
	}
	if got.Healthy || !reflect.DeepEqual(got.Locators, []authority.Locator{locatorB}) {
		return fmt.Errorf("cross-revision conflict after reopen changed availability: healthy=%v locators=%+v", got.Healthy, got.Locators)
	}
	return nil
}
