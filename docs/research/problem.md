# Problem Statement

## Scale of Genomics Reference Datasets

Modern genomics pipelines depend on large pre-built reference artifacts:

- STAR genome index: approximately 40 GiB (8–12 binary files)
- BWA-MEM2 index for hg38: approximately 15 GiB (5 binary files)
- GRCh38 reference FASTA: approximately 3 GiB (single file, low entropy)
- Combined pipeline environment (reference + index + annotation): 50–60 GiB

These datasets are stable and shared across many pipeline runs. They change
infrequently — typically only when a new reference build is released — but are
accessed every time a pipeline executes. Efficient, reproducible distribution
is therefore a prerequisite for any production genomics compute environment.

---

## The Legacy sori Format

The original sori publish path wraps each source directory (partition) into a
single gzip-compressed tar layer and stores it as an OCI image:

```
source directory
  |
  v  TarGzDirTo
tar.gz stream --> temp file (full layer size on disk)
  |
  v  seek(0)
ORAS blob push
  |
  v
OCI manifest push
```

This design has two structural problems for large datasets.

### Disk Spike

A 40 GiB STAR index requires approximately 40 GiB of temporary disk space
during push. The temp file is necessary because ORAS `Store.Push` requires the
OCI descriptor's `Digest` and `Size` fields before reading the stream, and a
tar.gz stream cannot report its compressed size until the last byte is written.
On a server where `/tmp` shares a volume with the source data, this doubles the
effective disk requirement.

### Failure Restart Cost

A 30 GiB push that fails at 28 GiB — due to a network timeout, registry error,
or operator intervention — must restart from byte zero. The entire artifact is
re-uploaded regardless of how much was transferred before the failure. For
reference datasets that change infrequently, this is operationally expensive
and discourages incremental updates.

---

## Legacy vs Chunked CAS Comparison

| Dimension | Legacy tar.gz | Chunked CAS |
|---|---|---|
| Temp file size during push | O(artifact size) — one full-layer tar.gz | O(metadata) — no full-artifact temp file |
| Peak RSS during push | O(artifact size) or O(compress buffer) | Bounded: ~256 MiB at concurrency=2, independent of chunk size |
| Second push upload bytes | 100% re-uploaded (no dedup) | 0 chunk bytes (all chunks exist in registry CAS) |
| Partial update upload bytes | 100% re-uploaded | Only modified chunks; unmodified chunks skipped via Exists() check |
| Integrity verification | None at chunk level; tar extraction trust | Three-layer: chunk sha256, file sha256, tree walk |
| File boundary alignment | Not preserved (cross-file tar blocks) | Strict: each file chunked independently; no chunk crosses a file boundary |
| Registry layer count | 1 layer per partition | 1 chunk-index.json + 1 per chunk (up to 899) + optional metadata layers |
| Fetch resume support | None — restart from zero on failure | None in V1 (deferred); restart from zero; V1.1 planned |

---

## Why OCI Registries Are the Right Distribution Primitive

OCI registries provide three properties that make them well-suited for large
genomics reference distribution:

1. **Content-addressable storage.** Every blob is addressed by its sha256
   digest. An identical chunk pushed from two different datasets, or two
   versions of the same dataset, is stored once and referenced by both
   manifests. This is chunk-level deduplication with no additional
   infrastructure.

2. **Existing authentication and authorisation infrastructure.** Harbor, GHCR,
   ECR, and Zot all provide token-based authentication that bioinformatics
   organisations already operate for container image distribution. sori
   credentials flow through the same `AuthConfig` mechanism, reusing existing
   registry accounts without additional key management.

3. **CDN integration and geographic distribution.** Registry operators can
   front blob storage with a CDN. A pipeline running in a different region
   benefits from CDN caching of chunk blobs automatically, without any
   changes to the sori client.

---

## Why Not Other Approaches

### Refgenie

Refgenie is a recipe-based genomics asset manager with a local registry. Assets
are downloaded via HTTP from a configured remote server. Refgenie has no OCI
integration, no registry-level deduplication (each download transfers the full
asset), and no chunk-level integrity verification. It is well-suited for
individual workstations or small teams using a shared Refgenie server, but
provides no partial-update or retry-resume capability for large binary assets.

### CVMFS (CernVM File System)

CVMFS is a read-only distributed filesystem delivered via Fuse mount. It is
widely used in HEP (high-energy physics) workflows and has bioinformatics
deployments. However, it is not an OCI artifact format: CVMFS requires a
dedicated Squid proxy and Stratum 1 mirror infrastructure, which adds
operational complexity for organisations that do not already run CVMFS servers.
CVMFS does not produce OCI manifests and cannot use an existing OCI registry as
backing storage. For organisations that already operate Harbor or GHCR, adding
CVMFS infrastructure to distribute reference data is a significant operational
cost.

### DataLad

DataLad is a Git-annex-based data versioning system designed for managing
code and data together. Its strength is tracking dataset provenance through
Git history. For large binary genomics reference datasets, the Git object model
adds overhead that does not provide meaningful benefit: reference genomes are
not version-controlled in the same sense as code. DataLad does not integrate
with OCI registries and does not provide registry-level chunk deduplication or
partial-update capability.

### Nextflow Fusion

Nextflow Fusion streams data directly from S3 during pipeline execution,
presenting it as a virtual filesystem. It is tightly coupled to AWS S3 (and
related object stores) and to the Nextflow pipeline framework. It is not an OCI
artifact format, does not use OCI registries, and provides no chunk-level
deduplication or partial-update for pushing large reference datasets. Fusion
is a runtime optimisation for compute-to-storage I/O within a specific cloud
provider; sori addresses the distribution and storage-layer problem across
registry providers.
