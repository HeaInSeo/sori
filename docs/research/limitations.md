# Known Limitations

## V1 Limitations (Deferred to V1.1)

These limitations are known design gaps in V1. Each has a concrete reason for
deferral and is tracked for V1.1.

### No Fetch Resume

If a fetch operation is interrupted after some chunks have been written, the
staging directory is removed on failure and the entire fetch must restart from
the beginning. This is consistent with the existing sori staging policy
(`defer os.RemoveAll(stagingDir)`).

Resumable fetch requires a persistent staging cache outside the normal staging
path, or a modified failure policy that preserves partial output. This is a
separate design problem and should not block V1. Deferred to V1.1 (see
p5-rfc.md D-6, section 9).

### No Symlink Support

V1 only handles regular files. Symlinks encountered during push are rejected
with `ErrValidation`. Supporting symlinks requires a schema change to
`chunk-index.json` to store link targets; the V1 schema has no such field.
Deferred to V1.1 (see p5-rfc.md D-10, section 9).

### No Small-File Packing

Each file becomes at least one chunk, regardless of size. A dataset with many
small files (for example, thousands of `.fai` index files of a few kilobytes
each) produces a large number of single-chunk layers. This is operationally
inefficient but functionally correct. Small-file packing (combining multiple
small files into a single chunk blob) complicates reassembly and is deferred
to a future RFC.

The `MaxChunkedLayers=900` hard cap acts as a guardrail: if the total chunk
count would exceed 900 layers, `ChunkedPublish` returns `ErrValidation` before
any blob is pushed. This prevents unbounded manifest growth for small-file
datasets.

### MaxChunkedLayers=900 Hard Cap

The total OCI layer count per manifest is capped at 900. This value is derived
from ECR's documented 1000-layer limit with headroom. The effective chunk layer
budget is computed dynamically:

```
metadataLayerCount = 1                       (chunk-index.json, always)
                   + 1 if DatasetMetadata    (optional)
                   + 1 if ConfigBlob         (optional)
maxChunkLayers = 900 - metadataLayerCount
```

Worst case (both optional layers present): 897 chunk layers.
Minimum case (neither optional layer): 899 chunk layers.

Once ECR's empirical limit is verified in P5-7 integration tests, this constant
may be raised. Small-file packing (future RFC) would allow datasets with many
small files to fit within the limit.

---

## Scope Limitations (By Design, Not Bugs)

These are deliberate scope boundaries, not implementation gaps.

### Public Reference Genomics Data Only

sori is designed for publicly available genomics reference data: reference
genomes, pre-built indices, annotation files. It provides no encryption at
rest, no access-control enforcement, and no audit logging.

Storing HIPAA-covered or GDPR-covered data (patient-derived or sample-level
genomic data) via sori without an appropriate encryption and access-control
adapter layer is not supported. This constraint is enforced by documentation
and user agreement; sori has no mechanism to detect whether a file contains
personal genomic data.

### No Registry-to-Registry Copy

sori does not support direct registry-to-registry copy. To copy a chunked CAS
artifact from one registry to another, push it to a local OCI layout store
first, then use ORAS copy to transfer it to the destination registry.

### No CDC Chunking

V1 uses fixed-size chunking only. Content-defined chunking (CDC / Rabin
fingerprint) is not supported. CDC would improve cross-version deduplication
for datasets with insertions, but adds rolling-hash complexity, determinism
requirements, and cross-platform reproducibility concerns. Deferred to a
future RFC (see p5-rfc.md section 9).

### No Per-File Chunk Size

The chunk size is configured per artifact, not per file. A dataset containing
both very large files and very small files uses the same nominal chunk size for
all files. Per-file chunk sizes would require a schema change to
`chunk-index.json`. Deferred to V1.1 (open question OQ-5 in p5-rfc.md).

---

## Measurement Limitations

### Peak RSS Approximation

`pushPeakRSSMiB` (M-05) is measured by sampling `runtime.MemStats.HeapInuse +
runtime.MemStats.StackInuse` at 50 ms intervals during the push operation. This
is a Go heap and stack approximation, not the actual process RSS as reported by
`/proc/<pid>/status`. The value may undercount memory held by the OS page cache
or cgo/CGO allocations. For the purposes of the gate criterion (must stay below
256 MiB at concurrency=2), this approximation is sufficient.

### Temp Disk Sampling

`pushPeakTempDiskMiB` (M-06) is measured by summing the sizes of regular files
under the OCI store directory at 100 ms intervals. This is a directory-size
approximation, not an inode-level accounting. Small metadata files written by
the OCI store implementation may appear in this measurement. The gate threshold
is 10 MiB to accommodate this overhead.

### Retry Behaviour Not Benchmarked

`retryPushSeconds` (M-04) is `0.0` in all current benchmark results. Measuring
retry behaviour accurately requires a mock registry that returns 5xx errors at
a controlled point in the push sequence. This is deferred to integration testing
with a fake registry implementation.

---

## Registry Limitations

### Full Round-Trip Tested Only with Local OCI Store

All unit tests and benchmark runs use a local OCI layout store (`oci.New`).
The local OCI store's `Exists()` behaviour and digest verification differ
slightly from live registries (for example, local OCI does not enforce upload
ordering or GC policies). Full round-trip correctness on live registries is
verified only in integration tests (P5-7), which are gated behind the
`integration` build tag and are not run in CI.

### Harbor, GHCR, and ECR Require Manual Integration Run

Integration tests for Harbor, GHCR (GitHub Container Registry), and ECR
(Amazon Elastic Container Registry) require credentials and live registry
endpoints. These are V1 release blockers (B-4) but are not automated in CI.
Manual integration runs are required before the V1 tag is created.

### Zot Compatibility Not Yet Verified

Zot is an open-source OCI reference registry implementation. It is listed as a
nice-to-have in the P5-7 compatibility matrix. Compatibility has not been
verified as of V1. If Zot compatibility is important for a deployment, a manual
integration run should be performed before relying on chunked CAS artifacts with
a Zot backend.
