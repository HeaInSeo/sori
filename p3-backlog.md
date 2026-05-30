# P3 Backlog

Items explicitly deferred from the current sprint. Each entry describes the
problem, the accepted trade-off in the current code, and the intended design
when the item is picked up.

---

## P3-1: Large dataset publish-path hardening

**Historical problem**
The original publish path accumulated each compressed layer in a `bytes.Buffer`
(`publishVolumeToStore` + public `TarGzDir` / `TarGzDirFiles` helpers), holding
all layer bytes in memory simultaneously. For multi-GB datasets this risked OOM.

**Current state — core publish path**
`publishVolumeToStore` now uses `buildAndPushTempLayer` (commit `c540719`):
each layer is written to a temp file, pushed, and the file is closed and removed
before the next layer begins. At most one layer file is open at a time regardless
of partition count. Memory pressure from layer content is eliminated on the core
path.

**Remaining limitation — public helper API**
`TarGzDir(fsDir, prefixPath string) ([]byte, error)` and
`TarGzDirFiles(fsDir, prefixPath string, …) (bool, []byte, error)` still
accumulate the compressed stream into a `[]byte` before returning. These are
utility/test-facing helpers; they are not used by the core publish path.
Large-dataset callers should use the `…To(w io.Writer, …)` streaming variants
(`TarGzDirTo`, `TarGzDirFilesTo`) directly.

**Deferred design**
Disk-safety hardening (Step 2) and chunked CAS (Step 3) remain unimplemented.
Step 3 is the high-value item for large genomics datasets; see p5-rfc.md.

### ✅ Step 1 — temp-file-backed tar layer (done, commit `329ea8b` + `c540719`)

- Each layer is written to a temp file (`os.CreateTemp`) instead of a
  `bytes.Buffer`.
- Digest and size are computed in a single pass using
  `io.MultiWriter(tmp, hash, countWriter)`.
- The temp file is seeked to 0 and passed to ORAS push; removed immediately
  after each layer push via `buildAndPushTempLayer` (commit `c540719`), so
  at most one temp file is open at a time.
- `TarGzDirTo(w io.Writer, …)` and `TarGzDirFilesTo(w io.Writer, …)` were
  extracted so callers can write directly to any `io.Writer`.

### ⏳ Step 2 — spooling and disk-safety improvement (future, low priority)

**Scope correction**: "true streaming push" is a misnomer.  ORAS
`Store.Push` requires `Descriptor.Digest` and `Descriptor.Size` before the
stream is read, so the full content must be produced before pushing regardless.
A tar.gz stream cannot report its final size until the last byte is written.

What Step 2 actually covers (if ever picked up):

- Disk preflight check: verify available space before starting a large layer.
- Clearer error messages when temp write fails due to disk full.
- Code boundary cleanup to make room for Step 3 integration.

What Step 2 does **not** cover:

- Eliminating the temp file (not possible without chunked CAS).
- Solving disk spike for large datasets (that is Step 3's job).
- Resumable upload (that is Step 3's job).

### ⏳ Step 3 — chunked CAS (tracked in p5-rfc.md)

File-aware fixed-size chunked CAS for large genomics datasets.
Design decisions and test plan are in `p5-rfc.md`.

---

## ✅ P3-2: Remote registry fetch API (done)

### P3-2A — ReadOnlyTarget internal refactor (commit `96a6fa4`)

- Extracted `fetchVolSeqFrom` and `fetchVolParallelFrom` accepting
  `oras.ReadOnlyTarget`; `FetchVolSeq` / `FetchVolParallel` are now thin
  wrappers that open `oci.New(repo)` and delegate.
- `fetchVolWithStaging` and `fetchVolWithAtomicOverwrite` likewise open the
  store once and call the internal `…From` variants.
- No public API change; all existing tests pass.

### P3-2B — `Client.FetchVolumeFromRemote` (commit `b9753fa`)

- Added `fetchVolWithStagingFrom` and `fetchVolWithAtomicOverwriteFrom`
  accepting `oras.ReadOnlyTarget`, shared by both local and remote callers.
- `Client.FetchVolumeFromRemote(ctx, destRoot, target RemoteTarget, tag string,
  opts FetchOptions)` creates a `remote.Repository` via
  `registryutil.NewRepository`, injects `c.httpClient` if set, and routes to
  the staged or atomic-overwrite path.
- Remote fetch always uses the staging pipeline; direct extraction into
  `destRoot` is not offered.
- `RequireEmptyDestination` and `AtomicOverwrite` are mutually exclusive
  (`ErrValidation`).
- Tests (`remote_fetch_test.go`): full round-trip via `httptest` mock OCI
  registry, tag-not-found → `ErrNotFound`, corrupt manifest → `ErrIntegrity`,
  four input-validation cases → `ErrValidation`.

---

## ✅ P3-3: Staging directory design for fetch integrity (done)

### Problem (original)

`FetchVolSeq` and `FetchVolParallel` extracted layers directly into `destRoot`.
A mid-fetch failure left `destRoot` in a partial, indistinguishable state.

### What was implemented

**Staging path** (`RequireEmptyDestination=true`, commit `329ea8b`):

```
ensureDestinationAbsent(destRoot)    // fail if destRoot already exists
stagingDir = MkdirTemp(parent, …)   // sibling of destRoot, same filesystem
fetchVolSeqFrom / fetchVolParallelFrom → stagingDir
validateStagingDir(…)               // partition dirs present + configblob.json valid JSON
os.Rename(stagingDir, destRoot)     // atomic commit on success
os.RemoveAll(stagingDir)            // deferred cleanup on failure
```

**`validateStagingDir`** (extracted helper): checks that every partition
directory listed in `vi.Partitions` exists under staging, and that
`configblob.json` (if present) is valid JSON.

**`ensureDestinationAbsent`**: renamed from `ensureEmptyDir` to match actual
semantics — rejects `destRoot` even if it is empty.

**AtomicOverwrite path** (`FetchOptions.AtomicOverwrite=true`, commit
`b6b9fd6`):

```
Phase 1 — extract to staging sibling
Phase 2 — os.Rename(destRoot, backupPath)   // backup existing dest
Phase 3 — os.Rename(stagingDir, destRoot)   // atomic commit
Cleanup — os.RemoveAll(backupPath)           // best-effort
```

- `backupPath` obtained via `uniqueBackupPath` (temp-file trick for guaranteed
  absent path; no pre-created directory that would block `os.Rename`).
- Phase 3 failure triggers best-effort rollback; if rollback also fails the
  error message includes both `stagingDir` and `backupPath` for manual
  recovery.
- `AtomicOverwrite` and `RequireEmptyDestination` are mutually exclusive
  (`ErrValidation`).
- Package-level test hooks (`testHookPhase2RenameErr`, `testHookPhase3RenameErr`,
  `testHookBackupCleanupErr`) enable targeted failure injection in tests.

**Cross-filesystem note**: `MkdirTemp` is called with `filepath.Dir(destRoot)`
as the parent, ensuring staging is always on the same filesystem as `destRoot`
so that `os.Rename` remains atomic.

---

## Fetch safety policy

Three fetch modes are available, in decreasing order of safety:

**AtomicOverwrite** (`FetchOptions{AtomicOverwrite: true}`) — recommended for
update workflows:
- Extracts to a staging sibling, backs up existing `destRoot`, commits
  atomically.  `destRoot` is always fully populated or fully restored.

**Safe fetch** (`FetchOptions{RequireEmptyDestination: true}`) — recommended
for fresh installs:
- `destRoot` must not exist. Extracts to staging, commits on full success.
  `destRoot` is either absent (failure) or fully populated (success).

**Remote fetch** (`Client.FetchVolumeFromRemote`) — always uses safe fetch by
default; `AtomicOverwrite` opt-in supported.

**Legacy direct fetch** (`RequireEmptyDestination: false`, default):
- Extracts directly into `destRoot`. A mid-fetch failure leaves a partial
  state. Retained for backward compatibility with existing callers only.

---

## Open items

- **P3-1 Step 2**: spooling/disk-safety cleanup — low priority, scope defined above.
- **P3-1 Step 3**: chunked CAS — design captured in `p5-rfc.md`; implementation pending.

## ✅ FetchVolumeFresh convenience helper (done)

`Client.FetchVolumeFresh(ctx, destRoot, repo, tag string, concurrency int)` added
as a zero-config safe-fetch wrapper: implicitly sets `RequireEmptyDestination:true`
and delegates to `FetchVolume`.

- `destRoot` must not exist; returns `ErrConflict` if it does.
- On success `destRoot` is fully populated (staging rename).
- On failure `destRoot` is left absent (staging is removed).
- Tests: `RejectsExistingDestination`, `UsesStagingAndCommitsOnSuccess`,
  `LeavesDestAbsentOnFailure`.
