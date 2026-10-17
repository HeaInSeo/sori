package jobledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Namespace states.
const (
	NamespaceActive = "ACTIVE"
)

// NamespaceRecord is a server-issued dedup namespace (D01.2 B.2.2). Token encoding
// and MAC verification belong to the RPC layer; this record is what a verified token
// must match.
type NamespaceRecord struct {
	NamespaceID      string
	InstallationID   string
	PrincipalSubject string
	ActivationEpoch  uint64
	State            string
	IssuedAt         time.Time
}

// NamespaceRef is what a verified namespace token claims.
type NamespaceRef struct {
	NamespaceID      string
	InstallationID   string
	PrincipalSubject string
	ActivationEpoch  uint64
}

// Ref returns the reference a token for this namespace carries.
func (n NamespaceRecord) Ref() NamespaceRef {
	return NamespaceRef{NamespaceID: n.NamespaceID, InstallationID: n.InstallationID, PrincipalSubject: n.PrincipalSubject, ActivationEpoch: n.ActivationEpoch}
}

// Phase is a Job phase (D01.4 job.proto JobPhase).
type Phase string

// Job phases.
const (
	PhaseQueued          Phase = "QUEUED"
	PhaseRunning         Phase = "RUNNING"
	PhaseCancelRequested Phase = "CANCEL_REQUESTED"
	PhaseSucceeded       Phase = "SUCCEEDED"
	PhaseFailed          Phase = "FAILED"
	PhaseCancelled       Phase = "CANCELLED"
	PhaseHold            Phase = "HOLD"
)

// Terminal reports whether p is a terminal phase.
func (p Phase) Terminal() bool {
	return p == PhaseSucceeded || p == PhaseFailed || p == PhaseCancelled
}

// Receipt is a Job's terminal receipt. It is written once and never changed.
type Receipt struct {
	Phase      Phase
	RevisionID string
	Error      string
	RecordedAt time.Time
}

// Job is one durable Job record. Kind, NamespaceID, IdempotencyKey, MeaningDigest,
// DomainOpKey and CreatedEpoch are fixed at acceptance; RevisionID is set at most once.
type Job struct {
	JobID          string
	Kind           string
	NamespaceID    string
	IdempotencyKey string
	MeaningDigest  string
	DomainOpKey    string
	CreatedEpoch   uint64
	AcceptedAt     time.Time

	Phase             Phase
	CancelRequested   bool
	CancelRequestedBy string
	RevisionID        string
	LastError         string
	Terminal          *Receipt
	RecordVersion     uint64
}

// Dispatchable reports whether the Job may be dispatched: it has a durable key
// mapping (every stored Job has one), is not terminal and has no cancel intent.
func (j Job) Dispatchable() bool {
	return (j.Phase == PhaseQueued || j.Phase == PhaseRunning) && !j.CancelRequested
}

// Outcome classifies a (namespace, key, meaning) against this store.
type Outcome string

// Outcomes.
const (
	OutcomeNew      Outcome = "NEW"
	OutcomeExisting Outcome = "EXISTING"
	OutcomeConflict Outcome = "CONFLICT"
	OutcomeHold     Outcome = "RECONCILE_HOLD"
	OutcomeInvalid  Outcome = "NAMESPACE_INVALID"
)

// Decision is a classification result. Row names the D01.2 B.2.3 row that matched.
type Decision struct {
	Outcome Outcome
	Row     string
	Job     *Job
}

// AcceptRequest is one SubmitJob acceptance after authentication, authorization and
// request-meaning computation.
type AcceptRequest struct {
	Namespace      NamespaceRef
	Principal      string
	IdempotencyKey string
	MeaningDigest  string
	Kind           string
}

// AllocateNamespace durably issues a namespace for principal in the Guard's epoch.
// The record is committed before it is returned.
func (s *Store) AllocateNamespace(ctx context.Context, g Guard, principal string) (NamespaceRecord, error) {
	var out NamespaceRecord
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := fence(ctx, tx, g); err != nil {
			return err
		}
		id, err := newID("nsr")
		if err != nil {
			return err
		}
		out = NamespaceRecord{NamespaceID: id, InstallationID: g.InstallationID, PrincipalSubject: principal,
			ActivationEpoch: g.ActivationEpoch, State: NamespaceActive, IssuedAt: s.now().UTC()}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO namespaces (namespace_id, record) VALUES (?, ?)`, id, string(raw)); err != nil {
			return fmt.Errorf("jobledger: insert namespace: %w", err)
		}
		return appendLedger(ctx, tx, "namespace", id, out)
	})
	if err != nil {
		return NamespaceRecord{}, err
	}
	return out, nil
}

// Lookup classifies a key without changing anything (LookupJobByKey). It works on a
// read-only store too.
func (s *Store) Lookup(ctx context.Context, g Guard, req AcceptRequest) (Decision, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Decision{}, fmt.Errorf("jobledger: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	m, err := readMarker(ctx, tx)
	if err != nil {
		return Decision{}, err
	}
	return classify(ctx, tx, g, m, req)
}

// classify implements D01.2 B.2.3 (N0, N1, N8, then N2/N3/N4/N4H/N5 for the current
// epoch and N6/N7 for older epochs). Key tombstones (N2e/N6e) and RETIRED
// namespaces (N9) are not produced by this store yet.
func classify(ctx context.Context, q queryRower, g Guard, m marker, req AcceptRequest) (Decision, error) {
	ns := req.Namespace
	switch {
	case !g.usable():
		return Decision{Outcome: OutcomeHold, Row: "N0"}, nil
	case ns.InstallationID != g.InstallationID || ns.PrincipalSubject != req.Principal:
		return Decision{Outcome: OutcomeInvalid, Row: "N1"}, nil
	case ns.ActivationEpoch > g.ActivationEpoch:
		return Decision{Outcome: OutcomeHold, Row: "N8"}, nil
	}
	job, found, err := jobByKey(ctx, q, ns.NamespaceID, req.IdempotencyKey)
	if err != nil {
		return Decision{}, err
	}
	current := ns.ActivationEpoch == g.ActivationEpoch
	if found {
		row := map[bool]string{true: "N2", false: "N6"}[current]
		if job.MeaningDigest != req.MeaningDigest {
			return Decision{Outcome: OutcomeConflict, Row: map[bool]string{true: "N3", false: "N6"}[current], Job: &job}, nil
		}
		return Decision{Outcome: OutcomeExisting, Row: row, Job: &job}, nil
	}
	if !current {
		return Decision{Outcome: OutcomeHold, Row: "N7"}, nil
	}
	rec, ok, err := namespaceByID(ctx, q, ns.NamespaceID)
	switch {
	case err != nil:
		return Decision{}, err
	case !ok:
		return Decision{Outcome: OutcomeHold, Row: "N5"}, nil
	case rec.Ref() != ns:
		return Decision{Outcome: OutcomeInvalid, Row: "N1"}, nil
	case !m.liveCurrent(g):
		return Decision{Outcome: OutcomeHold, Row: "N4H"}, nil
	}
	return Decision{Outcome: OutcomeNew, Row: "N4"}, nil
}

// Accept is the atomic durable acceptance boundary (D01.2 r1 §2): in one
// transaction it fences the Guard, classifies the key and, for a new key, writes
// the key→Job→domain-operation mapping together with the dispatchable Job. A replay
// with the same key and meaning returns the same Job (EXISTING) and writes nothing.
// A process that dies before the commit leaves no Job and no mapping.
func (s *Store) Accept(ctx context.Context, g Guard, req AcceptRequest) (Decision, error) {
	var out Decision
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		m, err := fence(ctx, tx, g)
		if err != nil {
			return err
		}
		if out, err = classify(ctx, tx, g, m, req); err != nil {
			return err
		}
		switch out.Outcome {
		case OutcomeNew:
			job, err := s.insertJob(ctx, tx, g, req)
			out.Job = &job
			return err
		case OutcomeExisting:
			return nil
		case OutcomeConflict:
			return fmt.Errorf("%w: key %q in namespace %s (%s)", ErrIdempotencyConflict, req.IdempotencyKey, req.Namespace.NamespaceID, out.Row)
		case OutcomeInvalid:
			return fmt.Errorf("%w (%s)", ErrNamespaceInvalid, out.Row)
		default:
			return fmt.Errorf("%w (%s)", ErrReconcileHold, out.Row)
		}
	})
	if err != nil {
		return Decision{}, err
	}
	return out, nil
}

func (s *Store) insertJob(ctx context.Context, tx *sql.Tx, g Guard, req AcceptRequest) (Job, error) {
	jobID, err := newID("job")
	if err != nil {
		return Job{}, err
	}
	dop, err := newID("dop")
	if err != nil {
		return Job{}, err
	}
	job := Job{
		JobID: jobID, Kind: req.Kind, NamespaceID: req.Namespace.NamespaceID, IdempotencyKey: req.IdempotencyKey,
		MeaningDigest: req.MeaningDigest, DomainOpKey: dop, CreatedEpoch: g.ActivationEpoch, AcceptedAt: s.now().UTC(),
		Phase: PhaseQueued, RecordVersion: 1,
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return Job{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency (namespace_id, client_key, meaning_digest, job_id, domain_op_key, record)
		VALUES (?, ?, ?, ?, ?, ?)`, job.NamespaceID, job.IdempotencyKey, job.MeaningDigest, job.JobID, job.DomainOpKey, string(raw)); err != nil {
		return Job{}, fmt.Errorf("jobledger: insert key mapping: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (job_id, record_version, record) VALUES (?, ?, ?)`,
		job.JobID, job.RecordVersion, string(raw)); err != nil {
		return Job{}, fmt.Errorf("jobledger: insert job: %w", err)
	}
	return job, appendLedger(ctx, tx, "accept", job.JobID, job)
}

func namespaceByID(ctx context.Context, q queryRower, id string) (NamespaceRecord, bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM namespaces WHERE namespace_id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return NamespaceRecord{}, false, nil
	}
	if err != nil {
		return NamespaceRecord{}, false, fmt.Errorf("jobledger: read namespace: %w", err)
	}
	var rec NamespaceRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return NamespaceRecord{}, false, fmt.Errorf("jobledger: decode namespace: %w", err)
	}
	return rec, true, nil
}

func jobByKey(ctx context.Context, q queryRower, namespaceID, key string) (Job, bool, error) {
	var jobID string
	err := q.QueryRowContext(ctx, `SELECT job_id FROM idempotency WHERE namespace_id = ? AND client_key = ?`, namespaceID, key).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("jobledger: read key mapping: %w", err)
	}
	job, ok, err := jobByID(ctx, q, jobID)
	if err == nil && !ok {
		err = fmt.Errorf("%w: key mapping without job %s", ErrLedgerIntegrity, jobID)
	}
	return job, ok, err
}

func jobByID(ctx context.Context, q queryRower, id string) (Job, bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM jobs WHERE job_id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("jobledger: read job: %w", err)
	}
	var job Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		return Job{}, false, fmt.Errorf("jobledger: decode job: %w", err)
	}
	return job, true, nil
}

// GetJob returns a Job by id.
func (s *Store) GetJob(ctx context.Context, id string) (Job, bool, error) {
	return jobByID(ctx, s.db, id)
}

// UnfinishedJobs returns every Job without a terminal receipt, in acceptance order,
// for reconcile after a restart.
func (s *Store) UnfinishedJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.record FROM jobs j JOIN idempotency i ON i.job_id = j.job_id ORDER BY i.rowid`)
	if err != nil {
		return nil, fmt.Errorf("jobledger: list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("jobledger: list jobs: %w", err)
		}
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			return nil, fmt.Errorf("jobledger: decode job: %w", err)
		}
		if job.Terminal == nil {
			out = append(out, job)
		}
	}
	return out, rows.Err()
}

// Transition is a requested Job phase change.
type Transition struct {
	Phase      Phase
	RevisionID string // set once; a different later value is a conflict
	Error      string
}

// allowed lists the phase edges. RUNNING→RUNNING records progress such as the
// accepted RevisionID. CANCEL_REQUESTED and HOLD can still end SUCCEEDED or FAILED:
// an effect that already committed is reported, not undone. HOLD keeps its phase
// when cancel intent is recorded, so reconciliation records a committed success
// directly from HOLD rather than resuming through RUNNING.
var allowed = map[Phase][]Phase{
	PhaseQueued:          {PhaseRunning, PhaseCancelled, PhaseFailed, PhaseHold},
	PhaseRunning:         {PhaseRunning, PhaseSucceeded, PhaseFailed, PhaseHold},
	PhaseCancelRequested: {PhaseCancelled, PhaseSucceeded, PhaseFailed, PhaseHold},
	PhaseHold:            {PhaseRunning, PhaseSucceeded, PhaseFailed, PhaseCancelled},
}

func edgeAllowed(from, to Phase) bool {
	for _, p := range allowed[from] {
		if p == to {
			return true
		}
	}
	return false
}

// Advance moves a Job to tr.Phase if its record version is expectedVersion (CAS).
// A terminal phase writes the terminal receipt. RevisionID, once recorded, never
// changes. A Job with cancel intent keeps it.
func (s *Store) Advance(ctx context.Context, g Guard, jobID string, expectedVersion uint64, tr Transition) (Job, error) {
	return s.mutateJob(ctx, g, jobID, func(job *Job) error {
		if job.RecordVersion != expectedVersion {
			return fmt.Errorf("%w: job %s at version %d, update from %d", ErrVersionConflict, jobID, job.RecordVersion, expectedVersion)
		}
		if !edgeAllowed(job.Phase, tr.Phase) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, job.Phase, tr.Phase)
		}
		// A Job with cancel intent is never resumed, including from HOLD, where
		// RequestCancel keeps the phase.
		if job.CancelRequested && (tr.Phase == PhaseRunning || tr.Phase == PhaseQueued) {
			return fmt.Errorf("%w: job %s has cancel intent", ErrInvalidTransition, jobID)
		}
		if tr.RevisionID != "" {
			if job.RevisionID != "" && job.RevisionID != tr.RevisionID {
				return fmt.Errorf("%w: job %s already records revision %s", ErrDescriptorConflict, jobID, job.RevisionID)
			}
			job.RevisionID = tr.RevisionID
		}
		job.Phase, job.LastError = tr.Phase, tr.Error
		if tr.Phase.Terminal() {
			job.Terminal = &Receipt{Phase: tr.Phase, RevisionID: job.RevisionID, Error: tr.Error, RecordedAt: s.now().UTC()}
		}
		return nil
	})
}

// RequestCancel durably records cancel intent (CancelJob). It is idempotent: a Job
// that already has cancel intent is returned unchanged. It never deletes the key
// mapping, the domain operation key or an accepted RevisionID.
func (s *Store) RequestCancel(ctx context.Context, g Guard, jobID, actor string) (Job, error) {
	return s.mutateJob(ctx, g, jobID, func(job *Job) error {
		if job.CancelRequested {
			return errUnchanged
		}
		job.CancelRequested, job.CancelRequestedBy = true, actor
		if job.Phase != PhaseHold {
			job.Phase = PhaseCancelRequested
		}
		return nil
	})
}

var errUnchanged = errors.New("unchanged")

// mutateJob runs one fenced read-modify-write of a Job record and appends it to the
// ledger. fn returning errUnchanged commits nothing and returns the stored Job.
func (s *Store) mutateJob(ctx context.Context, g Guard, jobID string, fn func(*Job) error) (Job, error) {
	var out Job
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := fence(ctx, tx, g); err != nil {
			return err
		}
		job, ok, err := jobByID(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: job %s", ErrNotFound, jobID)
		}
		if job.Terminal != nil {
			return fmt.Errorf("%w: job %s is %s", ErrAlreadyTerminal, jobID, job.Phase)
		}
		out = job
		if err := fn(&job); err != nil {
			return err
		}
		job.RecordVersion++
		raw, err := json.Marshal(job)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET record_version = ?, record = ? WHERE job_id = ? AND record_version = ?`,
			job.RecordVersion, string(raw), jobID, out.RecordVersion)
		if err != nil {
			return fmt.Errorf("jobledger: update job: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.Join(fmt.Errorf("%w: job %s changed during update", ErrVersionConflict, jobID), err)
		}
		out = job
		return appendLedger(ctx, tx, "job", jobID, job)
	})
	if errors.Is(err, errUnchanged) {
		return out, nil
	}
	if err != nil {
		return Job{}, err
	}
	return out, nil
}
