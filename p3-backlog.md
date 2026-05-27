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
- Replace `TarGzDir` with a streaming variant that writes directly to an
  `io.Writer` (pipe or temp file).
- Thread the writer through `pushIfNeeded` so ORAS receives an `io.Reader`
  rather than a fully-materialized `[]byte`.
- Consider content-addressable chunked push (ORAS streaming copy) to avoid
  re-uploading unchanged layers.

---

## P3-2: Remote registry fetch API

**Current behaviour**  
`FetchVolSeq` and `FetchVolParallel` open a **local** OCI store (`oci.New`)
and extract from it. There is no code path that pulls layers directly from a
remote registry (e.g., Harbor, GHCR) into a local directory.

**Accepted trade-off**  
The current two-step workflow (remote pull → local OCI store → extract) is
functional for existing callers.

**Intended design when picked up**  
- Add `Client.FetchVolumeFromRemote(ctx, destRoot, remoteRepo, tag string,
  target RemoteTarget, opts FetchOptions)`.
- Internally open a `remote.Repository` via `registryutil.NewRepository`,
  resolve the manifest, and stream each layer directly to the staging
  directory (integrating with the staging design below).
- Reuse the existing `fetchVolWithStaging` commit/cleanup logic.

---

## P3-3: Staging directory design for tar extraction (fetch integrity)

**Problem**  
`FetchVolSeq` and `FetchVolParallel` extract layers directly into `destRoot`.
If any layer download or unpack fails midway, `destRoot` is left in a partial
state that is indistinguishable from a complete extraction. Individual
`writeFileAtomic` calls cannot prevent this because they operate per-file, not
per-directory.

**Current mitigation**  
`RequireEmptyDestination=true` + `fetchVolWithStaging`:

```
ensureEmptyDir(destRoot)           // fail fast if non-empty
stagingDir = MkdirTemp(parent, …)  // sibling of destRoot, same filesystem
FetchVolSeq/Parallel → stagingDir  // all extraction to staging
os.Rename(stagingDir, destRoot)    // atomic directory swap on success
os.RemoveAll(stagingDir)           // deferred cleanup on failure
```

This guarantees `destRoot` is either untouched or fully populated when
`RequireEmptyDestination=true`. Legacy callers that pass `false` still get
direct extraction.

**Known gaps / next steps**

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
