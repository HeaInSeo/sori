# P5 RFC — Chunked CAS for Large Dataset Artifacts

**Status**: Draft (v2 — post-review corrections applied)
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
    [0]  application/vnd.sori.chunk-index.v1+json    (chunk-index.json)
    [1]  application/vnd.sori.configblob.v1+json     (original configblob.json, if present)
    [2]  application/vnd.sori.chunk.v1               (chunk 0)
    [3]  application/vnd.sori.chunk.v1               (chunk 1)
    ...
```

All blobs appear as layers for OCI GC safety.  `chunk-index.json` is the
primary metadata contract.  `configblob.json` is stored as a dedicated layer
so the original caller-supplied config is preserved independently of format
metadata (see D-7).

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
mediaType `application/vnd.sori.configblob.v1+json` (layers[1] in D-3).  The
chunked fetch path reconstructs `configblob.json` from this layer, mirroring
`restoreConfigBlob` in the legacy path.  If no configblob was provided, this
layer is omitted.

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

This accounts for:
- 1 chunk-index.json layer
- 1 optional configblob layer
- Up to 898 chunk layers

If the computed chunk count would exceed `MaxChunkedLayers - 2`, the publish
call returns `ErrValidation`.

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
  2. Compute total chunk count; if > MaxChunkedLayers-2 → ErrValidation
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
  5. If configblob provided: push as blob (mediaType vnd.sori.configblob.v1+json)
  6. Build OCI manifest (config + chunk-index + optional configblob + all chunks)
  7. Push manifest under volName tag
```

**Peak memory**: hash buffer (~32 KiB stdlib default) plus chunk-index.json
in memory (tens of KB for typical datasets).  No per-chunk 1 GiB buffer.
**Peak disk**: zero temp files.  Chunks are read directly from source files
via `io.SectionReader` and pushed in-flight.

This eliminates the disk spike (1-A) entirely.

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
  8. Locate configblob layer by mediaType vnd.sori.configblob.v1+json (if present)
     fetch and write configblob.json under destRoot
  9. Write volume-index.json
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

## 7. Open questions (resolve before implementation)

| # | Question | Impact |
|---|---|---|
| OQ-1 | Do Harbor / Zot / ECR / GHCR cap layer count, and at what value? | May require lowering MaxChunkedLayers or prioritising small-file packing |
| OQ-2 | Is `PackageOptions.Format ArtifactFormat` the right API shape, or a separate `ChunkedPackageOptions`? | Public API surface; hard to change after stable |
| OQ-3 | Should symlinks be supported in V1.1, and if so, how are link targets stored in chunk-index.json? | Schema extension |
| OQ-4 | Should `VolumeIndex` gain an `ArtifactFormat` field to let callers distinguish format without re-fetching the manifest? | Typing convenience; backward compatibility if added later |
| OQ-5 | What is the correct `chunkSize` for artifacts that mix very large and very small files? Should chunk size be per-file rather than per-artifact? | Schema change if per-file |

---

## 8. V1.1 and future RFC items

| Item | Why deferred |
|---|---|
| **Fetch resume** | Conflicts with current staging policy; needs separate staging-cache design |
| **Symlink support** | Schema change required; V1 rejects symlinks explicitly |
| **Small-file packing** | Complicates reassembly; revisit after OQ-1 is resolved |
| **CDC chunking** | High complexity (rolling hash, determinism, cross-platform tests); separate RFC |
| **Chunk-level compression** | Low value for genomics files; separate RFC |
| **Remote-to-remote copy** | Requires registry-side blob mount; separate RFC |
| **Encryption at rest** | Out of scope for sori core; adapter-layer concern |
