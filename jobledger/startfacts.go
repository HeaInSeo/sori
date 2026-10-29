package jobledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// StartFacts are the read-only facts a server start classifier needs about one
// job ledger (SORI-D01-W021B-FMT-PREP-r2 §9.1 P-b). They are reported as facts,
// not as the three offline-init outcomes of InspectInitState, so that "not
// content-empty", "RESTORED_INACTIVE marker" and "unknown schema" stay apart.
type StartFacts struct {
	// Pristine: no database file (and no sidecar), an empty file, or an SQLite
	// file with no schema object. A pristine path is reported, never created.
	Pristine bool
	// SchemaKnown: the schema objects are exactly this version's schema and
	// schema_version is this version. False for a pristine store.
	SchemaKnown bool
	// ContentEmpty: no row in any content table (every table but ledger_meta).
	// Meaningful only when SchemaKnown.
	ContentEmpty bool
	// Marked: installation_id is recorded. The remaining fields are meaningful
	// only when Marked.
	Marked         bool
	InstallationID string
	RestoreState   string
	ActiveEpoch    uint64
	CutSeq         uint64
	RestoreNonce   string
	// InitMarkerOnly: the markers are exactly the four Init markers for
	// InstallationID (LIVE, epoch 1, cut 0) and nothing else besides
	// schema_version.
	InitMarkerOnly bool
}

// InspectStartFacts reads the start facts of the store at path without
// changing it. Like InspectInitState it never uses Open, which would create a
// schema or switch the journal mode; a non-empty file is read through a
// read-only, query-only connection. A store that cannot be read with certainty
// (a non-regular file, a stray sidecar, a failed integrity check, an unreadable
// or malformed marker) returns an error and no facts.
func InspectStartFacts(ctx context.Context, path string) (StartFacts, error) {
	if path == "" || path == ":memory:" {
		return StartFacts{}, fmt.Errorf("%w: %q", ErrTransientPath, path)
	}
	pristine, err := inspectFile(path)
	switch {
	case err != nil:
		return StartFacts{}, fmt.Errorf("jobledger: start facts: %w", err)
	case pristine:
		return StartFacts{Pristine: true}, nil
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: read %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()
	return startFacts(ctx, tx, path)
}

func startFacts(ctx context.Context, tx *sql.Tx, path string) (StartFacts, error) {
	var check string
	if err := tx.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: check %s: %w", path, err)
	}
	if check != "ok" {
		return StartFacts{}, fmt.Errorf("jobledger: start facts: check %s: %s", path, check)
	}
	objects, err := schemaObjects(ctx, tx)
	if err != nil {
		return StartFacts{}, err
	}
	if len(objects) == 0 {
		return StartFacts{Pristine: true}, nil
	}
	if err := checkSchemaObjects(ctx, tx); err != nil {
		if errors.Is(err, ErrSchemaVersion) {
			return StartFacts{}, nil
		}
		return StartFacts{}, err
	}
	meta, err := readMeta(ctx, tx)
	if err != nil {
		return StartFacts{}, err
	}
	if meta[metaSchemaVersion] != strconv.Itoa(SchemaVersion) {
		return StartFacts{}, nil
	}
	f := StartFacts{SchemaKnown: true}
	if f.ContentEmpty, err = contentEmpty(ctx, tx); err != nil {
		return StartFacts{}, err
	}
	id, ok := meta[metaInstallationID]
	if !ok {
		return f, nil
	}
	if id == "" {
		return StartFacts{}, errors.New("jobledger: start facts: empty installation_id marker")
	}
	f.Marked, f.InstallationID = true, id
	f.RestoreState, f.RestoreNonce = meta[metaRestoreState], meta[metaRestoreNonce]
	for key, dst := range map[string]*uint64{metaActiveEpoch: &f.ActiveEpoch, metaCutSeq: &f.CutSeq} {
		v, ok := meta[key]
		if !ok {
			return StartFacts{}, fmt.Errorf("jobledger: start facts: marker %s missing", key)
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != v {
			return StartFacts{}, fmt.Errorf("jobledger: start facts: marker %s = %q", key, v)
		}
		*dst = n
	}
	_, _, cerr := classifyMarkers(meta)
	f.InitMarkerOnly = cerr == nil
	return f, nil
}

// contentEmpty reports whether every content table is empty.
func contentEmpty(ctx context.Context, tx *sql.Tx) (bool, error) {
	err := checkNoContent(ctx, tx)
	switch {
	case err == nil:
		return true, nil
	case isContentFound(err):
		return false, nil
	}
	return false, err
}

// isContentFound reports whether err is checkNoContent's refusal of a
// non-empty content table; any other error is a read failure.
func isContentFound(err error) bool {
	var ce *contentError
	return errors.As(err, &ce)
}
