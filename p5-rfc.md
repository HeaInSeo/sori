# P5 RFC — Chunked CAS for Large Dataset Artifacts

**Status**: Draft  
**Target**: sori v0.6 (experimental flag), v0.7+ (stable)  
**Problem driver**: Large genomics reference datasets (STAR index 40 GB, hg38 reference 60 GB+)

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

A 40 GB STAR index requires ~40 GB of temp space during push (the compressed
tar.gz may be slightly smaller, but the order of magnitude holds).  For a
server where `/tmp` is on the same volume as the data, this doubles effective
disk requirement.

Step 2 of P3-1 cannot eliminate this.  ORAS `Store.Push` requires
`ocispec.Descriptor.Digest` and `.Size` before reading the stream; a tar.gz
stream does not know its final size until the last byte is written.  Some form
of spooling is structurally required with the current artifact format.

### 1-B. Failure restart cost

A 30 GB push that fails at 28 GB restarts from zero.  For genomics reference
sets that change infrequently but are large, this is operationally expensive.
Retrying over a slow network link may take hours.

---

## 2. Non-goals (V1)

| Excluded | Reason |
|---|---|
| Content-defined chunking (CDC / Rabin fingerprint) | Adds significant complexity (rolling hash, boundary algorithm, determinism, cross-platform reproducibility, test surface). Deferred to a future RFC. |
| Migration of existing artifacts | Existing artifacts must keep working unchanged. No migration path is needed; dual-path fetch handles it. |
| Streaming single-file upload without any in-memory buffer | Each chunk (1 GiB) is read into a buffer for hashing + push. That buffer is the new "temp", sized per chunk rather than per layer. |
| CLI surface changes | sori is a library. Any CLI flags are the caller's concern. |

---

## 3. Decisions

### D-1. Chunk size

| Parameter | Value |
|---|---|
| Default | 1 GiB (1 073 741 824 bytes) |
| Configurable range | 256 MiB – 2 GiB |
| Initial recommendation | 1 GiB |

Rationale: a 60 GB file produces ~60 chunks; a 200 GB reference set produces
~200 chunks.  Manifest size stays manageable, and restart granularity is
sufficient.  512 MiB doubles chunk count with little benefit at this stage;
2 GiB increases restart cost.

### D-2. Chunking strategy

**File-aware fixed-size chunking.**

Algorithm:
1. Walk the source directory recursively; collect regular files sorted by path.
2. For each file, divide into fixed-size chunks.  The final chunk of a file
   may be smaller.
3. Compute `sha256` of each chunk independently.
4. Small files (< chunk size) become a single chunk each.  Small-file packing
   (bundling multiple small files into one chunk) is **not** in V1 — it
   complicates the schema and fetch reassembly.

### D-3. Manifest structure

**Option B** (chunk-index.json as primary contract):

```
OCI Image Manifest
  config:   application/vnd.sori.chunked-cas.config.v1+json
  layers:
    [0]  application/vnd.sori.chunk-index.v1+json   (chunk-index.json)
    [1]  application/vnd.sori.chunk.v1              (chunk 0 of first file)
    [2]  application/vnd.sori.chunk.v1              (chunk 1 of first file)
    ...
```

All chunks appear as layers so that OCI-compliant registries do not GC them.
`chunk-index.json` is the single source of truth for file paths, modes, sizes,
and chunk boundaries.  The layer ordering in the manifest mirrors the chunk
ordering in chunk-index.json; this is an invariant fetchers may rely on.

### D-4. chunk-index.json schema

```json
{
  "schemaVersion": "sori.chunked-cas.v1",
  "artifactDigest": "sha256:<digest-of-chunk-index-json-itself>",
  "chunkSize": 1073741824,
  "files": [
    {
      "path": "hg38.fa",
      "mode": 420,
      "size": 64424509440,
      "digest": "sha256:<whole-file-digest>",
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
- `artifactDigest` is the sha256 of the serialised chunk-index.json bytes
  (self-referential; computed last).
- `mode` is the Unix file permission bits as a decimal integer.
- `digest` at the file level is the sha256 of the full unmodified file
  (pre-chunking); used for whole-file integrity verification after reassembly.
- `offset` is the byte offset within the file for the chunk's starting byte.

### D-5. Push resume

Before pushing each chunk blob, call `oras.ReadOnlyTarget.Resolve` (or
equivalent existence check) on the registry using the chunk's digest.  If the
blob already exists, skip the push.  This leverages OCI content-addressable
deduplication — identical chunks across artifact versions or different datasets
are automatically shared.

Implementation uses the same `pushIfNeeded` pattern already established in
`publishVolumeToStore`.

### D-6. Fetch resume / verification

Before writing each chunk to disk, check whether the destination file segment
already exists with the correct digest.  If a previous fetch was interrupted,
already-verified segments are skipped.  This requires a small per-fetch state
file (e.g. `.sori-fetch-progress.json` alongside the destination) that is
removed on successful completion.

Fetch resume is a V1 goal but may be deferred to V1.1 if it complicates the
initial implementation materially.

### D-7. Dual-path fetch (backward compatibility)

The fetch path detects artifact format from the OCI manifest:

```
manifest.config.MediaType == "application/vnd.oci.image.config.v1+json"
    → legacy tar.gz path (existing fetchVolSeqFrom / fetchVolParallelFrom)

manifest.config.MediaType == "application/vnd.sori.chunked-cas.config.v1+json"
    → chunked CAS fetch path (new)
```

No migration of existing artifacts.  Both paths are maintained indefinitely
until the legacy format is formally deprecated (separate RFC).

### D-8. Experimental flag

The chunked CAS push path is gated behind `PublishOptions.ChunkedCAS bool`
(provisional name).  The option is documented as experimental:

```go
type PublishOptions struct {
    ChunkedCAS bool // Experimental: use chunked CAS format for large datasets.
}
```

The fetch path is **not** gated — it detects the format automatically.  A
client that has never set `ChunkedCAS` can still fetch a chunked artifact
produced by another client.

---

## 4. Push flow (V1)

```
ChunkedPublish(ctx, srcDir, volName, opts):

  1. Walk srcDir → sorted file list
  2. For each file:
       for each chunk (fixed-size slices):
         read chunk bytes into buffer (max 1 chunk = 1 GiB in memory)
         sha256(buffer) → chunkDigest
         if registry.Exists(chunkDigest): skip
         else: registry.Push(chunkDescriptor, buffer)
         append chunk metadata to in-progress chunk-index
  3. Serialise chunk-index.json → push as blob
  4. Build OCI manifest (config + chunk-index layer + all chunk layers)
  5. Push manifest under volName tag
```

Peak memory: 1 chunk buffer + chunk-index.json (tens of KB).
Peak disk: zero temp files (chunks are pushed directly from memory buffer).

This eliminates the disk spike entirely — the original problem.

---

## 5. Fetch flow (V1)

```
ChunkedFetch(ctx, destRoot, src, tag):

  1. Resolve tag → manifest descriptor
  2. Fetch manifest
  3. Detect format via config.MediaType
     → legacy: delegate to fetchVolSeqFrom / fetchVolParallelFrom
     → chunked: continue below
  4. Fetch chunk-index.json (layers[0])
  5. Parse chunk-index.json → file list
  6. For each file:
       create destination file (os.Create with full size pre-allocated)
       for each chunk (parallel up to concurrency limit):
         fetch chunk blob → write at correct offset
         verify sha256 matches chunk-index entry
  7. For each file: verify whole-file digest matches chunk-index entry
  8. Write volume-index.json
```

Parallel fetch: chunks within a file and across files can be fetched
concurrently.  The existing worker-pool pattern from `fetchVolParallelFrom`
is reused.

---

## 6. Test plan

### Synthetic fixtures (no real genomics data needed)

| Fixture | Purpose |
|---|---|
| `small.txt` (1 KB) | Single-chunk small file |
| `medium.bin` (10 MB) | Small file, single chunk |
| `large.bin` (3 × chunkSize + 500 MB) | Multi-chunk file with partial final chunk |
| `unchanged-large` | Same content as `large.bin` in a second push |
| `changed-large` | First 100 bytes modified; verifies only affected chunk re-uploads |

### Required test cases

1. **First push**: all chunks uploaded, manifest created.
2. **Identical second push**: all chunks skipped (registry dedup), manifest
   re-tagged.
3. **Partial change push**: only modified chunks uploaded; unmodified chunks
   skipped.
4. **Full fetch**: reassembled directory byte-identical to source.
5. **Interrupted fetch + resume**: partial destination recognised; completed
   chunks skipped.
6. **Legacy fetch**: artifact produced by old tar.gz path still fetches
   correctly via dual-path.
7. **Cross-client**: artifact produced with `ChunkedCAS=true` fetched by a
   client that has only the legacy code (should work via auto-detection).

### Genomics-profile fixtures (after synthetic tests pass)

- BWA index profile: several files, each 2–5 GB.
- STAR index profile: ~40 GB total, 8–12 large binary files.
- hg38.fa profile: single 60 GB FASTA (generated synthetically with
  `dd if=/dev/urandom` or similar; does not need biological content).
- Small metadata profile: `.dict`, `.fai`, JSON files alongside large binaries.

---

## 7. Open questions (to resolve before implementation)

| # | Question | Impact |
|---|---|---|
| OQ-1 | Does Harbor / Zot / other target registries cap layer count per manifest? | Determines whether 200-chunk manifests work in practice |
| OQ-2 | Should `PublishOptions.ChunkedCAS` accept a `ChunkSize` override, or is the default fixed per-push? | API surface decision |
| OQ-3 | Is `.sori-fetch-progress.json` the right mechanism for fetch resume, or should V1 skip resume and just re-download? | Scope of V1 fetch |
| OQ-4 | Should symlinks be preserved? Current tar.gz path preserves them implicitly. Chunked CAS must decide explicitly. | Schema addition if yes |

---

## 8. Out of scope — future RFCs

- **Content-defined chunking (CDC)**: Rolling hash, Rabin fingerprint.
  High value for incremental updates but high implementation complexity.
  Separate RFC after V1 ships.
- **Small-file packing**: Bundling many small files into a single chunk to
  reduce manifest layer count.  Complicates reassembly; revisit if OQ-1
  reveals a hard layer-count limit.
- **Remote-to-remote copy**: Copying a chunked artifact between registries
  without downloading chunks to the client.
- **Encryption at rest**: Chunk-level encryption.  Out of scope for sori core;
  belongs in a separate adapter layer.
