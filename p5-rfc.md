# P5 RFC — Chunked CAS for Large Dataset Artifacts

**Status**: Final (v6 — OCI config descriptor terminology, split schemaVersion/artifactFormat, GC retry policy)
**Target**: sori v0.6 (experimental), v0.7+ (stable)
**Problem driver**: Large genomics reference datasets (STAR index ~40 GB, hg38 reference ~60 GB+)

---

## 1. Problem

The current publish path:

```
directory
  ↓ TarGzDirTo
tar.gz stream → temp file (full layer size on disk)
  ↓ seek(0)
ORAS blob push
  ↓
OCI manifest push
```

This creates two distinct problems for large genomics datasets.

### 1-A. Disk spike

A 40 GB STAR index requires ~40 GB of temp space during push.  For a server
where `/tmp` shares a volume with the data, this doubles effective disk
requirement.

P3-1 Step 2 (spooling/disk-safety) cannot eliminate this.  ORAS
`Store.Push` requires `ocispec.Descriptor.Digest` and `.Size` before reading
the stream.  A tar.gz stream cannot report its final size until the last byte
is written, so spooling to temp storage is structurally required with the
current artifact format.

### 1-B. Failure restart cost

A 30 GB push that fails at 28 GB restarts from zero.  For genomics reference
sets that change infrequently but are large, this is operationally expensive.

---

## 2. Non-goals (V1)

| Excluded | Reason |
|---|---|
| Content-defined chunking (CDC / Rabin fingerprint) | Rolling hash, boundary algorithm, determinism, cross-platform reproducibility — too complex for V1. Future RFC. |
| Migration of existing artifacts | Dual-path fetch handles it; no migration needed. |
| Transparent chunk compression | V1 chunks are raw file byte ranges. Optimising resumability and partial dedup, not compression ratio. See D-9. |
| Small-file packing | Complicates reassembly; guarded by MaxChunkedLayers instead. See D-11. |
| Fetch resume across interrupted fetches | Conflicts with the existing staging policy (staging is removed on failure). Deferred to V1.1. See D-6. |
| Symlinks and special files | V1 handles regular files only. See D-10. |
| CLI surface changes | sori is a library; CLI flags are the caller's concern. |
| Registry GC automation | Chunk blob lifecycle management is a registry operator concern; sori emits no lifecycle annotations and does not trigger GC. |
| Patient / sample sensitive data (HIPAA / GDPR scope) | sori is designed for public reference genomes only. Encrypting patient-derived or personally-identifiable genomic data requires an adapter layer with encryption-at-rest and access-control guarantees outside sori's scope. |

---

## 3. Decisions

### D-1. Chunk size

| Parameter | Value |
|---|---|
| Default | 1 GiB (1 073 741 824 bytes) |
| Configurable range | 256 MiB – 2 GiB |

Rationale: 60 GB file → ~60 chunks; 200 GB reference set → ~200 chunks.
Manifest size stays manageable, restart granularity is sufficient.
512 MiB doubles chunk count with little benefit at this stage; 2 GiB
increases restart cost.

### D-2. Chunking strategy

**File-aware fixed-size chunking.**

1. Walk source directory recursively; collect regular files sorted by path.
2. Divide each file into fixed-size chunks; the final chunk of a file may be
   smaller than the chunk size.
3. Compute `sha256` of each chunk independently via a two-pass section reader
   (see Push flow, §4).
4. Small files (< chunk size) become a single chunk each.
5. Small-file packing is **not** in V1 (see D-11 for the guardrail instead).

### D-3. Manifest structure

```
OCI Image Manifest
  config:   application/vnd.sori.chunked-cas.config.v1+json
  layers:
    [0]  application/vnd.sori.chunk-index.v1+json          (chunk-index.json)
    [1]  application/vnd.sori.dataset.metadata.v1+json     (dataset-metadata.json, if provided)
    [2]  application/vnd.sori.configblob.v1+json           (original configblob.json, if present)
    [3]  application/vnd.sori.chunk.v1                     (chunk 0)
    [4]  application/vnd.sori.chunk.v1                     (chunk 1)
    ...
```

All blobs appear as layers for OCI GC safety.  `chunk-index.json` is the
primary metadata contract.  `configblob.json` is stored as a dedicated layer
so the original caller-supplied config is preserved independently of format
metadata (see D-7).

The OCI `config` descriptor (`manifest.Config`) is distinct from the entries
in `manifest.Layers`.  Its **mediaType is the primary format detection signal**
used by dual-path fetch (D-13):

```
manifest.Config.MediaType == "application/vnd.oci.image.config.v1+json"
    → legacy tar.gz path

manifest.Config.MediaType == "application/vnd.sori.chunked-cas.config.v1+json"
    → chunked CAS path
```

The config descriptor blob contains a small JSON document for secondary
validation:

```json
{
  "schemaVersion": "sori.chunked-cas.config.v1",
  "artifactFormat": "sori.chunked-cas.v1"
}
```

`schemaVersion` identifies the schema of this config document itself.
`artifactFormat` identifies the artifact layout version.  Separating the two
allows the config schema to evolve (e.g. add `producerVersion`, `features`)
independently of the artifact format version.  The config blob must be pushed
before the OCI manifest (see §7-5).

Layer ordering mirrors chunk-index.json order for human readability.
**Fetch correctness must rely on descriptors (digest + mediaType), not
positional index.**  A fetcher must locate chunk-index.json by its mediaType,
not by assuming it is layers[0].

### D-4. chunk-index.json schema

```json
{
  "schemaVersion": "sori.chunked-cas.v1",
  "chunkSize": 1073741824,
  "files": [
    {
      "path": "hg38.fa",
      "mode": 420,
      "size": 64424509440,
      "digest": "sha256:<whole-file-sha256>",
      "chunks": [
        { "offset": 0,          "size": 1073741824, "digest": "sha256:..." },
        { "offset": 1073741824, "size": 1073741824, "digest": "sha256:..." },
        { "offset": 2147483648, "size": 1073741824, "digest": "sha256:..." }
      ]
    },
    {
      "path": "hg38.fa.fai",
      "mode": 420,
      "size": 7168,
      "digest": "sha256:...",
      "chunks": [
        { "offset": 0, "size": 7168, "digest": "sha256:..." }
      ]
    }
  ]
}
```

Field notes:
- `schemaVersion`: fixed string `"sori.chunked-cas.v1"` for format detection.
- `chunkSize`: the nominal chunk size used for this artifact.  Actual final
  chunk of each file may be smaller.
- `path`: clean relative path from the artifact root.  Validation rules in D-10.
- `mode`: Unix file permission bits as a decimal integer (e.g. `420` = `0o644`).
- `digest` (file level): sha256 of the full unmodified file; used for
  whole-file integrity verification after reassembly.
- `offset`: byte offset within the file where this chunk begins.
- `digest` (chunk level): sha256 of the raw chunk bytes.

**No `artifactDigest` field.**  chunk-index.json identity is established by
the OCI descriptor digest of the blob itself (the sha256 stored in the
manifest layer entry).  A self-referential field that must equal the sha256 of
the document containing it is structurally awkward to compute and verify; the
OCI descriptor is sufficient.

### D-5. Push resume (chunk-level deduplication)

Before pushing each chunk, check whether the blob already exists in the
registry using the chunk digest.  If it does, skip the push.  This leverages
OCI content-addressable storage — identical chunks across versions or datasets
are automatically shared without re-uploading.

Implementation follows the `pushIfNeeded` pattern from `publishVolumeToStore`.

### D-6. Fetch resume

Fetch resume (skipping already-downloaded chunks across interrupted fetches)
is **deferred to V1.1**.

Reason: the existing staging policy destroys the staging directory on failure
(`defer os.RemoveAll(stagingDir)`).  A progress file alongside the staging
directory would be removed with it.  Resumable staging requires a distinct
design — either a persistent staging cache outside the normal staging path, or
a modified failure policy.  This is a separate design problem and should not
block V1.

V1 fetch: on any error, staging is removed and the fetch must restart in full.
This is consistent with the existing safe-fetch and atomic-overwrite policies.

### D-7. configblob.json compatibility

The legacy publish path stores the caller-supplied `configblob.json` as the
OCI config descriptor (mediaType `application/vnd.oci.image.config.v1+json`),
and `restoreConfigBlob` reconstructs it from the config blob on fetch.

In chunked CAS the config descriptor carries format metadata
(`application/vnd.sori.chunked-cas.config.v1+json`), not the original config.
The original `configblob.json` must be stored separately.

**Decision**: if a configblob is provided, store it as a dedicated layer with
mediaType `application/vnd.sori.configblob.v1+json`.  Fetchers locate it by
mediaType, not by layer position.  The chunked fetch path reconstructs
`configblob.json` from this layer, mirroring `restoreConfigBlob` in the legacy
path.  If no configblob was provided, this layer is omitted.

### D-8. Experimental gate and API placement

Chunked CAS is an **artifact format** choice, not a remote push option.  It
belongs on the packaging side, not on `PushOptions`.

Current relevant types in `options.go`:
```go
type PackageOptions struct {
    ConfigBlob        []byte
    RequireConfigBlob bool
}
```

Proposed direction — add an `ArtifactFormat` type:

```go
// ArtifactFormat selects the on-disk and OCI artifact layout used during
// packaging.  The default (zero value) is ArtifactFormatLegacy.
type ArtifactFormat int

const (
    // ArtifactFormatLegacy packages each partition as a gzip-compressed tar
    // layer.  This is the original sori format and is always supported on fetch.
    ArtifactFormatLegacy ArtifactFormat = iota

    // ArtifactFormatChunkedCAS packages files as fixed-size raw chunks with a
    // chunk-index.json manifest.  Experimental: requires a client with chunked
    // CAS fetch support.
    ArtifactFormatChunkedCAS
)

type PackageOptions struct {
    ConfigBlob        []byte
    RequireConfigBlob bool
    // Format selects the artifact layout.  Defaults to ArtifactFormatLegacy.
    // ArtifactFormatChunkedCAS is experimental.
    Format ArtifactFormat
    // DatasetMetadata is the serialised dataset-metadata.json to include as a
    // dedicated OCI layer (mediaType vnd.sori.dataset.metadata.v1+json).
    // Optional: fetch works without it; catalog exposure is degraded without it.
    DatasetMetadata []byte
}
```

The exact name and location are an open question (OQ-2); the above is the
recommended starting point.

### D-9. Chunk content: raw bytes, no compression

V1 chunks are raw file byte ranges.  No transparent compression is applied.

Rationale: genomics reference files (FASTA, binary index files) are typically
already incompressible or at low compression ratios.  Adding compression
complicates chunk digest verification and increases CPU cost without meaningful
space savings.  Compression is a future RFC item.

### D-10. Path validation and file type policy

All paths in `chunk-index.json` must pass validation before use:

**Reject with ErrValidation:**
- Absolute paths (starts with `/`)
- Paths containing `..` components
- Empty path segments (`//` or trailing `/`)
- Duplicate paths within the same artifact
- Paths referencing anything other than regular files

**V1 file type policy:**
- Regular files: supported.
- Symlinks: **rejected** with ErrValidation during push.  Symlink support is a
  future design decision (schema change required to store link targets).
- Empty directories: **not preserved**.  Directory structure is implicit from
  file paths; a directory that contains no files is lost.  This is consistent
  with the legacy tar path behaviour for empty directories within partitions.
- Device files, pipes, sockets: rejected with ErrValidation.

### D-11. MaxChunkedLayers guardrail

Small-file packing is out of scope for V1.  To prevent unbounded manifest
growth when a dataset contains many small files, a hard limit applies:

```
MaxChunkedLayers = 900
```

The effective chunk layer budget is computed dynamically at push time based on
which optional blobs are present:

```
metadataLayerCount := 1  // chunk-index.json (always required)
if opts.DatasetMetadata != nil { metadataLayerCount++ }
if opts.ConfigBlob != nil      { metadataLayerCount++ }
maxChunkLayers := MaxChunkedLayers - metadataLayerCount
```

If the computed chunk count would exceed `maxChunkLayers`, the publish call
returns `ErrValidation` before any blob is pushed.

Worst case (both optional layers present): 900 − 3 = **897 chunk layers**.
Minimum case (neither optional layer): 900 − 1 = **899 chunk layers**.

Rationale: ECR documented a 1000-layer limit; 900 provides headroom.  Once
OQ-1 is resolved this constant may be raised.  Small-file packing (future RFC)
would allow datasets with many small files to fit within the limit.

### D-12. VolumeIndex and VolumeRef semantics under chunked CAS

- `VolumeIndex.VolumeRef`: still the sha256 digest of the top-level OCI
  manifest.  Meaning is unchanged.
- `VolumeIndex.Partitions`: the legacy path populates Partitions from tar
  layers (one entry per partition).  The chunked CAS path has no partitions
  concept; `Partitions` will be empty.  chunk-index.json is the source of
  truth for file-level structure.
- `ArtifactFormat` field in `VolumeIndex`: whether to add a field to record
  the format used is an open question (OQ-5).

### D-13. Dual-path fetch (backward compatibility)

Format detection on fetch:

```
manifest.Config.MediaType == "application/vnd.oci.image.config.v1+json"
    → legacy tar.gz path (fetchVolSeqFrom / fetchVolParallelFrom)

manifest.Config.MediaType == "application/vnd.sori.chunked-cas.config.v1+json"
    → chunked CAS fetch path (new)
```

Both paths are maintained indefinitely until legacy is formally deprecated
(separate RFC).

**Client compatibility note**: only clients that include the chunked CAS fetch
code can fetch chunked artifacts.  A client built before this feature exists
cannot auto-detect the format; it will fail on an unknown config mediaType.
"Any client" auto-detection applies only within the new-client population.

---

## 4. Push flow (V1)

```
ChunkedPublish(ctx, srcDir, volName, opts):

  1. Walk srcDir → sorted regular-file list
     reject symlinks / special files with ErrValidation
  2. Compute metadataLayerCount (1 + present optional layers)
     estimatedChunks = sum over each file of ceil(file.size / chunkSize)
     if estimatedChunks > MaxChunkedLayers - metadataLayerCount → ErrValidation
  3. For each file, for each chunk range [offset, offset+chunkSize):
       -- Pass 1: hash (small buffer, no heap allocation for full chunk) --
       f = os.Open(file)
       sr = io.NewSectionReader(f, offset, chunkSize)
       h = sha256.New()
       io.Copy(h, sr)          // reads with stdlib default 32 KiB buffer
       chunkDigest = sha256:hex(h.Sum(nil))

       -- registry dedup check --
       if registry.Exists(chunkDigest): record in chunk-index, continue

       -- Pass 2: push (SectionReader seeks back to offset 0) --
       sr.Seek(0, io.SeekStart)
       registry.Push(chunkDescriptor{digest, size}, sr)
       append chunk entry to in-progress chunk-index
  4. Serialise chunk-index.json → push as blob
  5. If dataset-metadata provided: push as blob (mediaType vnd.sori.dataset.metadata.v1+json)
  6. If configblob provided: push as blob (mediaType vnd.sori.configblob.v1+json)
  7. Push OCI config descriptor blob:
       {"schemaVersion": "sori.chunked-cas.config.v1", "artifactFormat": "sori.chunked-cas.v1"}
  8. Build OCI manifest (config + chunk-index + optional dataset-metadata + optional configblob + all chunks)
  9. Push manifest → assign tag
```

**Peak memory**: hash buffer (~32 KiB stdlib default) plus chunk-index.json
in memory (tens of KB for typical datasets).  No per-chunk 1 GiB buffer.
**Peak disk**: sori does not create a full-artifact temporary file during push.
Chunks are read directly from source files via `io.SectionReader` and pushed
in-flight.  This eliminates the sori-controlled full-layer disk spike (1-A).
OS page cache and registry-side buffering are outside sori's control.

---

## 5. Fetch flow (V1)

```
ChunkedFetch(ctx, destRoot, src, tag, concurrency):

  1. Resolve tag → manifest descriptor
  2. Fetch manifest → detect format via Config.MediaType (D-13)
     → legacy: delegate to existing fetch path
     → chunked: continue
  3. Locate chunk-index layer by mediaType vnd.sori.chunk-index.v1+json
  4. Fetch and parse chunk-index.json
  5. Validate all paths (D-10)
  6. For each file in chunk-index (parallel, worker pool):
       pre-allocate destination file to full size
       for each chunk:
         fetch chunk blob by digest
         write at correct offset
         verify sha256 == chunk entry digest → ErrIntegrity on mismatch
  7. For each file: verify whole-file digest == chunk-index file.digest
  8. Locate dataset-metadata layer by mediaType vnd.sori.dataset.metadata.v1+json (if present)
     fetch and write destRoot/.sori/dataset-metadata.json
     (reserved path; avoids collision with a source file named dataset-metadata.json)
  9. Locate configblob layer by mediaType vnd.sori.configblob.v1+json (if present)
     fetch and write configblob.json under destRoot (legacy path; backward compatible)
 10. Write volume-index.json under destRoot (legacy path; backward compatible)
```

No fetch resume in V1: on any error, the staging directory is removed and the
caller must retry from scratch (consistent with existing staging policy).

---

## 6. Test plan

### Synthetic fixtures

| Fixture | Purpose |
|---|---|
| `small.txt` (1 KB) | Single-chunk small file |
| `medium.bin` (10 MB) | Single chunk, below chunk size |
| `large.bin` (3 × chunkSize + 500 MiB) | Multi-chunk, partial final chunk |
| `changed-large` | `large.bin` with first 100 bytes modified |
| Mixed directory | Both small and large files together |

### V1 required test cases

1. **First push**: all chunks uploaded, manifest created.
2. **Identical second push**: all chunks skipped (registry dedup), manifest re-tagged.
3. **Partial change push**: only modified chunks uploaded; unmodified chunks skipped.
4. **Full fetch**: reassembled directory byte-identical to source (file content + mode).
5. **Chunk integrity failure**: fetch returns `ErrIntegrity` on digest mismatch.
6. **Path validation**: absolute path / `..` / duplicate path / symlink source → `ErrValidation`.
7. **MaxChunkedLayers**: dataset generating > 900 chunks → `ErrValidation`.
8. **Legacy fetch**: existing tar.gz artifact still fetches correctly (dual-path).
9. **configblob round-trip**: configblob provided on push is restored on fetch.

### V1.1 test cases (deferred with fetch resume)

10. **Interrupted fetch + resume**: partial destination kept; already-fetched chunks verified and skipped on retry.

### Genomics-profile fixtures (after synthetic tests pass)

- BWA index profile: several files, 2–5 GB each.
- STAR index profile: ~40 GB total, 8–12 large binary files.
- hg38.fa profile: single ~60 GB file (synthetic, does not need real biological content).
- Metadata profile: `.dict`, `.fai`, JSON alongside large binaries.

---

## 7. Production Readiness / Operational Requirements

P5 V1 is not shippable until every subsection in this chapter is satisfied.
Functional tests alone are insufficient.

---

### §7-1. Benchmark / Resource Gate

Every benchmark run records all twelve metrics against all seven fixture
datasets.  A build that violates any pass criterion is a blocking failure.

#### Metrics

| # | Metric | Unit |
|---|---|---|
| M-01 | First push wall-clock time | seconds |
| M-02 | Second push wall-clock time (all chunks skipped) | seconds |
| M-03 | Partial update push time (changed chunks only) | seconds |
| M-04 | Failed push retry time (skip already uploaded chunks) | seconds |
| M-05 | Peak RSS memory during push | MiB |
| M-06 | Peak temporary disk usage during push | MiB |
| M-07 | Total bytes read from source during push | bytes |
| M-08 | Total bytes uploaded to registry during push | bytes |
| M-09 | Number of blobs/layers created in manifest | count |
| M-10 | Fetch wall-clock time | seconds |
| M-11 | Peak disk usage during fetch | MiB |
| M-12 | Reconstructed tree digest verification time | milliseconds |

#### Fixture datasets

| Fixture | Description |
|---|---|
| `synthetic-1GiB` | Single file, 1 GiB of pseudorandom bytes |
| `synthetic-10GiB` | 5 files × 2 GiB each |
| `synthetic-50GiB` | 10 files × 5 GiB each |
| `genomics-fasta` | Single large FASTA-like file (~3 GiB, compressible line structure) |
| `genomics-bwa` | BWA index profile: 5 files, 2–5 GiB each, binary |
| `genomics-star` | STAR index profile: 8–12 binary files, ~40 GiB total |
| `genomics-mixed` | Large binaries + small metadata (`.dict`, `.fai`, JSON) |

Genomics fixtures are generated synthetically; no real biological data is
required.  Content should reflect realistic compressibility profiles (FASTA:
low entropy; binary index: near-incompressible).

#### Pass criteria

| Criterion | Rule |
|---|---|
| **No full-artifact temp file** | A temporary file equal to or larger than the full artifact size must never be created during push (M-06 must remain `O(metadata + small buffers)`, not `O(artifact size)`) |
| **RSS does not scale with chunk size** | Push peak RSS (M-05) must be independent of configured chunk size |
| **RSS ceiling** | Push peak RSS (M-05) must stay below 256 MiB at upload concurrency = 2 for all fixtures |
| **Second push uploads zero chunks** | M-08 for an unchanged artifact must equal zero chunk bytes (only manifest + metadata re-pushed) |
| **Retry skips uploaded chunks** | After a failed mid-push, retry must upload only chunks not yet present in registry (verified via M-08 and M-09 comparison) |
| **MaxChunkedLayers fires pre-push** | A dataset that exceeds MaxChunkedLayers must return ErrValidation before any blob is pushed to the registry (M-09 = 0 on failure) |
| **Reconstructed tree matches source** | Byte-for-byte digest of every file and the whole tree must match source after fetch |
| **Benchmark results persisted** | Each gate run saves a result artifact at `docs/bench/YYYY-MM-DD-<fixture>.json` |

#### Result format

```json
{
  "date": "2026-05-30",
  "soriVersion": "v0.6.0-dev",
  "fixture": "synthetic-10GiB",
  "chunkSizeBytes": 1073741824,
  "uploadConcurrency": 2,
  "metrics": {
    "firstPushSeconds":         42.1,
    "secondPushSeconds":         1.3,
    "partialUpdatePushSeconds": 12.7,
    "retryPushSeconds":         14.2,
    "pushPeakRSSMiB":           48.3,
    "pushPeakTempDiskMiB":       0.1,
    "sourceBytesRead":    10737418240,
    "uploadedBytes":      10737418240,
    "blobsCreated":              10,
    "fetchSeconds":             38.6,
    "fetchPeakDiskMiB":       9960.0,
    "treeVerifyMs":             210
  },
  "passed": true,
  "violations": []
}
```

`violations` lists the criterion IDs that failed, if any.

---

### §7-2. Registry Compatibility Matrix

Chunked CAS must be verified against the following registries before V1
release.

| Registry | Required for V1 | Notes |
|---|---|---|
| **Harbor** | Yes — V1 blocker | Primary target; must pass full push + fetch + dedup round-trip |
| **GHCR** (GitHub Container Registry) | Yes — V1 blocker | Must pass full push + fetch + dedup round-trip |
| **ECR** (Amazon Elastic Container Registry) | High priority | ECR's 1000-layer cap informs MaxChunkedLayers=900; verify empirically |
| **Local OCI layout** (`oci.New`) | Yes — V1 blocker | Used in all unit tests; must support Exists check for dedup |
| **Zot** | Nice-to-have | Open-source reference implementation; test if available |

For each required registry, the compatibility test covers:

1. Push a chunked artifact → verify manifest and all blobs reachable.
2. Second push (identical) → verify zero bytes uploaded (dedup).
3. Fetch → verify tree digest matches source.
4. Tag overwrite (re-push after partial change) → verify old chunks still
   present, only changed chunks uploaded.

Registry tests are integration tests, gated behind a build tag
(`//go:build integration`).  They require credentials and a live registry
endpoint; they are not run in CI by default.

---

### §7-3. Retry / Cancel / Timeout Policy

| Condition | Behaviour |
|---|---|
| **Context cancellation** (`ctx.Err() != nil`) | Immediate stop; return `ctx.Err()` unwrapped.  No retry. |
| **5xx server error** | Bounded exponential backoff: 3 attempts, initial delay 500 ms, max delay 8 s, jitter ±10%.  After 3 failures return `ErrTransport`. |
| **401 / 403 auth error** | Hard fail immediately; return `ErrAuth`.  Retrying with the same credentials cannot succeed. |
| **Digest mismatch on fetch** | Hard fail immediately; return `ErrIntegrity`.  A corrupted chunk must not be silently retried or skipped. |
| **Network timeout** | Treat as transient; subject to 5xx backoff policy. |
| **404 on chunk Exists check** | Expected — treat as "not uploaded"; proceed with push. |

Retry state is per-chunk, not per-artifact.  A retry of a full publish call
restarts the walk from chunk 0 but skips chunks already confirmed present
(D-5 dedup).  There is no global retry loop wrapping the entire artifact push.

---

### §7-4. Integrity Verification Policy

#### Pre-push (publish side)

- Compute chunk digest via Pass 1 `io.SectionReader` hash (§4).
- After `registry.Push`, perform an optional existence check (`Exists(digest)`)
  to confirm the registry acknowledged the blob.  This is a best-effort safety
  net; full round-trip verification is the caller's responsibility.

#### Post-fetch (fetch side)

Three verification layers, applied in order:

1. **Chunk-level**: after writing each chunk to its destination offset, verify
   sha256 of written bytes == `chunk.digest` in chunk-index.json.
   → `ErrIntegrity` on mismatch; fetch stops immediately.
2. **File-level**: after all chunks for a file are written, verify sha256 of
   the whole assembled file == `file.digest` in chunk-index.json.
   → `ErrIntegrity` on mismatch.
3. **Tree-level** (M-12): after all files are written, re-walk the destination
   tree and verify each file digest matches.  Measures reassembly correctness
   end-to-end.

#### Partial output on failure

On any integrity failure during fetch, the staging directory is removed
(consistent with existing staging policy).  No partial output is left in
`destRoot`.

---

### §7-5. Atomic Publish Semantics

The publish sequence must ensure that no partially-complete chunked artifact
is ever observable at a named tag.

**Required order:**

1. Push all chunk blobs (content-addressed; safe to push in any order).
2. Push chunk-index.json blob.
3. Push dataset-metadata blob (if present).
4. Push original configblob blob as OCI layer (if present).
5. Push OCI config descriptor blob:
   `{"schemaVersion": "sori.chunked-cas.config.v1", "artifactFormat": "sori.chunked-cas.v1"}`.
6. Push OCI manifest → assign tag.

**Invariant**: a tag is only created or updated at step 6.  Before that point,
individual blobs are present in the registry CAS but not reachable from any
manifest.  If the push fails at any step before step 6, no tag points to an
incomplete manifest.

**GC window**: between steps 1–5 and step 6, pushed blobs are unreferenced and
theoretically eligible for registry GC.  In practice this window is seconds,
and most registries apply a grace period before collecting unreferenced blobs.
If the manifest push (step 6) fails because a referenced blob was reclaimed by
GC, the client must treat it as a transient publish failure and retry from the
dedup-aware chunk push flow (steps 1–5 will skip blobs already present).

**No rollback mechanism**: sori does not delete blobs on push failure.
Unreferenced blobs left by a failed push are GC'd by the registry operator.
This is consistent with standard OCI client behaviour.

---

### §7-6. Observability / Progress

Long-running operations on large datasets must not be silent.  V1 must emit
progress information at a granularity that makes 40 GB pushes operationally
observable.

#### Required progress events

| Event | Trigger | Fields |
|---|---|---|
| `ChunkSkipped` | Exists check returned true | `file`, `chunkIndex`, `digest` |
| `ChunkUploaded` | Push completed for a chunk | `file`, `chunkIndex`, `digest`, `bytes`, `durationMs` |
| `ChunkFetched` | Chunk written to destination | `file`, `chunkIndex`, `digest`, `bytes` |
| `FileDone` | All chunks of a file written and verified | `file`, `totalBytes` |
| `ArtifactDone` | Manifest pushed / fetch complete | `totalBytes`, `durationMs`, `chunksUploaded`, `chunksSkipped` |

#### Progress interface

The progress callback is injected via options, not logged globally.  Callers
that do not need progress pass `nil`.  Example:

```go
type ChunkProgress struct {
    Event      string // "ChunkSkipped" | "ChunkUploaded" | "ChunkFetched" | ...
    File       string
    ChunkIndex int
    Bytes      int64
    DurationMs int64
    Digest     string
}

type ProgressFunc func(ChunkProgress)
```

`PackageOptions` and the fetch options struct gain an optional `Progress
ProgressFunc` field.

#### No silent long-running operations

A caller that pushes a 60 GB artifact with `Progress: nil` should still see
regular `Log.Infof` output at chunk boundaries so that the operation is not
completely silent in server logs.

---

### §7-7. Concurrency / Backpressure

#### Upload concurrency (push)

| Parameter | V1 default | Notes |
|---|---|---|
| `uploadConcurrency` | 2 | Matches existing `PublishOptions.Concurrency` convention |
| Inflight read buffer | 8–16 MiB per worker | Applied at the `io.SectionReader` read loop; not a per-chunk heap allocation |
| Max inflight bytes | `uploadConcurrency × chunkSize` (≤ 2 GiB) | Not a hard limit in V1; informational for capacity planning |

With concurrency=2 and 1 GiB chunks, peak inflight data is 2 × 32 KiB hash
buffers (Pass 1) or 2 × ORAS internal buffers (Pass 2).  No 1 GiB buffers are
allocated.

#### Fetch concurrency

| Parameter | V1 default | Notes |
|---|---|---|
| `fetchConcurrency` | 4 | Higher than upload; fetch is typically read-bound, not write-bound |
| Worker pool | Fixed goroutine pool, bounded by `fetchConcurrency` | Prevents goroutine explosion on datasets with many small files |
| File pre-allocation | `os.File.Truncate(fileSize)` before first chunk write | Eliminates fragmentation and detects disk-full early |

#### Backpressure

Worker pool uses a channel-based semaphore.  If all workers are busy,
submission blocks the walk goroutine rather than spawning unbounded goroutines.
No external rate limiter in V1.

---

### §7-8. Data Scope / Security

**In scope (V1)**: publicly available genomics reference data.  Examples:
- hg38 / hg19 human reference genome (FASTA + index files)
- GRCh38 annotation GTF
- STAR genome index (pre-built from public reference)
- BWA / Bowtie2 index (pre-built from public reference)

**Out of scope**: patient-derived or sample-level genomic data.  sori V1
provides no encryption at rest, no access-control enforcement, and no audit
logging.  Storing HIPAA-covered or GDPR-covered data via sori without an
appropriate encryption + access-control adapter layer is prohibited.

This constraint is enforced by documentation and user agreement, not by code.
sori has no mechanism to detect whether a file contains personal genomic data.

**Credential handling**: registry credentials flow through `AuthConfig` with
`${ENV_VAR}` substitution (P4-1).  Credentials are never written to
`chunk-index.json` or any sori metadata artifact.

---

### §7-9. Format Version / Capability Policy

#### schemaVersion enforcement

On fetch, if `chunk-index.json.schemaVersion` is not `"sori.chunked-cas.v1"`,
the fetch must return `ErrValidation` immediately without downloading any chunk
blobs.

This is a hard fail, not a best-effort fallback.  A client that silently
ignores an unknown schemaVersion risks data corruption if the schema changes
in a future version.

#### Config mediaType enforcement

On fetch, if `manifest.Config.MediaType` is `"application/vnd.sori.chunked-cas.config.v1+json"`
but no chunk-index layer is found by mediaType, the fetch must return
`ErrValidation`.  An incomplete manifest is not silently treated as a legacy
artifact.

#### Forward compatibility

A sori client encountering a `schemaVersion` it does not recognise must return
`ErrValidation` with a message indicating the unknown version string.  It must
not attempt to interpret unknown fields.

Unknown `schemaVersion` values are reserved for future RFC iterations.

---

### §7-10. Dataset Preflight / Suitability

Before starting a chunked CAS push, the implementation must perform a
preflight check and report whether the dataset is suitable.

#### Preflight steps

1. **Walk and count**: walk `srcDir`, count files and total bytes.
2. **Chunk estimate**: compute `estimatedChunks = sum over each regular file of ceil(file.size / chunkSize)`.
   File boundaries are never shared between chunks; each file contributes at least one chunk regardless of size.
3. **MaxChunkedLayers check**: if `estimatedChunks > MaxChunkedLayers - metadataLayerCount`,
   return `ErrValidation` before any blobs are pushed (see D-11).
4. **Suitability recommendation**: if `totalBytes < 1 GiB`, emit a log warning
   recommending `ArtifactFormatLegacy` instead.  Chunked CAS overhead (chunk-index,
   manifest complexity, dedup check round-trips) is not justified for small datasets.
5. **Symlink / special file check**: report all rejectable files before pushing
   any blobs (fail fast; see D-10).

#### Suitability thresholds

| Total dataset size | Recommendation |
|---|---|
| < 1 GiB | Use `ArtifactFormatLegacy`; chunked CAS overhead exceeds benefit |
| 1 GiB – 10 GiB | Chunked CAS acceptable; legacy also fine |
| > 10 GiB | Chunked CAS recommended; legacy disk spike and restart cost are significant |

These are recommendations emitted as log warnings, not hard validation errors.

---

## 8. Open questions (resolve before implementation)

| # | Question | Impact |
|---|---|---|
| OQ-1 | Do Harbor / Zot / ECR / GHCR cap layer count, and at what value? | May require lowering MaxChunkedLayers or prioritising small-file packing |
| OQ-2 | Is `PackageOptions.Format ArtifactFormat` the right API shape, or a separate `ChunkedPackageOptions`? | Public API surface; hard to change after stable |
| OQ-3 | Should symlinks be supported in V1.1, and if so, how are link targets stored in chunk-index.json? | Schema extension |
| OQ-4 | Should `VolumeIndex` gain an `ArtifactFormat` field to let callers distinguish format without re-fetching the manifest? | Typing convenience; backward compatibility if added later |
| OQ-5 | What is the correct `chunkSize` for artifacts that mix very large and very small files? Should chunk size be per-file rather than per-artifact? | Schema change if per-file |

---

## 9. V1.1 and future RFC items

| Item | Why deferred |
|---|---|
| **Fetch resume** | Conflicts with current staging policy; needs separate staging-cache design |
| **Symlink support** | Schema change required; V1 rejects symlinks explicitly |
| **Small-file packing** | Complicates reassembly; revisit after OQ-1 is resolved |
| **CDC chunking** | High complexity (rolling hash, determinism, cross-platform tests); separate RFC |
| **Chunk-level compression** | Low value for genomics files; separate RFC |
| **Remote-to-remote copy** | Requires registry-side blob mount; separate RFC |
| **Encryption at rest** | Out of scope for sori core; adapter-layer concern |

---

## 10. Dataset Metadata / Catalog Exposure

P5 V1 must ship with a stable metadata contract so that data artifacts are
discoverable by catalog services and pipeline editors — not just fetchable by
the sori runtime.

---

### §10-1. Design principle: separation of concerns

`chunk-index.json` and `dataset-metadata.json` serve fundamentally different
audiences and must not be conflated.

| Layer | Audience | Purpose |
|---|---|---|
| `chunk-index.json` | sori runtime | Storage reconstruction: file paths, chunk offsets, digests, sizes |
| `dataset-metadata.json` | Catalog / pipeline editor / human operator | Dataset identity, domain context, tool compatibility |

**`chunk-index.json` must never contain** organism names, reference build
names, display names, tool compatibility flags, or any domain/UI concern.  It
is a low-level integrity contract between the push implementation and the fetch
implementation.

**`dataset-metadata.json`** is the contract between the data producer and the
catalog/pipeline UX layer.  It is optional at the storage level: a fetch
succeeds without it.  Without it, catalog exposure is degraded.

---

### §10-2. dataset-metadata.json layer

| Property | Value |
|---|---|
| OCI layer mediaType | `application/vnd.sori.dataset.metadata.v1+json` |
| Required for fetch | No — fetch proceeds without it |
| Required for catalog exposure | Yes — catalog entry is unavailable or degraded without it |
| Manifest position | See D-3; located by mediaType, not positional index |

The layer is included in the OCI manifest alongside chunk layers, ensuring it
is subject to the same OCI GC semantics as the rest of the artifact.  A
catalog service fetches only this blob from the manifest; it does not need to
download any chunk blobs.

Supplied via `PackageOptions.DatasetMetadata []byte`.  If the field is nil,
the layer is omitted from the manifest.

---

### §10-3. dataset-metadata.json schema

Minimum fields for a V1 catalog-capable artifact:

```json
{
  "schemaVersion": "sori.dataset.metadata.v1",
  "kind": "reference_genome",
  "displayName": "GRCh38 BWA Index (Homo sapiens hg38)",
  "description": "Pre-built BWA-MEM2 index for GRCh38/hg38. Suitable for short-read whole-genome alignment.",
  "organism": {
    "name": "Homo sapiens",
    "taxonomyId": "9606"
  },
  "reference": {
    "name": "GRCh38",
    "version": "p14",
    "aliases": ["hg38", "GCA_000001405.15"]
  },
  "dataTypes":            ["index", "reference"],
  "fileFormats":          ["bwa-index"],
  "compatibleTools":      ["bwa", "bwa-mem2"],
  "compatibleNodeTypes":  ["BwaAlignNode", "BwaMem2AlignNode"],
  "compatibleInputTypes": ["reference_genome", "bwa_index"],
  "compatibleInputs": [
    {
      "inputType":       "reference_genome",
      "format":          "bwa-index",
      "compatibleTools": ["bwa", "bwa-mem2"],
      "organism":        "Homo sapiens",
      "reference":       "GRCh38"
    }
  ],
  "sizeBytes": 42949672960,
  "source":  "https://ftp.ncbi.nlm.nih.gov/genomes/all/GCA/000/001/405/GCA_000001405.15_GRCh38/",
  "license": "public-domain",
  "tags": ["human", "hg38", "bwa", "alignment"],
  "createdAt":        "2026-05-30T09:00:00Z",
  "createdBy":        "pipeline-admin@example.org",
  "validationStatus": "validated",
  "artifactRef":      "harbor.internal/genomics/references:grch38-bwa-20260530"
}
```

> **Note**: `manifestDigest` is intentionally absent from dataset-metadata.json.
> dataset-metadata.json is pushed as an OCI layer; its own blob digest
> contributes to the manifest digest calculation.  Embedding `manifestDigest`
> inside the document would create the same self-reference problem as the
> removed `artifactDigest` field in chunk-index.json (see D-4).  The manifest
> digest is supplied externally by the catalog/indexer after resolving the tag.

#### Field reference

| Field | Type | Required | Description |
|---|---|---|---|
| `schemaVersion` | string | Yes | Fixed `"sori.dataset.metadata.v1"` |
| `kind` | string | Yes | Dataset category (e.g. `reference_genome`, `annotation`, `index`) |
| `displayName` | string | Yes | Human-readable name shown in catalog / pipeline editor |
| `description` | string | Yes | One-paragraph plain-text description |
| `organism.name` | string | Recommended | Scientific name (e.g. `Homo sapiens`) |
| `organism.taxonomyId` | string | Recommended | NCBI Taxonomy ID as a string (e.g. `"9606"`) |
| `reference.name` | string | Recommended | Reference build name (e.g. `GRCh38`) |
| `reference.version` | string | Optional | Patch version (e.g. `p14`) |
| `reference.aliases` | []string | Optional | Common aliases (e.g. `["hg38"]`) |
| `dataTypes` | []string | Recommended | Semantic type tags (e.g. `["index", "reference"]`) |
| `fileFormats` | []string | Recommended | File format identifiers (e.g. `["bwa-index", "fasta"]`) |
| `compatibleTools` | []string | Recommended | Tool names that consume this dataset |
| `compatibleNodeTypes` | []string | Recommended | Pipeline node type IDs that accept this dataset |
| `compatibleInputTypes` | []string | Yes (catalog routing) | Flat list of input type identifiers for quick search/filter |
| `compatibleInputs` | []object | Recommended | Structured compatibility records; each entry specifies `inputType`, `format`, `compatibleTools`, `organism`, `reference` — used for precise pipeline editor matching |
| `sizeBytes` | int64 | Recommended | Total uncompressed size in bytes |
| `source` | string | Optional | Upstream URL or citation |
| `license` | string | Recommended | License identifier (e.g. `"public-domain"`, `"CC-BY-4.0"`) |
| `tags` | []string | Optional | Free-form search/filter tags |
| `createdAt` | RFC3339 | Recommended | Artifact creation timestamp |
| `createdBy` | string | Optional | Operator identifier (email or username) |
| `validationStatus` | string | Optional | `"validated"` \| `"unvalidated"` \| `"deprecated"` |
| `artifactRef` | string | Recommended | `registry/repo:tag` used to pull this artifact |
| ~~`manifestDigest`~~ | — | **Absent** | Self-reference: cannot be embedded (see schema note above). Catalog/indexer fills this in externally from the OCI resolve step. |

---

### §10-4. Catalog projection contract

The pipeline editor and NodeVault catalog service must not interpret
`chunk-index.json` directly.  The catalog projection pipeline is:

```
OCI manifest
  → locate layer by mediaType vnd.sori.dataset.metadata.v1+json
  → fetch dataset-metadata.json blob (single small blob; no chunk downloads)
  → catalog service / indexer maps to CatalogEntry
  → pipeline editor reads CatalogEntry
```

#### CatalogEntry structure

`id` (= manifest digest) is filled in by the catalog/indexer from the OCI
tag-resolve step, not from the dataset-metadata.json blob itself (see schema
note in §10-3).

```json
{
  "id":           "sha256:abc123...",
  "displayName":  "GRCh38 BWA Index (Homo sapiens hg38)",
  "shortDescription": "Pre-built BWA-MEM2 index for the GRCh38/hg38 human reference genome.",
  "tags":         ["human", "hg38", "bwa", "alignment"],
  "category":     "reference_genome",
  "sizeBytes":    42949672960,
  "validated":    true,
  "artifactRef":  "harbor.internal/genomics/references:grch38-bwa-20260530",
  "compatibleInputTypes": ["reference_genome", "bwa_index"],
  "compatibleInputs": [
    {
      "inputType":       "reference_genome",
      "format":          "bwa-index",
      "compatibleTools": ["bwa", "bwa-mem2"],
      "organism":        "Homo sapiens",
      "reference":       "GRCh38"
    }
  ],
  "uiHints": {
    "icon":  "dna",
    "color": "blue"
  }
}
```

**Degraded mode** (dataset-metadata.json absent): the catalog entry is either
suppressed or shown with `displayName = artifactRef` and `validated = false`.
Low-level fetch via `FetchVolume` / `FetchVolumeFromRemote` still succeeds;
only the catalog UX is degraded.

---

### §10-5. Pipeline editor integration example

**Scenario**: a user wires up a BWA alignment node in the pipeline editor.

1. The node definition declares `inputType: "reference_genome"` and
   `format: "bwa-index"`.
2. The pipeline editor performs a coarse search via the flat shortcut:
   `GET /catalog?compatibleInputTypes=reference_genome`
3. The catalog returns candidate entries based on `compatibleInputTypes`.
4. For precise filtering, the editor evaluates each entry's `compatibleInputs`
   array to confirm a record where `inputType == "reference_genome"` AND
   `format == "bwa-index"` exists.  Entries that lack a matching record are
   filtered out even if they appear in the coarse results.
5. The editor presents: **GRCh38 BWA Index** — 40 GB — validated ✓
6. The user selects the entry.  The editor stores the `artifactRef` in the
   node's input binding.
7. At pipeline execution time, the executor calls
   `Client.FetchVolumeFromRemote(ctx, destRoot, target, tag, opts)` using the
   stored `artifactRef`.

The executor never reads `chunk-index.json` or `dataset-metadata.json` directly.
`dataset-metadata.json` is consumed exclusively by the catalog layer; the
execution layer only calls the sori fetch API.

---

### §10-6. Product readiness criteria

An implementation that passes all §6 functional tests and §7 benchmark gates
but does not include `dataset-metadata.json` support is a **storage PoC**, not
a **product-ready V1**.

| Criterion | Gate |
|---|---|
| `dataset-metadata.json` schema frozen (schemaVersion, required fields) | **V1 blocker** |
| Catalog projection contract documented (CatalogEntry shape) | **V1 blocker** |
| `PackageOptions.DatasetMetadata []byte` accepted and pushed as OCI layer | **V1 blocker** |
| Fetch writes `dataset-metadata.json` to `destRoot` when layer is present | **V1 blocker** |
| `schemaVersion` mismatch on catalog read → `ErrValidation` | **V1 blocker** (mirrors §7-9) |
| Integration test: push with metadata → catalog projection → round-trip verify | **V1 blocker** |
| `uiHints` controlled vocabulary (icon names, color palette) | Future — V1 field is free-form string |
| Full ontology validation (NCBI Taxonomy API, OBO lookups) | Future |
| External metadata registry sync (dbGaP, EGA, BioStudies) | Future |
| Advanced catalog search ranking / relevance scoring | Future |

---

### §10-7. Non-goals (V1)

| Excluded | Reason |
|---|---|
| Full ontology integration (OBO, OLS, NCBI Taxonomy API) | External service dependency; V1 accepts free-form strings only |
| Advanced search ranking / relevance scoring | Catalog service concern; sori defines the contract, not the search algorithm |
| Rich icon / asset system | `uiHints.icon` is a free-form string; controlled vocabulary and assets are a front-end concern |
| External metadata registry sync (dbGaP, EGA, BioStudies) | Cross-registry provenance requires a separate integration layer |
| Automated metadata validation against biological databases | V1 does not call NCBI or any live API during push |
| Metadata encryption or redaction | Patient/sample data is out of scope (see §2, §7-8); public reference data requires no redaction |
