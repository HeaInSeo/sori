package acquisition

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/HeaInSeo/sori/authority"
)

// SORI-I4A acceptance tests: OCI pull-by-digest first acquisition slice. All tests
// are hermetic (in-process fake registry; no real registry network).

const (
	testAsset  authority.AssetID = "asset-ref-genome"
	testSource string            = "upstream:example/ref-genome" // logical coordinate, not an endpoint
)

type harness struct {
	auth    *authority.Authority
	cps     CheckpointStore
	mem     *MemoryCheckpointStore
	staging string
	creds   map[CredentialRef]auth.Credential
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := authority.NewMemoryStore()
	mem := NewMemoryCheckpointStore()
	return &harness{
		auth:    authority.New(st),
		cps:     mem,
		mem:     mem,
		staging: t.TempDir(),
		creds:   map[CredentialRef]auth.Credential{},
	}
}

// acquirer returns a fresh Acquirer over the harness' durable state. Constructing a
// new one models a process restart: only the checkpoint/authority stores and the
// staging area survive.
func (h *harness) acquirer() *Acquirer {
	return &Acquirer{
		Authority:   h.auth,
		Checkpoints: h.cps,
		StagingRoot: h.staging,
		HTTPClient:  &http.Client{}, // no retrying client: deterministic failures
		Credentials: func(_ context.Context, ref CredentialRef) (auth.Credential, error) {
			c, ok := h.creds[ref]
			if !ok {
				return auth.EmptyCredential, errors.New("unknown credential ref")
			}
			return c, nil
		},
	}
}

func defaultMembers() map[string]string {
	return map[string]string{"genome.fa": ">chr1\nACGTACGT\n", "genome.fa.fai": "chr1\t8\t6\t8\t9\n"}
}

func memberDecls() []MemberDecl {
	return []MemberDecl{
		{SemanticKey: "genome.fa", Role: "primary", DataFormat: "fasta", Cardinality: authority.CardinalitySingle},
		{SemanticKey: "genome.fa.fai", Role: "index", DataFormat: "fai", Cardinality: authority.CardinalitySingle},
	}
}

func request(op OperationID, subject digest.Digest, ep Endpoint) Request {
	return Request{
		OperationID:      op,
		AssetID:          testAsset,
		SourceCoordinate: testSource,
		Subject:          subject,
		ObservedVersion:  "v1", // evidence only
		Members:          memberDecls(),
		Endpoint:         ep,
	}
}

func mustGetOp(t *testing.T, h *harness, id OperationID) Operation {
	t.Helper()
	op, ok, err := h.mem.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("checkpoint for %q: ok=%v err=%v", id, ok, err)
	}
	return op
}

// revisionCount counts accepted Revisions (store ids are sequential sori-rev-N).
func revisionCount(t *testing.T, h *harness) int {
	t.Helper()
	n := 0
	for i := 1; i <= 16; i++ {
		_, ok, err := h.auth.GetRevision(context.Background(), authority.RevisionID("sori-rev-"+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			n++
		}
	}
	return n
}

// AC1 + AC7(same-op retry): the same logical acquisition retried after success does
// not mint a second operation or Revision, and does not re-transfer.
func TestI4A_SameOperationRetryDoesNotMintSecondRevision(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-1", fx.digest, reg.endpoint(""))

	first, err := h.acquirer().Acquire(ctx, req)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !first.Accepted() || first.Revision.RevisionID == "" {
		t.Fatalf("expected accepted revision, got %+v", first.Operation)
	}
	hitsAfterFirst := reg.hits()

	for i := 0; i < 3; i++ {
		again, err := h.acquirer().Acquire(ctx, req)
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if again.Revision.RevisionID != first.Revision.RevisionID {
			t.Fatalf("retry minted a different revision: %q vs %q", again.Revision.RevisionID, first.Revision.RevisionID)
		}
	}
	if got := revisionCount(t, h); got != 1 {
		t.Fatalf("expected exactly 1 revision, got %d", got)
	}
	if reg.hits() != hitsAfterFirst {
		t.Fatalf("retry of an accepted operation re-transferred (%d -> %d hits)", hitsAfterFirst, reg.hits())
	}
	if op := mustGetOp(t, h, "acq-1"); op.Attempts != 1 {
		t.Fatalf("expected 1 transfer attempt, got %d", op.Attempts)
	}

	// Concurrent retries of the same operation converge on the same Revision.
	var wg sync.WaitGroup
	ids := make([]authority.RevisionID, 4)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := h.acquirer().Acquire(ctx, req)
			if err == nil {
				ids[i] = res.Revision.RevisionID
			}
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != first.Revision.RevisionID {
			t.Fatalf("concurrent retry returned %q, want %q", id, first.Revision.RevisionID)
		}
	}
}

// AC1: an OperationID re-used for a different logical acquisition fails closed.
func TestI4A_OperationIDReuseForDifferentSubjectConflicts(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	other := newSubjectFixture(t, map[string]string{"genome.fa": ">chr2\nTTTT\n", "genome.fa.fai": "chr2\t4\t6\t4\t5\n"})
	reg := newFakeRegistry(t, fx, other)
	ctx := context.Background()

	if _, err := h.acquirer().Acquire(ctx, request("acq-1", fx.digest, reg.endpoint(""))); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, err := h.acquirer().Acquire(ctx, request("acq-1", other.digest, reg.endpoint("")))
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("expected ErrOperationConflict, got %v", err)
	}
	if op := mustGetOp(t, h, "acq-1"); op.Subject != fx.digest {
		t.Fatalf("pinned subject was overwritten: %s", op.Subject)
	}
	if got := revisionCount(t, h); got != 1 {
		t.Fatalf("expected 1 revision, got %d", got)
	}
}

// AC2: the subject must be a digest (a tag is never authority), and the digest is
// pinned into the durable operation record BEFORE any transfer — even when the
// transfer then fails.
func TestI4A_DigestPinnedBeforeTransfer(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()

	for _, bad := range []digest.Digest{"latest", "v1", "", "sha256:not-hex"} {
		_, err := h.acquirer().Acquire(ctx, request("acq-tag", bad, reg.endpoint("")))
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("subject %q: expected ErrInvalidRequest, got %v", bad, err)
		}
	}
	if _, ok, _ := h.mem.Get(ctx, "acq-tag"); ok {
		t.Fatal("a tag-subject request must not create an operation")
	}

	reg.set(func(f *fakeRegistry) { f.failBlobs = true })
	res, err := h.acquirer().Acquire(ctx, request("acq-pin", fx.digest, reg.endpoint("")))
	if err == nil {
		t.Fatal("expected transfer failure")
	}
	op := mustGetOp(t, h, "acq-pin")
	if op.Phase != PhasePinned || op.Subject != fx.digest || op.PublicationRequestID == "" {
		t.Fatalf("expected pinned checkpoint with subject %s, got %+v", fx.digest, op)
	}
	if res.Accepted() || revisionCount(t, h) != 0 {
		t.Fatal("a failed transfer must not produce a revision")
	}
}

// AC3 + AC7(digest mismatch): a registry serving different manifest bytes under the
// pinned digest fails closed; nothing is staged or accepted.
func TestI4A_ManifestDigestMismatchFailsClosed(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	reg.set(func(f *fakeRegistry) { f.tamperManifest = true })

	_, err := h.acquirer().Acquire(context.Background(), request("acq-m", fx.digest, reg.endpoint("")))
	if !errors.Is(err, ErrDigestMismatch) || !IsFailClosed(err) {
		t.Fatalf("expected ErrDigestMismatch, got %v", err)
	}
	assertNothingAccepted(t, h, "acq-m")
}

// AC3 + AC7(digest mismatch): a member blob whose bytes do not match its manifest
// digest fails closed.
func TestI4A_MemberBlobDigestMismatchFailsClosed(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	reg.set(func(f *fakeRegistry) { f.tamperBlob = true })

	_, err := h.acquirer().Acquire(context.Background(), request("acq-b", fx.digest, reg.endpoint("")))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch, got %v", err)
	}
	assertNothingAccepted(t, h, "acq-b")

	// Once the endpoint serves the genuine bytes, the SAME operation completes.
	reg.set(func(f *fakeRegistry) { f.tamperBlob = false })
	res, err := h.acquirer().Acquire(context.Background(), request("acq-b", fx.digest, reg.endpoint("")))
	if err != nil || !res.Accepted() {
		t.Fatalf("retry after mismatch: accepted=%v err=%v", res.Accepted(), err)
	}
}

// AC3: a manifest that does not close over the declared members fails closed.
func TestI4A_MemberClosureMismatchFailsClosed(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, map[string]string{"genome.fa": "ACGT"})
	reg := newFakeRegistry(t, fx)

	_, err := h.acquirer().Acquire(context.Background(), request("acq-c", fx.digest, reg.endpoint("")))
	if !errors.Is(err, ErrSubjectInvalid) {
		t.Fatalf("expected ErrSubjectInvalid, got %v", err)
	}
	assertNothingAccepted(t, h, "acq-c")
}

func assertNothingAccepted(t *testing.T, h *harness, id OperationID) {
	t.Helper()
	op := mustGetOp(t, h, id)
	if op.Phase != PhasePinned || op.StagedPath != "" || op.LastError == "" {
		t.Fatalf("expected recoverable pinned checkpoint with recorded error, got %+v", op)
	}
	if revisionCount(t, h) != 0 {
		t.Fatal("fail-closed acquisition produced a revision")
	}
	entries, err := os.ReadDir(h.staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("fail-closed acquisition left staged content: %v", entries)
	}
}

// AC4 + AC7(endpoint/credential rotation): the endpoint and credential reference are
// mutable availability coordinates — rotating them on retry continues the SAME
// operation, and neither enters the accepted Revision's identity/provenance.
func TestI4A_EndpointAndCredentialRotation(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	primary := newFakeRegistry(t, fx)
	mirror := newFakeRegistry(t, fx)
	primary.set(func(f *fakeRegistry) { f.user, f.pass = "alice", "pw-a"; f.failBlobs = true })
	mirror.set(func(f *fakeRegistry) { f.user, f.pass = "bob", "pw-b" })
	h.creds["cred/primary"] = auth.Credential{Username: "alice", Password: "pw-a"}
	h.creds["cred/mirror"] = auth.Credential{Username: "bob", Password: "pw-b"}
	ctx := context.Background()

	if _, err := h.acquirer().Acquire(ctx, request("acq-rot", fx.digest, primary.endpoint("cred/primary"))); err == nil {
		t.Fatal("expected primary endpoint failure")
	}
	before := mustGetOp(t, h, "acq-rot")

	// A wrong credential for the mirror fails without accepting anything.
	if _, err := h.acquirer().Acquire(ctx, request("acq-rot", fx.digest, mirror.endpoint("cred/primary"))); err == nil {
		t.Fatal("expected auth failure with the wrong credential reference")
	}

	res, err := h.acquirer().Acquire(ctx, request("acq-rot", fx.digest, mirror.endpoint("cred/mirror")))
	if err != nil || !res.Accepted() {
		t.Fatalf("rotated acquire: accepted=%v err=%v", res.Accepted(), err)
	}
	after := res.Operation
	if after.Fingerprint != before.Fingerprint || after.Subject != before.Subject || after.PublicationRequestID != before.PublicationRequestID {
		t.Fatalf("rotation changed operation identity: before=%+v after=%+v", before, after)
	}
	if after.Attempts != 3 {
		t.Fatalf("expected 3 attempts on one operation, got %d", after.Attempts)
	}

	prov := res.Revision.Manifest.Provenance
	for _, leak := range []string{primary.host(), mirror.host(), "cred/", "pw-", "alice", "bob"} {
		for _, field := range []string{prov.SourceCoordinate, prov.ObservedVersion, prov.ObservedChecksum, res.Revision.Fingerprint} {
			if strings.Contains(field, leak) {
				t.Fatalf("endpoint/credential %q leaked into revision identity/provenance: %+v", leak, prov)
			}
		}
	}
	if prov.ObservedChecksum != fx.digest.String() {
		t.Fatalf("observed checksum = %q, want pinned digest %s", prov.ObservedChecksum, fx.digest)
	}

	// Rotating back to the (now healthy) primary after acceptance: same Revision, no transfer.
	primary.set(func(f *fakeRegistry) { f.failBlobs = false })
	hits := primary.hits()
	again, err := h.acquirer().Acquire(ctx, request("acq-rot", fx.digest, primary.endpoint("cred/primary")))
	if err != nil || again.Revision.RevisionID != res.Revision.RevisionID {
		t.Fatalf("post-rotation retry: rev=%q err=%v", again.Revision.RevisionID, err)
	}
	if primary.hits() != hits || revisionCount(t, h) != 1 {
		t.Fatal("post-acceptance retry re-transferred or minted a revision")
	}
}

// crashStore wraps a CheckpointStore and fails ONE Update into a given phase,
// modeling a process crash at that checkpoint boundary.
type crashStore struct {
	CheckpointStore
	mu      sync.Mutex
	crashOn Phase
	fired   bool
}

var errCrash = errors.New("simulated crash")

func (c *crashStore) Update(ctx context.Context, op Operation) (Operation, error) {
	c.mu.Lock()
	crash := !c.fired && op.Phase == c.crashOn
	if crash {
		c.fired = true
	}
	c.mu.Unlock()
	if crash {
		return Operation{}, errCrash
	}
	return c.CheckpointStore.Update(ctx, op)
}

// AC5 + AC7(crash/restart): a crash after a verified staging, before the acceptance
// handoff, leaves a recoverable STAGED checkpoint and NO Revision (transfer success
// alone is not acceptance). A restart resumes without re-transferring.
func TestI4A_CrashAfterStagingBeforeAcceptIsRecoverable(t *testing.T) {
	h := newHarness(t)
	h.cps = &crashStore{CheckpointStore: h.mem, crashOn: PhaseAcceptPending}
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-crash-1", fx.digest, reg.endpoint(""))

	if _, err := h.acquirer().Acquire(ctx, req); !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	op := mustGetOp(t, h, "acq-crash-1")
	if op.Phase != PhaseStaged || op.StagedPath == "" {
		t.Fatalf("expected STAGED checkpoint, got %+v", op)
	}
	if revisionCount(t, h) != 0 {
		t.Fatal("transfer success alone must not be an accepted revision")
	}
	hits := reg.hits()

	res, err := h.acquirer().Acquire(ctx, req) // restart
	if err != nil || !res.Accepted() {
		t.Fatalf("resume: accepted=%v err=%v", res.Accepted(), err)
	}
	if reg.hits() != hits {
		t.Fatal("resume from STAGED re-transferred")
	}
	if revisionCount(t, h) != 1 {
		t.Fatal("expected exactly one revision after resume")
	}
}

// AC5 + AC7(crash/restart): a crash after the authority committed but before the
// ACCEPTED checkpoint leaves an explicit UNKNOWN (ACCEPT_PENDING_UNKNOWN); the
// restart reconciles to the SAME Revision.
func TestI4A_CrashAfterAcceptBeforeCheckpointReconcilesUnknown(t *testing.T) {
	h := newHarness(t)
	h.cps = &crashStore{CheckpointStore: h.mem, crashOn: PhaseAccepted}
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-crash-2", fx.digest, reg.endpoint(""))

	res, err := h.acquirer().Acquire(ctx, req)
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	if res.Operation.Phase != PhaseAcceptPending || mustGetOp(t, h, "acq-crash-2").Phase != PhaseAcceptPending {
		t.Fatalf("expected explicit UNKNOWN (%s), got %q", PhaseAcceptPending, res.Operation.Phase)
	}
	if revisionCount(t, h) != 1 {
		t.Fatal("expected the committed revision to exist")
	}

	again, err := h.acquirer().Acquire(ctx, req)
	if err != nil || !again.Accepted() {
		t.Fatalf("reconcile: accepted=%v err=%v", again.Accepted(), err)
	}
	if again.Revision.RevisionID != "sori-rev-1" || revisionCount(t, h) != 1 {
		t.Fatalf("reconcile minted a second revision: %q", again.Revision.RevisionID)
	}
}

// AC5 + AC3: a staged copy corrupted between crash and restart is re-verified, fails
// closed, and the operation resets to PINNED so a retry re-transfers.
func TestI4A_ResumedStagedCopyIsReverified(t *testing.T) {
	h := newHarness(t)
	h.cps = &crashStore{CheckpointStore: h.mem, crashOn: PhaseAcceptPending}
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()
	req := request("acq-crash-3", fx.digest, reg.endpoint(""))

	if _, err := h.acquirer().Acquire(ctx, req); !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	op := mustGetOp(t, h, "acq-crash-3")
	member := op.StagedMembers[0].Proof.Digest
	d := digest.Digest(member)
	blob := filepath.Join(op.StagedPath, "blobs", d.Algorithm().String(), d.Encoded())
	// Staged blobs are written read-only; replace the file to model corruption.
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := h.acquirer().Acquire(ctx, req)
	if !errors.Is(err, ErrStagedInvalid) || !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrStagedInvalid/ErrDigestMismatch, got %v", err)
	}
	if got := mustGetOp(t, h, "acq-crash-3"); got.Phase != PhasePinned || revisionCount(t, h) != 0 {
		t.Fatalf("expected reset to PINNED with no revision, got %+v", got)
	}

	res, err := h.acquirer().Acquire(ctx, req)
	if err != nil || !res.Accepted() {
		t.Fatalf("re-transfer: accepted=%v err=%v", res.Accepted(), err)
	}
}

// AC7(publication retry identity separation): the publication RequestID is minted
// once per acquisition operation, is distinct from the acquisition OperationID and
// from other operations, and a publication retry reconciles to the same Revision.
func TestI4A_PublicationRetryIdentitySeparation(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)
	ctx := context.Background()

	// An unrelated I1M publisher happens to use the raw operation-id string as its
	// RequestID. It must not collide with the acquisition's publication identity.
	legacy, err := h.auth.AcceptRevision(ctx, authority.AcceptRequest{
		RequestID: "acq-pub",
		AssetID:   "asset-other",
		Manifest: authority.SemanticManifest{
			Origin: authority.OriginExternalImport,
			Members: []authority.Member{{
				SemanticKey: "x", Role: "primary", DataFormat: "bin", Cardinality: authority.CardinalitySingle,
				Proof: authority.ContentProof{Algorithm: "sha256", Digest: "abc"},
			}},
			Provenance: authority.Provenance{SourceCoordinate: "legacy"},
		},
	})
	if err != nil {
		t.Fatalf("legacy accept: %v", err)
	}

	res, err := h.acquirer().Acquire(ctx, request("acq-pub", fx.digest, reg.endpoint("")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pub := res.Operation.PublicationRequestID
	if pub == authority.RequestID(res.Operation.ID) || pub != PublicationRequestID("acq-pub") {
		t.Fatalf("publication id %q not separated from operation id %q", pub, res.Operation.ID)
	}
	if res.Revision.RequestID != pub || res.Revision.RevisionID == legacy.RevisionID {
		t.Fatalf("revision %+v not published under %q", res.Revision, pub)
	}

	// Publication retry through the authority directly (lost response): same Revision.
	retry, err := h.auth.AcceptRevision(ctx, acceptRequest(res.Operation))
	if err != nil || retry.RevisionID != res.Revision.RevisionID {
		t.Fatalf("publication retry: rev=%q err=%v", retry.RevisionID, err)
	}

	// A different logical acquisition gets its own publication identity.
	other, err := h.acquirer().Acquire(ctx, request("acq-pub-2", fx.digest, reg.endpoint("")))
	if err != nil {
		t.Fatalf("second operation: %v", err)
	}
	if other.Operation.PublicationRequestID == pub {
		t.Fatal("distinct operations share a publication RequestID")
	}
}

// The accepted Revision carries the I4A frozen-subject evidence.
func TestI4A_AcceptedRevisionCarriesFrozenSubjectProof(t *testing.T) {
	h := newHarness(t)
	fx := newSubjectFixture(t, defaultMembers())
	reg := newFakeRegistry(t, fx)

	res, err := h.acquirer().Acquire(context.Background(), request("acq-proof", fx.digest, reg.endpoint("")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m := res.Revision.Manifest
	if m.Origin != authority.OriginExternalImport || m.Provenance.ObservedVersion != "v1" || m.Provenance.ObservedChecksum != fx.digest.String() {
		t.Fatalf("unexpected provenance: %+v", m.Provenance)
	}
	if m.Provenance.UpstreamBuilderKnown || m.Provenance.UpstreamBuilder != "" {
		t.Fatal("upstream builder must be explicitly unknown, not fabricated")
	}
	for _, mem := range m.Members {
		want := digest.FromBytes([]byte(defaultMembers()[mem.SemanticKey]))
		if mem.Proof.Digest != want.String() {
			t.Fatalf("member %q proof %q, want %s", mem.SemanticKey, mem.Proof.Digest, want)
		}
	}
}
