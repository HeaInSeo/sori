# Technical Architecture

## Three-Layer Design

The chunked CAS artifact format is built on three layers:

```
Layer 1 — Chunk blobs
  Individual fixed-size raw byte ranges of source files, each stored as a
  separate OCI layer blob addressed by its sha256 digest.

Layer 2 — chunk-index.json
  The storage reconstruction contract: file paths, chunk offsets, sizes,
  and digests. Stored as an OCI layer (not the config blob). Fetchers
  locate it by media type, not by positional index.

Layer 3 — OCI manifest
  The top-level artifact descriptor. Config media type signals format
  version. Layers list chunk-index, optional metadata blobs, and all
  chunk blobs in order.
```

---

## Key Design Decisions

### D-1: Fixed Chunk Size (Not Content-Defined Chunking)

V1 uses fixed-size chunking at a default of 1 GiB (configurable 256 MiB –
2 GiB). Content-defined chunking (CDC / Rabin fingerprint) would improve
cross-version deduplication for datasets with insertions but adds rolling-hash
complexity, determinism requirements, and cross-platform reproducibility
concerns. Fixed-size chunking is predictable: a 40 GiB STAR index produces
approximately 40 chunks; the manifest size is manageable and the restart
granularity is sufficient for the target dataset sizes.

File boundaries are never shared between chunks. Each file contributes at least
one chunk regardless of size. This simplifies reassembly: a chunk belongs to
exactly one file at exactly one offset.

### D-3: OCI Config Blob as Format Detection Signal

The OCI manifest's config descriptor (`manifest.Config.MediaType`) is the
primary format detection signal for dual-path fetch:

```
manifest.Config.MediaType == "application/vnd.oci.image.config.v1+json"
    -> legacy tar.gz path

manifest.Config.MediaType == "application/vnd.sori.chunked-cas.config.v1+json"
    -> chunked CAS path
```

The config descriptor blob contains a secondary validation document:

```json
{
  "schemaVersion": "sori.chunked-cas.config.v1",
  "artifactFormat": "sori.chunked-cas.v1"
}
```

`schemaVersion` identifies the schema of the config document itself.
`artifactFormat` identifies the artifact layout version. Separating the two
fields allows the config schema to evolve (for example, adding `producerVersion`
or `features`) without changing the artifact format version string.

### D-4: chunk-index.json as the Storage Reconstruction Contract

`chunk-index.json` is the only document a fetcher needs to reconstruct the
source directory. It contains:

- `schemaVersion`: fixed string `"sori.chunked-cas.v1"` for format detection
- `chunkSize`: the nominal chunk size used for this artifact
- `files[]`: for each file, `path`, `mode`, `size`, `digest` (whole-file
  sha256), and `chunks[]` (each with `offset`, `size`, `digest`)

`chunk-index.json` must never contain organism names, reference build names,
display names, tool compatibility flags, or any domain or UI concern. It is a
low-level integrity contract. Domain metadata belongs in `dataset-metadata.json`
(see D-4 / separation of concerns below).

There is no `artifactDigest` field. The chunk-index identity is established by
the OCI descriptor digest of the blob itself as recorded in the manifest layer
entry. A self-referential field would be structurally awkward to compute and
verify; the OCI descriptor is sufficient.

### D-5: Registry-Level Dedup via Exists() Before Push

Before pushing each chunk blob, the implementation calls `registry.Exists(chunkDigest)`.
If the registry already holds a blob with that digest, the push is skipped and
only a `ChunkSkipped` progress event is emitted. This leverages OCI
content-addressing: identical chunks across versions or datasets are stored once
and shared automatically. On an unchanged second push of a 40 GiB artifact,
zero chunk bytes are uploaded; only the manifest and metadata blobs are
re-pushed.

### D-10: Path Validation

All paths in `chunk-index.json` are validated before any chunk is fetched or
written. The following are rejected with `ErrPathValidation`:

- Absolute paths (starts with `/`)
- Paths containing `..` components
- Empty path segments (`//` or trailing `/`)
- Duplicate paths within the same artifact

Symlinks are rejected with `ErrValidation` during push. Empty directories are
not preserved (directory structure is implicit from file paths). Device files,
pipes, and sockets are rejected.

### D-13: Dual-Path Fetch Detection

Both the legacy tar.gz path and the chunked CAS path are maintained
indefinitely. Format detection uses `manifest.Config.MediaType` (see D-3).
Fetchers must not assume positional layer ordering; all metadata blobs are
located by media type. A client built before the chunked CAS feature exists
cannot auto-detect the new format; it will fail with an unknown config media
type. The dual-path auto-detection applies only within clients that include
the chunked CAS fetch code.

---

## Two-Pass SectionReader Push

Each chunk is processed without allocating a full 1 GiB buffer:

```
Pass 1 — Hash (no heap allocation for the full chunk)
  f = os.Open(file)
  sr = io.NewSectionReader(f, offset, chunkSize)
  h = sha256.New()
  io.Copy(h, sr)          // reads with stdlib default 32 KiB buffer
  chunkDigest = sha256:hex(h.Sum(nil))

  registry.Exists(chunkDigest)?
    yes -> record in chunk-index, emit ChunkSkipped, continue
    no  -> proceed to Pass 2

Pass 2 — Push
  sr.Seek(0, io.SeekStart)
  registry.Push(chunkDescriptor{digest, size}, sr)
  append chunk entry to in-progress chunk-index
```

Peak memory during push: hash buffer (~32 KiB, stdlib default) plus the
chunk-index.json document in memory (tens of KB for typical datasets). No
per-chunk 1 GiB buffer is allocated. Peak temporary disk: sori creates no
full-artifact temporary file during push. Chunks are read directly from source
files via `io.SectionReader` and pushed in-flight.

---

## Atomic Publish Order (Section 7-5)

The publish sequence ensures no partially-complete artifact is observable at a
named tag until all blobs are present:

1. Push all chunk blobs (content-addressed; safe to push in any order or
   concurrently)
2. Push `chunk-index.json` blob
3. Push `dataset-metadata.json` blob (if provided)
4. Push original `configblob.json` blob as an OCI layer (if provided)
5. Push OCI config descriptor blob
6. Push OCI manifest and assign tag

The tag is only created or updated at step 6. Before that point, individual
blobs exist in the registry CAS but are not reachable from any manifest. If the
push fails at any step before step 6, no tag points to an incomplete manifest.

---

## Worker Pool Design

### Push Concurrency

Upload concurrency is 2 by default, matching the existing sori `PublishOptions`
convention. A channel-based semaphore bounds inflight pushes. With
concurrency=2 and 1 GiB chunks, peak inflight data is 2 x 32 KiB hash buffers
(Pass 1) or 2 x ORAS internal buffers (Pass 2). No 1 GiB buffers are
allocated.

### Fetch Concurrency

Fetch concurrency is 4 by default. Fetch is typically read-bound rather than
write-bound, so a higher concurrency limit is appropriate. A fixed goroutine
pool prevents goroutine explosion on datasets with many small files. Files are
pre-allocated to their full size via `os.File.Truncate(fileSize)` before the
first chunk write, which eliminates fragmentation and detects disk-full
conditions early.

### Backpressure

The worker pool submits work via a channel-based semaphore. If all workers are
busy, submission blocks the walk goroutine rather than spawning unbounded
goroutines. No external rate limiter is applied in V1.

---

## Integrity Verification

Verification is applied in three layers after fetch:

1. **Chunk-level**: after writing each chunk to its destination offset, verify
   sha256 of the written bytes matches `chunk.digest` in chunk-index.json.
   Returns `ErrIntegrity` on mismatch; fetch stops immediately.

2. **File-level**: after all chunks for a file are written, verify sha256 of
   the whole assembled file matches `file.digest` in chunk-index.json.
   Returns `ErrIntegrity` on mismatch.

3. **Tree-level** (metric M-12): after all files are written, re-walk the
   destination tree and verify each file digest matches. Measures reassembly
   correctness end-to-end and is reported as `treeVerifyMs` in the benchmark
   result.

On any integrity failure, the staging directory is removed and no partial
output is left in `destRoot`.

---

## dataset-metadata vs chunk-index Separation

`chunk-index.json` and `dataset-metadata.json` serve different audiences and
are deliberately stored as separate OCI layers:

| Layer | Audience | Purpose |
|---|---|---|
| `chunk-index.json` | sori runtime | Storage reconstruction: file paths, chunk offsets, digests, sizes |
| `dataset-metadata.json` | Catalog / pipeline editor / human operator | Dataset identity, organism, reference build, tool compatibility |

A catalog service fetches only the `dataset-metadata.json` blob from the
manifest; it does not download any chunk blobs. A fetch operation succeeds
without `dataset-metadata.json`; only catalog exposure is degraded when it is
absent.

The catalog projection pipeline is:

```
OCI manifest
  -> locate layer by mediaType vnd.sori.dataset.metadata.v1+json
  -> fetch dataset-metadata.json blob (one small blob; no chunks)
  -> catalog service maps to CatalogEntry
  -> pipeline editor reads CatalogEntry
```

`manifestDigest` is intentionally absent from both `chunk-index.json` and
`dataset-metadata.json`. Both documents are pushed as OCI layers; their own
blob digests contribute to the manifest digest calculation, making
self-reference structurally impossible. The catalog or indexer fills in the
manifest digest externally after resolving the tag.
