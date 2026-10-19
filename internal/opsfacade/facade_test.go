package opsfacade_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	opsv1 "github.com/HeaInSeo/sori/api/sori/ops/v1"
	"github.com/HeaInSeo/sori/internal/opsfacade"
	"github.com/HeaInSeo/sori/jobledger"
)

// Everything below that answers ALLOW or VALID is a test fixture. None of it
// is reachable from production code.

const (
	installation = "inst-facade"
	principal    = "alice"
)

var guard1 = jobledger.Guard{InstallationID: installation, ActivationEpoch: 1}

type trace struct{ calls []string }

func (t *trace) add(s string) { t.calls = append(t.calls, s) }

type fixedStartup struct {
	t  *trace
	st opsfacade.StartupState
}

func (s fixedStartup) State(context.Context) (opsfacade.StartupState, error) {
	s.t.add("startup")
	return s.st, nil
}

type fixedAuth struct {
	t   *trace
	d   opsfacade.AuthDecision
	err error
}

func (a fixedAuth) Authorize(context.Context, opsfacade.Caller, opsfacade.AuthRequest) (opsfacade.AuthDecision, error) {
	a.t.add("authorize")
	return a.d, a.err
}

// tokenNamespaces maps token strings to namespace refs owned by one principal.
type tokenNamespaces struct {
	t     *trace
	byTok map[string]jobledger.NamespaceRef
}

func (n tokenNamespaces) Verify(_ context.Context, c opsfacade.Caller, tok string) (jobledger.NamespaceRef, error) {
	n.t.add("namespace")
	ref, ok := n.byTok[tok]
	if !ok || ref.PrincipalSubject != c.Principal {
		return jobledger.NamespaceRef{}, errors.New("namespace token rejected")
	}
	return ref, nil
}

// specMeanings stands in for the server-verified meaning of a frozen input:
// the meaning is a hash of the transfer locator and target as the fixture's
// "verified state" records them. It is not the D01 request-meaning envelope.
type specMeanings struct {
	t     *trace
	state map[string]string // transfer_id -> verified frozen content id
	err   error
}

// fixedMeaning answers every spec with the same VerifiedMeaning and no error.
type fixedMeaning struct {
	t *trace
	m opsfacade.VerifiedMeaning
}

func (m fixedMeaning) Meaning(context.Context, opsfacade.Caller, *opsv1.JobSpec) (opsfacade.VerifiedMeaning, error) {
	m.t.add("meaning")
	return m.m, nil
}

func (m specMeanings) Meaning(_ context.Context, _ opsfacade.Caller, spec *opsv1.JobSpec) (opsfacade.VerifiedMeaning, error) {
	m.t.add("meaning")
	if m.err != nil {
		return opsfacade.VerifiedMeaning{}, m.err
	}
	li := spec.GetLocalImport()
	frozen, ok := m.state[li.GetTransferId()]
	if li == nil || !ok {
		return opsfacade.VerifiedMeaning{}, errors.New("no verified frozen input")
	}
	sum := sha256.Sum256([]byte(frozen + "\x00" + li.GetTarget().GetCreateAssetName()))
	return opsfacade.VerifiedMeaning{SHA256: sum[:], Kind: "LOCAL_IMPORT"}, nil
}

// recordingLedger records every call and delegates to an optional store.
type recordingLedger struct {
	t     *trace
	inner opsfacade.Ledger
}

func (l recordingLedger) Accept(ctx context.Context, g jobledger.Guard, r jobledger.AcceptRequest) (jobledger.Decision, error) {
	l.t.add("ledger.Accept")
	if l.inner == nil {
		return jobledger.Decision{}, errors.New("no store")
	}
	return l.inner.Accept(ctx, g, r)
}

func (l recordingLedger) Lookup(ctx context.Context, g jobledger.Guard, r jobledger.AcceptRequest) (jobledger.Decision, error) {
	l.t.add("ledger.Lookup")
	if l.inner == nil {
		return jobledger.Decision{}, errors.New("no store")
	}
	return l.inner.Lookup(ctx, g, r)
}

func (l recordingLedger) GetJob(ctx context.Context, id string) (jobledger.Job, bool, error) {
	l.t.add("ledger.GetJob")
	if l.inner == nil {
		return jobledger.Job{}, false, errors.New("no store")
	}
	return l.inner.GetJob(ctx, id)
}

type env struct {
	ctx    context.Context
	store  *jobledger.Store
	tr     *trace
	f      *opsfacade.Facade
	ns     jobledger.NamespaceRecord
	state  map[string]string
	caller opsfacade.Caller
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	s, err := jobledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Init(ctx, installation); err != nil {
		t.Fatal(err)
	}
	ns, err := s.AllocateNamespace(ctx, guard1, principal)
	if err != nil {
		t.Fatal(err)
	}
	tr := &trace{}
	state := map[string]string{"xfer_a": "frozen-a"}
	f := &opsfacade.Facade{
		Ledger:     recordingLedger{t: tr, inner: s},
		Startup:    fixedStartup{t: tr, st: opsfacade.StartupState{Writability: opsv1.Writability_WRITABILITY_RW, Guard: guard1}},
		Authorizer: fixedAuth{t: tr, d: opsfacade.AuthAllow},
		Namespaces: tokenNamespaces{t: tr, byTok: map[string]jobledger.NamespaceRef{"sns1.tok": ns.Ref()}},
		Meanings:   specMeanings{t: tr, state: state},
	}
	return &env{ctx: ctx, store: s, tr: tr, f: f, ns: ns, state: state, caller: opsfacade.Caller{Principal: principal}}
}

func header() *opsv1.RequestHeader {
	return &opsv1.RequestHeader{ContractRevision: 1,
		Installation: &opsv1.InstallationBinding{InstallationId: installation, ActivationEpoch: 1}}
}

func spec(name string) *opsv1.JobSpec {
	return &opsv1.JobSpec{Spec: &opsv1.JobSpec_LocalImport{LocalImport: &opsv1.LocalImportSpec{
		TransferId: "xfer_a", AcceptProfile: "sori-local-import:v1",
		Target: &opsv1.AssetTarget{Target: &opsv1.AssetTarget_CreateAssetName{CreateAssetName: name}},
	}}}
}

func meaning(e *env, s *opsv1.JobSpec) []byte {
	m, err := e.f.Meanings.Meaning(e.ctx, opsfacade.Caller{Principal: principal}, s)
	if err != nil {
		panic(err)
	}
	return m.SHA256
}

func key(e *env, k string, s *opsv1.JobSpec) *opsv1.OperationKey {
	return &opsv1.OperationKey{NamespaceId: "sns1.tok", IdempotencyKey: k, RequestMeaningSha256: meaning(e, s)}
}

var alice = opsfacade.Caller{Principal: principal}

// The common.proto key form is 1..128 bytes of [A-Za-z0-9._~-]. The longest
// key that uses every allowed character class is accepted; one byte longer, or
// one disallowed byte, is FIELD_INVALID before the namespace port is asked.
func TestIdempotencyKeyFormBoundaries(t *testing.T) {
	longest := strings.Repeat("aZ09._~-", 16)
	if len(longest) != 128 {
		t.Fatalf("fixture key is %d bytes", len(longest))
	}
	e := newEnv(t)
	s := spec("genome-a")
	sub, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: key(e, longest, s), Spec: s})
	if err != nil || sub.Outcome != opsv1.SubmitOutcome_SUBMIT_OUTCOME_NEW {
		t.Fatalf("128-byte key: %v, %v", sub, err)
	}
	for _, k := range []string{longest + "a", "a\x00b", "a+b", "a:b", "a\nb"} {
		opKey := key(e, k, s)
		e.tr.calls = nil
		_, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: opKey, Spec: s})
		if got := opsfacade.ReasonOf(err); got != opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID {
			t.Fatalf("key %q: reason = %v (%v), want FIELD_INVALID", k, got, err)
		}
		if want := []string{"startup"}; !reflect.DeepEqual(e.tr.calls, want) {
			t.Fatalf("key %q: calls = %v, want %v", k, e.tr.calls, want)
		}
	}
}

// A JobSpec carrying fields this contract revision does not define, at the
// top level or in a nested message, is FIELD_UNKNOWN before the namespace
// port is asked. An unknown field is never hashed into the meaning.
func TestUnknownSpecFieldsRefusedBeforeNamespace(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 1)
	cases := map[string]func(*opsv1.JobSpec){
		"top level":    func(s *opsv1.JobSpec) { s.ProtoReflect().SetUnknown(unknown) },
		"local import": func(s *opsv1.JobSpec) { s.GetLocalImport().ProtoReflect().SetUnknown(unknown) },
		"asset target": func(s *opsv1.JobSpec) { s.GetLocalImport().GetTarget().ProtoReflect().SetUnknown(unknown) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			s := spec("genome-a")
			opKey := key(e, "k1", s)
			mutate(s)
			e.tr.calls = nil
			_, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: opKey, Spec: s})
			if got := opsfacade.ReasonOf(err); got != opsv1.ErrorReason_ERROR_REASON_FIELD_UNKNOWN {
				t.Fatalf("reason = %v (%v), want FIELD_UNKNOWN", got, err)
			}
			if want := []string{"startup"}; !reflect.DeepEqual(e.tr.calls, want) {
				t.Fatalf("calls = %v, want %v", e.tr.calls, want)
			}
		})
	}
}

// request_meaning_sha256 is a 32-byte SHA-256 value (common.proto). An absent
// digest is FIELD_UNSPECIFIED and any other length is FIELD_INVALID, both in
// the structure step: the namespace, authorization and meaning ports are never
// asked. A JobSpec with no case set is FIELD_UNSPECIFIED there too.
func TestMalformedClientMeaningDigestRefusedBeforePorts(t *testing.T) {
	cases := map[string]struct {
		digest []byte
		spec   func() *opsv1.JobSpec
		want   opsv1.ErrorReason
	}{
		"digest absent":   {nil, func() *opsv1.JobSpec { return spec("genome-a") }, opsv1.ErrorReason_ERROR_REASON_FIELD_UNSPECIFIED},
		"digest empty":    {[]byte{}, func() *opsv1.JobSpec { return spec("genome-a") }, opsv1.ErrorReason_ERROR_REASON_FIELD_UNSPECIFIED},
		"digest 1 byte":   {[]byte{1}, func() *opsv1.JobSpec { return spec("genome-a") }, opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		"digest 31 bytes": {make([]byte, 31), func() *opsv1.JobSpec { return spec("genome-a") }, opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		"digest 33 bytes": {make([]byte, 33), func() *opsv1.JobSpec { return spec("genome-a") }, opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		"spec no case":    {make([]byte, 32), func() *opsv1.JobSpec { return &opsv1.JobSpec{} }, opsv1.ErrorReason_ERROR_REASON_FIELD_UNSPECIFIED},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			s := c.spec()
			k := &opsv1.OperationKey{NamespaceId: "sns1.tok", IdempotencyKey: "k1", RequestMeaningSha256: c.digest}
			e.tr.calls = nil
			_, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: k, Spec: s})
			if got := opsfacade.ReasonOf(err); got != c.want {
				t.Fatalf("submit reason = %v (%v), want %v", got, err, c.want)
			}
			if want := []string{"startup"}; !reflect.DeepEqual(e.tr.calls, want) {
				t.Fatalf("submit calls = %v, want %v", e.tr.calls, want)
			}
			e.tr.calls = nil
			_, err = e.f.LookupJobByKey(e.ctx, alice, &opsv1.LookupJobByKeyRequest{Header: header(), Key: k, Spec: s})
			if got := opsfacade.ReasonOf(err); got != c.want {
				t.Fatalf("lookup reason = %v (%v), want %v", got, err, c.want)
			}
			if want := []string{"startup"}; !reflect.DeepEqual(e.tr.calls, want) {
				t.Fatalf("lookup calls = %v, want %v", e.tr.calls, want)
			}
		})
	}
}

// A Meanings answer whose kind the server cannot project, or whose kind belongs
// to another JobSpec case, is RO_HOLD before any ledger call. Even with the
// client digest matching, no Job is stored that ProjectJob would later refuse.
func TestUnsupportedOrMismatchedMeaningKindRefusedBeforeLedger(t *testing.T) {
	for name, kind := range map[string]string{
		"typo kind":        "LOCAL_IMPORTS",
		"lowercase kind":   "local_import",
		"other spec case":  "EXPORT",
		"other spec case2": "OCI_ACQUIRE",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			s := spec("genome-a")
			k := key(e, "k1", s)
			e.f.Meanings = fixedMeaning{t: e.tr, m: opsfacade.VerifiedMeaning{SHA256: k.GetRequestMeaningSha256(), Kind: kind}}
			e.tr.calls = nil
			sub, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: k, Spec: s})
			if got := opsfacade.ReasonOf(err); got != opsv1.ErrorReason_ERROR_REASON_RO_HOLD || sub.Job != nil {
				t.Fatalf("submit = %v, reason %v (%v), want RO_HOLD and no job", sub, got, err)
			}
			if want := []string{"startup", "namespace", "authorize", "meaning"}; !reflect.DeepEqual(e.tr.calls, want) {
				t.Fatalf("submit calls = %v, want %v", e.tr.calls, want)
			}
			if jobs, _ := e.store.UnfinishedJobs(e.ctx); len(jobs) != 0 {
				t.Fatalf("ledger stored %d jobs for kind %q", len(jobs), kind)
			}
			e.tr.calls = nil
			lk, err := e.f.LookupJobByKey(e.ctx, alice, &opsv1.LookupJobByKeyRequest{Header: header(), Key: k, Spec: s})
			if got := opsfacade.ReasonOf(err); got != opsv1.ErrorReason_ERROR_REASON_RO_HOLD || lk.Job != nil {
				t.Fatalf("lookup = %v, reason %v (%v), want RO_HOLD and no job", lk, got, err)
			}
			if want := []string{"startup", "namespace", "authorize", "meaning"}; !reflect.DeepEqual(e.tr.calls, want) {
				t.Fatalf("lookup calls = %v, want %v", e.tr.calls, want)
			}
		})
	}
}

// Each JobSpec case with the matching kind from the Meanings port is accepted
// and projects to that case's JobKind.
func TestMatchingMeaningKindAcceptedForEachSpecCase(t *testing.T) {
	cases := map[string]struct {
		spec *opsv1.JobSpec
		kind string
		want opsv1.JobKind
	}{
		"local import": {spec("genome-a"), "LOCAL_IMPORT", opsv1.JobKind_JOB_KIND_LOCAL_IMPORT},
		"oci acquire": {&opsv1.JobSpec{Spec: &opsv1.JobSpec_OciAcquire{OciAcquire: &opsv1.OciAcquireSpec{}}},
			"OCI_ACQUIRE", opsv1.JobKind_JOB_KIND_OCI_ACQUIRE},
		"export": {&opsv1.JobSpec{Spec: &opsv1.JobSpec_Export{Export: &opsv1.ExportSpec{}}},
			"EXPORT", opsv1.JobKind_JOB_KIND_EXPORT},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			digest := sha256.Sum256([]byte(name))
			e.f.Meanings = fixedMeaning{t: e.tr, m: opsfacade.VerifiedMeaning{SHA256: digest[:], Kind: c.kind}}
			k := &opsv1.OperationKey{NamespaceId: "sns1.tok", IdempotencyKey: "k1", RequestMeaningSha256: digest[:]}
			sub, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: k, Spec: c.spec})
			if err != nil || sub.Outcome != opsv1.SubmitOutcome_SUBMIT_OUTCOME_NEW || sub.Job.GetKind() != c.want {
				t.Fatalf("submit = %v (%v), want NEW %v", sub, err, c.want)
			}
		})
	}
}

// GetJob checks the authenticated caller before the startup port or the
// request fields, the same order prepare uses: an empty principal is
// AUTH_REQUIRED whatever the header or job_id say, and no port is asked.
func TestGetJobAuthenticatesBeforeStartupAndFields(t *testing.T) {
	cases := map[string]func(*env, *opsv1.GetJobRequest){
		"valid request":        func(*env, *opsv1.GetJobRequest) {},
		"unsupported revision": func(_ *env, r *opsv1.GetJobRequest) { r.Header.ContractRevision = 2 },
		"other installation":   func(_ *env, r *opsv1.GetJobRequest) { r.Header.Installation.InstallationId = "other" },
		"startup unavailable":  func(e *env, _ *opsv1.GetJobRequest) { e.f.Startup = nil },
		"empty job id":         func(_ *env, r *opsv1.GetJobRequest) { r.JobId = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := &opsv1.GetJobRequest{Header: header(), JobId: "job_x"}
			mutate(e, r)
			e.tr.calls = nil
			got, err := e.f.GetJob(e.ctx, opsfacade.Caller{}, r)
			if reason := opsfacade.ReasonOf(err); reason != opsv1.ErrorReason_ERROR_REASON_AUTH_REQUIRED || got != nil {
				t.Fatalf("get = %v, reason %v (%v), want AUTH_REQUIRED", got, reason, err)
			}
			if len(e.tr.calls) != 0 {
				t.Fatalf("ports asked before authentication: %v", e.tr.calls)
			}
		})
	}
}

func TestAckLossReplayConvergesOnSameJob(t *testing.T) {
	e := newEnv(t)
	s := spec("genome-a")
	sub, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: key(e, "k1", s), Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Outcome != opsv1.SubmitOutcome_SUBMIT_OUTCOME_NEW || sub.Job.GetJobId() == "" {
		t.Fatalf("first submit = %v", sub)
	}
	// The caller lost the ACK: it never saw job_id. Same key, same spec.
	lk, err := e.f.LookupJobByKey(e.ctx, alice, &opsv1.LookupJobByKeyRequest{Header: header(), Key: key(e, "k1", s), Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	if lk.Result != opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_FOUND || lk.Job.GetJobId() != sub.Job.GetJobId() {
		t.Fatalf("lookup after ACK loss = %v, want FOUND %s", lk, sub.Job.GetJobId())
	}
	again, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: key(e, "k1", s), Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != opsv1.SubmitOutcome_SUBMIT_OUTCOME_EXISTING || again.Job.GetJobId() != sub.Job.GetJobId() {
		t.Fatalf("replay submit = %v, want EXISTING %s", again, sub.Job.GetJobId())
	}
	jobs, err := e.store.UnfinishedJobs(e.ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ledger jobs = %d (%v), want 1", len(jobs), err)
	}
}

func TestNonFoundResultsCarryNoJob(t *testing.T) {
	e := newEnv(t)
	s := spec("genome-a")
	if _, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: key(e, "k1", s), Spec: s}); err != nil {
		t.Fatal(err)
	}
	other := spec("genome-b") // same key, different meaning
	d, err := e.store.Lookup(e.ctx, guard1, jobledger.AcceptRequest{Namespace: e.ns.Ref(), Principal: principal, IdempotencyKey: "k1",
		MeaningDigest: "sha256:different", Kind: "LOCAL_IMPORT"})
	if err != nil || d.Outcome != jobledger.OutcomeConflict || d.Job == nil {
		t.Fatalf("precondition: ledger CONFLICT with an internal Job, got %v %v", d, err)
	}
	cases := []struct {
		name  string
		f     func(*opsfacade.Facade)
		key   string
		spec  *opsv1.JobSpec
		want  opsv1.KeyLookupResult
		guard jobledger.Guard
	}{
		{"conflict", nil, "k1", other, opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_CONFLICT, guard1},
		{"not found", nil, "k2", s, opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_NOT_FOUND, guard1},
		{"hold", nil, "k2", s, opsv1.KeyLookupResult_KEY_LOOKUP_RESULT_RECONCILE_HOLD,
			jobledger.Guard{InstallationID: installation, ActivationEpoch: 1, CutCounter: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e.f.Startup = fixedStartup{t: e.tr, st: opsfacade.StartupState{Writability: opsv1.Writability_WRITABILITY_RO_HOLD, Guard: c.guard}}
			lk, err := e.f.LookupJobByKey(e.ctx, alice, &opsv1.LookupJobByKeyRequest{Header: header(), Key: key(e, c.key, c.spec), Spec: c.spec})
			if err != nil {
				t.Fatal(err)
			}
			if lk.Result != c.want || lk.Job != nil {
				t.Fatalf("lookup = %v job %v, want %v and no job", lk.Result, lk.Job, c.want)
			}
		})
	}
	e.f.Startup = fixedStartup{t: e.tr, st: opsfacade.StartupState{Writability: opsv1.Writability_WRITABILITY_RW, Guard: guard1}}
	sub, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: key(e, "k1", other), Spec: other})
	if opsfacade.ReasonOf(err) != opsv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_CONFLICT || sub.Job != nil {
		t.Fatalf("conflicting submit = %v %v, want IDEMPOTENCY_CONFLICT and no job", sub, err)
	}
}

// refusals: each case must stop before the first ledger call.
func TestRefusalsHappenBeforeAnyLedgerCall(t *testing.T) {
	ro := opsfacade.StartupState{Writability: opsv1.Writability_WRITABILITY_RO_HOLD, Guard: guard1}
	cases := []struct {
		name   string
		mutate func(e *env, k *opsv1.OperationKey, h *opsv1.RequestHeader)
		want   opsv1.ErrorReason
	}{
		{"auth deny", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Authorizer = fixedAuth{t: e.tr, d: opsfacade.AuthDeny}
		}, opsv1.ErrorReason_ERROR_REASON_AUTH_DENIED},
		{"auth unknown", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Authorizer = fixedAuth{t: e.tr, d: opsfacade.AuthUnknown}
		}, opsv1.ErrorReason_ERROR_REASON_AUTH_PROVIDER_UNKNOWN},
		{"auth provider error", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Authorizer = fixedAuth{t: e.tr, d: opsfacade.AuthAllow, err: errors.New("provider down")}
		}, opsv1.ErrorReason_ERROR_REASON_AUTH_PROVIDER_UNKNOWN},
		{"no authorizer", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) { e.f.Authorizer = nil },
			opsv1.ErrorReason_ERROR_REASON_AUTH_PROVIDER_UNKNOWN},
		{"unauthenticated", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) { e.caller = opsfacade.Caller{} },
			opsv1.ErrorReason_ERROR_REASON_AUTH_REQUIRED},
		{"no startup port", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) { e.f.Startup = nil },
			opsv1.ErrorReason_ERROR_REASON_RO_HOLD},
		{"read-only server", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Startup = fixedStartup{t: e.tr, st: ro}
		}, opsv1.ErrorReason_ERROR_REASON_RO_HOLD},
		{"stale epoch", func(_ *env, _ *opsv1.OperationKey, h *opsv1.RequestHeader) { h.Installation.ActivationEpoch = 0 },
			opsv1.ErrorReason_ERROR_REASON_FENCE_STALE},
		{"other installation", func(_ *env, _ *opsv1.OperationKey, h *opsv1.RequestHeader) { h.Installation.InstallationId = "other" },
			opsv1.ErrorReason_ERROR_REASON_INSTALLATION_MISMATCH},
		{"unsupported revision", func(_ *env, _ *opsv1.OperationKey, h *opsv1.RequestHeader) { h.ContractRevision = 2 },
			opsv1.ErrorReason_ERROR_REASON_UNSUPPORTED_CONTRACT},
		{"bad namespace token", func(_ *env, k *opsv1.OperationKey, _ *opsv1.RequestHeader) { k.NamespaceId = "sns1.forged" },
			opsv1.ErrorReason_ERROR_REASON_NAMESPACE_INVALID},
		{"no namespace port", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) { e.f.Namespaces = nil },
			opsv1.ErrorReason_ERROR_REASON_NAMESPACE_INVALID},
		{"client digest mismatch", func(_ *env, k *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			k.RequestMeaningSha256 = make([]byte, 32)
		}, opsv1.ErrorReason_ERROR_REASON_MEANING_DIGEST_MISMATCH},
		{"meaning verifier unavailable", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = specMeanings{t: e.tr, err: errors.New("verifier unavailable")}
		}, opsv1.ErrorReason_ERROR_REASON_RO_HOLD},
		{"meaning typed out-of-set reason", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = specMeanings{t: e.tr, err: &opsfacade.Error{Reason: opsv1.ErrorReason_ERROR_REASON_AUTH_REQUIRED}}
		}, opsv1.ErrorReason_ERROR_REASON_RO_HOLD},
		{"transfer not frozen", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = specMeanings{t: e.tr, err: &opsfacade.Error{Reason: opsv1.ErrorReason_ERROR_REASON_TRANSFER_NOT_FROZEN}}
		}, opsv1.ErrorReason_ERROR_REASON_TRANSFER_NOT_FROZEN},
		{"meaning field unknown", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = specMeanings{t: e.tr, err: &opsfacade.Error{Reason: opsv1.ErrorReason_ERROR_REASON_FIELD_UNKNOWN}}
		}, opsv1.ErrorReason_ERROR_REASON_FIELD_UNKNOWN},
		{"meaning field invalid (wrapped)", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = specMeanings{t: e.tr, err: fmt.Errorf("adapter: %w", &opsfacade.Error{Reason: opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID})}
		}, opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		{"member kind rejected", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = specMeanings{t: e.tr, err: &opsfacade.Error{Reason: opsv1.ErrorReason_ERROR_REASON_MEMBER_KIND_REJECTED}}
		}, opsv1.ErrorReason_ERROR_REASON_MEMBER_KIND_REJECTED},
		{"meaning digest not 32 bytes", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = fixedMeaning{t: e.tr, m: opsfacade.VerifiedMeaning{SHA256: make([]byte, 31), Kind: "LOCAL_IMPORT"}}
		}, opsv1.ErrorReason_ERROR_REASON_RO_HOLD},
		{"meaning without kind", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			e.f.Meanings = fixedMeaning{t: e.tr, m: opsfacade.VerifiedMeaning{SHA256: make([]byte, 32)}}
		}, opsv1.ErrorReason_ERROR_REASON_RO_HOLD},
		{"empty key", func(_ *env, k *opsv1.OperationKey, _ *opsv1.RequestHeader) { k.IdempotencyKey = "" },
			opsv1.ErrorReason_ERROR_REASON_FIELD_UNSPECIFIED},
		{"key 129 bytes", func(_ *env, k *opsv1.OperationKey, _ *opsv1.RequestHeader) {
			k.IdempotencyKey = strings.Repeat("a", 129)
		},
			opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		{"key with space", func(_ *env, k *opsv1.OperationKey, _ *opsv1.RequestHeader) { k.IdempotencyKey = "a b" },
			opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		{"key with slash", func(_ *env, k *opsv1.OperationKey, _ *opsv1.RequestHeader) { k.IdempotencyKey = "a/b" },
			opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		{"key non-ascii", func(_ *env, k *opsv1.OperationKey, _ *opsv1.RequestHeader) { k.IdempotencyKey = "kéy" },
			opsv1.ErrorReason_ERROR_REASON_FIELD_INVALID},
		{"no meaning port", func(e *env, _ *opsv1.OperationKey, _ *opsv1.RequestHeader) { e.f.Meanings = nil },
			opsv1.ErrorReason_ERROR_REASON_RO_HOLD},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			s := spec("genome-a")
			k, h := key(e, "k1", s), header()
			c.mutate(e, k, h)
			e.tr.calls = nil
			_, err := e.f.SubmitJob(e.ctx, e.caller, &opsv1.SubmitJobRequest{Header: h, Key: k, Spec: s})
			if got := opsfacade.ReasonOf(err); got != c.want {
				t.Fatalf("reason = %v (%v), want %v", got, err, c.want)
			}
			for _, call := range e.tr.calls {
				if call == "ledger.Accept" || call == "ledger.Lookup" {
					t.Fatalf("ledger reached on refusal: %v", e.tr.calls)
				}
			}
			if jobs, _ := e.store.UnfinishedJobs(e.ctx); len(jobs) != 0 {
				t.Fatalf("ledger mutated on refusal: %d jobs", len(jobs))
			}
		})
	}
}

func TestZeroFacadeRefusesEverything(t *testing.T) {
	var f opsfacade.Facade
	ctx := context.Background()
	s := spec("genome-a")
	k := &opsv1.OperationKey{NamespaceId: "sns1.tok", IdempotencyKey: "k1", RequestMeaningSha256: make([]byte, 32)}
	if _, err := f.SubmitJob(ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: k, Spec: s}); err == nil {
		t.Fatal("zero facade accepted a submit")
	}
	if _, err := f.LookupJobByKey(ctx, alice, &opsv1.LookupJobByKeyRequest{Header: header(), Key: k, Spec: s}); err == nil {
		t.Fatal("zero facade answered a lookup")
	}
	if _, err := f.GetJob(ctx, alice, &opsv1.GetJobRequest{Header: header(), JobId: "job_x"}); err == nil {
		t.Fatal("zero facade answered a get")
	}
	if err := f.CheckCursor(ctx, alice, "anything"); opsfacade.ReasonOf(err) != opsv1.ErrorReason_ERROR_REASON_CURSOR_INVALID_RELIST {
		t.Fatalf("zero facade cursor = %v, want CURSOR_INVALID_RELIST", err)
	}
}

func TestCallOrder(t *testing.T) {
	e := newEnv(t)
	s := spec("genome-a")
	k := key(e, "k1", s)
	e.tr.calls = nil
	if _, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: k, Spec: s}); err != nil {
		t.Fatal(err)
	}
	want := []string{"startup", "namespace", "authorize", "meaning", "ledger.Accept"}
	if !reflect.DeepEqual(e.tr.calls, want) {
		t.Fatalf("submit calls = %v, want %v", e.tr.calls, want)
	}
	e.tr.calls = nil
	if _, err := e.f.LookupJobByKey(e.ctx, alice, &opsv1.LookupJobByKeyRequest{Header: header(), Key: k, Spec: s}); err != nil {
		t.Fatal(err)
	}
	want[4] = "ledger.Lookup"
	if !reflect.DeepEqual(e.tr.calls, want) {
		t.Fatalf("lookup calls = %v, want %v", e.tr.calls, want)
	}
}

func TestGetJobDeniedEqualsMissing(t *testing.T) {
	e := newEnv(t)
	s := spec("genome-a")
	sub, err := e.f.SubmitJob(e.ctx, alice, &opsv1.SubmitJobRequest{Header: header(), Key: key(e, "k1", s), Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.f.GetJob(e.ctx, alice, &opsv1.GetJobRequest{Header: header(), JobId: sub.Job.GetJobId()})
	if err != nil || got.GetJobId() != sub.Job.GetJobId() {
		t.Fatalf("get = %v %v", got, err)
	}
	if got.GetAcceptance() != opsv1.AcceptanceState_ACCEPTANCE_STATE_UNKNOWN {
		t.Fatalf("get by id acceptance = %v, want UNKNOWN (no key-mapping proof on this read)", got.GetAcceptance())
	}
	if _, err := e.f.GetJob(e.ctx, alice, &opsv1.GetJobRequest{Header: header(), JobId: "job_missing"}); !errors.Is(err, opsfacade.ErrNotFound) {
		t.Fatalf("missing job err = %v", err)
	}
	e.f.Authorizer = fixedAuth{t: e.tr, d: opsfacade.AuthDeny}
	e.tr.calls = nil
	if _, err := e.f.GetJob(e.ctx, alice, &opsv1.GetJobRequest{Header: header(), JobId: sub.Job.GetJobId()}); !errors.Is(err, opsfacade.ErrNotFound) {
		t.Fatalf("denied get err = %v, want the same ErrNotFound", err)
	}
	for _, c := range e.tr.calls {
		if c == "ledger.GetJob" {
			t.Fatalf("ledger read on denied get: %v", e.tr.calls)
		}
	}
	e.f.Authorizer = fixedAuth{t: e.tr, d: opsfacade.AuthUnknown}
	if _, err := e.f.GetJob(e.ctx, alice, &opsv1.GetJobRequest{Header: header(), JobId: sub.Job.GetJobId()}); opsfacade.ReasonOf(err) !=
		opsv1.ErrorReason_ERROR_REASON_AUTH_PROVIDER_UNKNOWN {
		t.Fatalf("unknown-auth get err = %v", err)
	}
}

type fixedCursor struct{ v opsfacade.CursorVerdict }

func (c fixedCursor) Classify(context.Context, opsfacade.Caller, string) (opsfacade.CursorVerdict, error) {
	return c.v, nil
}

func TestCursorOnlyValidContinues(t *testing.T) {
	f := opsfacade.Facade{}
	for name, v := range map[string]opsfacade.CursorVerdict{"invalid": opsfacade.CursorInvalid, "expired": opsfacade.CursorExpired} {
		f.Cursors = fixedCursor{v: v}
		if err := f.CheckCursor(context.Background(), alice, "c"); opsfacade.ReasonOf(err) != opsv1.ErrorReason_ERROR_REASON_CURSOR_INVALID_RELIST {
			t.Fatalf("%s cursor = %v, want CURSOR_INVALID_RELIST", name, err)
		}
	}
	f.Cursors = fixedCursor{v: opsfacade.CursorValid}
	if err := f.CheckCursor(context.Background(), alice, "c"); err != nil {
		t.Fatalf("valid cursor = %v", err)
	}
}

func TestProjectJob(t *testing.T) {
	base := jobledger.Job{JobID: "job_a", Kind: "OCI_ACQUIRE", CreatedEpoch: 3, RevisionID: "rev_a", RecordVersion: 7,
		LastError: "registry said: secret-token-123"}
	for _, ph := range []jobledger.Phase{jobledger.PhaseQueued, jobledger.PhaseRunning, jobledger.PhaseCancelRequested,
		jobledger.PhaseSucceeded, jobledger.PhaseFailed, jobledger.PhaseCancelled, jobledger.PhaseHold} {
		j := base
		j.Phase = ph
		if ph.Terminal() {
			j.Terminal = &jobledger.Receipt{Phase: ph}
		}
		got, err := opsfacade.ProjectJob(j, true)
		if err != nil {
			t.Fatal(err)
		}
		if got.GetPhase().String() != "JOB_PHASE_"+string(ph) {
			t.Fatalf("phase %s -> %v", ph, got.GetPhase())
		}
		if got.GetEffect() != opsv1.EffectState_EFFECT_STATE_UNKNOWN || got.GetVerification() != opsv1.VerificationState_VERIFICATION_STATE_UNKNOWN {
			t.Fatalf("phase %s: effect %v verification %v, want UNKNOWN without durable evidence", ph, got.GetEffect(), got.GetVerification())
		}
		if got.GetLastError() != nil {
			t.Fatalf("phase %s: legacy LastError projected as %v", ph, got.GetLastError())
		}
		if got.GetAcceptance() != opsv1.AcceptanceState_ACCEPTANCE_STATE_ACCEPTED || got.GetKind() != opsv1.JobKind_JOB_KIND_OCI_ACQUIRE ||
			got.GetRevisionId() != "rev_a" || got.GetCreatedActivationEpoch() != 3 || got.GetRecordVersion() != 7 {
			t.Fatalf("phase %s: projection %v", ph, got)
		}
	}
	for name, j := range map[string]jobledger.Job{
		"unknown kind":  {JobID: "job_a", Kind: "SOMETHING", Phase: jobledger.PhaseQueued},
		"unknown phase": {JobID: "job_a", Kind: "EXPORT", Phase: "PAUSED"},
	} {
		if _, err := opsfacade.ProjectJob(j, true); err == nil {
			t.Fatalf("%s projected", name)
		}
	}
}
