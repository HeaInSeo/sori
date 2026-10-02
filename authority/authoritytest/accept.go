package authoritytest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/HeaInSeo/sori/authority"
)

// concurrency is the number of goroutines racing on one operation id.
const concurrency = 16

// A committed acceptance whose response was lost reconciles to the identical
// Revision on retry. Presentation is not identity-bearing, so a retry that changes
// only Manifest.Presentation is the same acceptance: it reconciles to the original
// Revision (original Presentation kept) instead of failing with ErrRequestConflict.
func acceptIdempotentRetry(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	first, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	retry, err := accept(a, "req-1", digestOne)
	if err != nil {
		return fmt.Errorf("idempotent retry: %w", err)
	}
	if err := sameRevision(retry, first); err != nil {
		return fmt.Errorf("retry: %w", err)
	}
	presentationOnly := acceptReq("req-1", assetA, digestOne)
	presentationOnly.Manifest.Presentation = map[string]string{"title": "conformance-retitled"}
	retry, err = a.AcceptRevision(context.Background(), presentationOnly)
	if err != nil {
		return fmt.Errorf("presentation-only retry: %w", err)
	}
	if err := sameRevision(retry, first); err != nil {
		return fmt.Errorf("presentation-only retry: %w", err)
	}
	stored, err := getRevision(s, first.RevisionID)
	if err != nil {
		return err
	}
	if err := sameRevision(stored, first); err != nil {
		return err
	}
	return acceptProfileOnlyRetry(a)
}

// AcceptRequest.Profile is a validation gate, not identity: a valid retry that changes
// only the profile reconciles to the original Revision, in either direction.
func acceptProfileOnlyRetry(a *authority.Authority) error {
	for _, order := range [][2]authority.AcceptProfile{
		{authority.ProfileI4AOCIDigest, authority.ProfileUnspecified},
		{authority.ProfileUnspecified, authority.ProfileI4AOCIDigest},
	} {
		id := authority.RequestID("req-profile-" + order[0].String())
		req := authority.AcceptRequest{RequestID: id, AssetID: assetA, Manifest: externalManifest(digestOne), Profile: order[0]}
		first, err := a.AcceptRevision(context.Background(), req)
		if err != nil {
			return fmt.Errorf("accept under %s: %w", order[0], err)
		}
		req.Profile = order[1]
		retry, err := a.AcceptRevision(context.Background(), req)
		if err != nil {
			return fmt.Errorf("profile-only retry %s -> %s: %w", order[0], order[1], err)
		}
		if err := sameRevision(retry, first); err != nil {
			return fmt.Errorf("profile-only retry %s -> %s: %w", order[0], order[1], err)
		}
	}
	return nil
}

// The same RequestID with a different identity-bearing fingerprint or asset fails
// closed and never overwrites the accepted Revision.
func acceptRequestConflict(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	ctx := context.Background()
	first, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	if _, err := a.AcceptRevision(ctx, acceptReq("req-1", assetA, digestTwo)); !errors.Is(err, authority.ErrRequestConflict) {
		return fmt.Errorf("same request, different content: err = %v, want ErrRequestConflict", err)
	}
	if _, err := a.AcceptRevision(ctx, acceptReq("req-1", assetB, digestOne)); !errors.Is(err, authority.ErrRequestConflict) {
		return fmt.Errorf("same request, different asset: err = %v, want ErrRequestConflict", err)
	}
	stored, err := getRevision(s, first.RevisionID)
	if err != nil {
		return err
	}
	return sameRevision(stored, first)
}

// Distinct RequestIDs are distinct Revisions even for identical content, and an
// unknown RevisionID is reported as absent without error.
func acceptDistinctRequests(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	r1, err := accept(a, "req-1", digestOne)
	if err != nil {
		return err
	}
	r2, err := accept(a, "req-2", digestOne)
	if err != nil {
		return err
	}
	if r1.RevisionID == "" || r1.RevisionID == r2.RevisionID {
		return fmt.Errorf("distinct requests must yield distinct non-empty revisions: %q / %q", r1.RevisionID, r2.RevisionID)
	}
	_, ok, err := s.GetRevision(context.Background(), "conformance-unknown-revision")
	if err != nil || ok {
		return fmt.Errorf("unknown revision: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	return nil
}

// Concurrent retries of one RequestID converge on exactly one Revision.
func acceptConcurrentSameRequest(t *testing.T, h Harness) error {
	a := authority.New(h.New(t))
	revs := make([]authority.Revision, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			revs[i], errs[i] = accept(a, "req-race", digestOne)
		}(i)
	}
	wg.Wait()
	for i := range concurrency {
		if errs[i] != nil {
			return fmt.Errorf("concurrent retry %d: %w", i, errs[i])
		}
		if err := sameRevision(revs[i], revs[0]); err != nil {
			return fmt.Errorf("concurrent retry %d diverged: %w", i, err)
		}
	}
	return nil
}

// Concurrent acceptances of one RequestID with different content commit exactly one
// winner; every loser fails closed with ErrRequestConflict.
func acceptConcurrentConflictingRequest(t *testing.T, h Harness) error {
	s := h.New(t)
	a := authority.New(s)
	revs := make([]authority.Revision, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			revs[i], errs[i] = a.AcceptRevision(context.Background(), acceptReq("req-race", assetA, fmt.Sprintf("digest-%d", i)))
		}(i)
	}
	wg.Wait()
	winner := -1
	for i := range concurrency {
		switch {
		case errs[i] == nil && winner >= 0:
			return fmt.Errorf("two different contents accepted for one request id (%d and %d)", winner, i)
		case errs[i] == nil:
			winner = i
		case !errors.Is(errs[i], authority.ErrRequestConflict):
			return fmt.Errorf("loser %d: err = %v, want ErrRequestConflict", i, errs[i])
		}
	}
	if winner < 0 {
		return errors.New("no concurrent acceptance won")
	}
	stored, err := getRevision(s, revs[winner].RevisionID)
	if err != nil {
		return err
	}
	return sameRevision(stored, revs[winner])
}
