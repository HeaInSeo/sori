# P3 Backlog

Items explicitly deferred from the current sprint. Each entry describes the
problem, the accepted trade-off in the current code, and the intended design
when the item is picked up.

---

## P3-1: Streaming tar/push for large datasets

**Current behaviour**
`TarGzDir` and `TarGzDirFiles` accumulate the entire compressed archive in a
`bytes.Buffer` before returning it. `publishVolumeToStore` holds all layer
bytes in memory simultaneously. For multi-GB datasets this risks OOM.

**Accepted trade-off**
Acceptable while dataset sizes remain small (development / integration use).

**Intended design when picked up**

Step 1 — temp-file-backed tar layer (recommended first step):
- Write the tar.gz to a temp file instead of a `bytes.Buffer`.
- Compute digest and size while writing (using `io.TeeReader` + `digest.Canonical.Hash()`).
- Re-open the temp file as an `io.Reader` and pass it to ORAS push.
- This requires two passes over the data (write + push) but avoids OOM and is
  straightforward to implement.

Step 2 — true streaming ingest (future):
- Push the tar.gz stream directly without materializing it to disk.
- Requires computing digest on-the-fly while ORAS reads the stream.
- Check whether the ORAS `Store.Push` API accepts a digest-preflight or
  streaming push mode before committing to this approach.

Step 3 — chunked CAS / content-addressable split (separate design):
- Splitting layers into content-addressable chunks is a distinct artifact
  format change. It should be designed and tracked separately, not bundled
  into the streaming fix.

---

## P3-2: Remote registry fetch API

**Current behaviour**
`FetchVolSeq` and `FetchVolParallel` open a **local** OCI store (`oci.New`)
and extract from it. There is no code path that pulls layers directly from a
remote registry (e.g., Harbor, GHCR) into a local directory.

**Accepted trade-off**
The current two-step workflow (remote pull → local OCI store → extract) relies
on external tools such as `oras copy` or similar; it is not a sori internal API.
This is functional for existing callers.

**Intended design when picked up**
- Add `Client.FetchVolumeFromRemote(ctx, destRoot, remoteRepo, tag string,
  target RemoteTarget, opts FetchOptions)`.
- Internally open a `remote.Repository` via `registryutil.NewRepository`,
  resolve the manifest, and stream each layer directly to the staging
  directory (integrating with the staging design below).
- Reuse the existing `fetchVolWithStaging` commit/cleanup logic.
- **Remote fetch must always use the staging pipeline** (staging → validation
  → rename commit). Direct extraction into destRoot is not acceptable for
  remote artifacts because we cannot trust their content before extraction.
- Verify each layer's digest and size against the manifest descriptor before
  passing the data to `untarGzDirFiltered`.
- Auth/TLS/retry configuration is shared with the existing
  `registryutil.RemoteConfig` / `RemoteTarget` types.

---

## P3-3: Staging directory design for fetch integrity

**Problem**
`FetchVolSeq` and `FetchVolParallel` extract layers directly into `destRoot`.
If any layer download or unpack fails midway, `destRoot` is left in a partial
state that is indistinguishable from a complete extraction. Individual
`writeFileAtomic` calls cannot prevent this because they operate per-file, not
per-directory.

**Current mitigation**
`RequireEmptyDestination=true` + `fetchVolWithStaging` (see also "Implemented mitigation" below).

**Implemented mitigation**
The current implementation provides the following guarantee when
`RequireEmptyDestination=true`:

```
ensureDestinationAbsent(destRoot)    // fail if destRoot already exists (even if empty)
stagingDir = MkdirTemp(parent, …)   // sibling of destRoot, same filesystem
FetchVolSeq/Parallel → stagingDir   // all extraction to staging
os.Rename(stagingDir, destRoot)     // atomic directory commit on success
os.RemoveAll(stagingDir)            // deferred cleanup on failure
```

`destRoot` is either untouched (on failure) or fully populated (on success).
`ensureDestinationAbsent` rejects `destRoot` even if it is empty but already
exists, preventing accidental overwrites.

Note: the helper is currently named `ensureEmptyDir` in the source; it should
be renamed to `ensureDestinationAbsent` to match its actual semantics.

Legacy callers that pass `RequireEmptyDestination=false` still get direct
extraction without staging.

**P3 remaining**

1. **Validation before commit** — after staging is fully populated but before
   rename, run a lightweight integrity check (e.g., verify every partition
   directory listed in `volume-index.json` actually exists in staging, and
   that `configblob.json` parses as valid JSON). Reject and clean up if any
   check fails.

2. **Overwrite / update case** — `RequireEmptyDestination=false` with an
   existing `destRoot` still does direct extraction. The correct design is a
   three-phase approach: extract to staging, rename current destRoot to a
   `.backup-*` sibling, rename staging to destRoot, remove backup. This
   requires careful error handling to avoid leaving the backup orphaned.

3. **Cross-filesystem fetch** — `os.Rename` is only atomic within a single
   filesystem. If `parent(destRoot)` and the temp-dir root are on different
   filesystems (e.g., bind mounts in containers), `MkdirTemp` must be called
   in the same mount as `destRoot`. Current code already passes
   `filepath.Dir(destRoot)` as the temp parent, so this is satisfied as long
   as callers do not place `destRoot` on a separate mount from its parent.

4. **Short-term policy** — until items 1–3 are addressed, the recommended
   caller contract is: always pass `RequireEmptyDestination=true` and let
   `destRoot` be a path that does not yet exist. This gives the strongest
   atomicity guarantee with the current implementation.

---

## Fetch safety policy

The two fetch modes differ significantly in safety guarantees:

**Safe fetch mode** (`RequireEmptyDestination=true`, recommended for all new code):
- `destRoot` must not exist at all before fetch begins.
- All extraction goes to a sibling staging directory.
- On success: staging is committed to `destRoot` via `os.Rename` (atomic within filesystem).
- On failure: staging directory is removed; `destRoot` is never touched.
- Result: `destRoot` is either absent (failure) or fully populated (success).

**Legacy fetch mode** (`RequireEmptyDestination=false`):
- Extracts directly into `destRoot`.
- A mid-fetch failure leaves `destRoot` in a partial state.
- Intended only for compatibility with existing callers or controlled local
  workflows where partial state is acceptable.

`FetchVolumeSequential` and `FetchVolumeParallel` default to
`RequireEmptyDestination=false` for backward compatibility. New code should
call `Client.FetchVolume(..., FetchOptions{RequireEmptyDestination: true})`
directly, or use a dedicated safe-fetch helper once one is added:

```go
// Proposed future helper — not yet implemented.
func (c *Client) FetchVolumeFresh(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error)
```
