# Related Work Notes

## Introduction

sori's differentiator is not a novel chunking algorithm. Fixed-size chunking
is a well-understood technique. The contribution is the combination of:

- OCI registry-based distribution, reusing existing authentication
  infrastructure and content-addressable storage
- Chunk-level deduplication via OCI content-addressing (Exists() before push)
- Chunk-level, file-level, and tree-level integrity verification
- A stable dataset-metadata schema for catalog exposure and pipeline editor
  integration
- A push/fetch library with a format specification that any pipeline framework
  can consume without depending on a specific pipeline engine

The tools below are compared on these dimensions: distribution mechanism,
deduplication, integrity verification, and OCI registry integration.

---

## Refgenie

**Brief description.** Refgenie is a recipe-based genomics asset manager. It
maintains a local registry of genome assets (reference sequences, indices,
annotation files) and downloads them from a configured remote Refgenie server
via HTTP. Assets are defined by recipes that describe how to build or download
them.

**Key difference from sori.** Refgenie has no OCI registry integration. Assets
are downloaded as single HTTP transfers with no chunk-level deduplication. A
failed download must restart in full. There is no mechanism for a producer to
push a new version and have only the changed portion re-uploaded. Refgenie is
designed around a specific server implementation (the Refgenie server) rather
than any OCI-compatible registry.

**Use case fit.** Refgenie is well-suited for individual workstations or small
teams using a shared Refgenie server and primarily downloading pre-existing
reference assets. It is less suited for organisations that already operate OCI
registries and want to distribute large binary artifacts with partial-update and
deduplication properties.

---

## CVMFS (CernVM File System)

**Brief description.** CVMFS is a read-only distributed filesystem delivered
via a Fuse mount. It was developed for distributing software and data in HEP
(high-energy physics) workflows and has bioinformatics deployments. Files are
content-addressed and cached at Squid proxies. A pipeline reads data from the
CVMFS mount as if it were a local filesystem.

**Key difference from sori.** CVMFS is not an OCI artifact format. It requires
a dedicated Stratum 0 (authoritative) server, one or more Stratum 1 mirror
servers, and Squid proxy infrastructure. This is a significant operational
investment for organisations that do not already run CVMFS servers. CVMFS does
not produce OCI manifests, cannot use an existing OCI registry as backing
storage, and has no concept of push/fetch at the library level.

**Use case fit.** CVMFS is appropriate for large-scale HEP or bioinformatics
facilities that already operate CVMFS infrastructure and need transparent
filesystem-level access to large, stable datasets. For organisations that
already operate Harbor or GHCR and want a library-level push/fetch API with OCI
artifact semantics, the operational overhead of adding CVMFS is difficult to
justify.

---

## DataLad

**Brief description.** DataLad is a Git-annex-based data versioning system
designed for managing code and data together. It tracks dataset provenance
through Git history and stores large files in a variety of backends (S3, SSH,
HTTP, etc.) via Git-annex pointers.

**Key difference from sori.** DataLad is not an OCI artifact format and has no
OCI registry integration. The Git object model adds history and provenance
tracking that is valuable for reproducible research code, but adds overhead
that does not provide meaningful benefit for large binary reference genomes
that do not change frequently. There is no chunk-level deduplication at the
registry level and no partial-update capability for binary blobs. DataLad is
designed for code-plus-data management; sori is designed specifically for large
binary artifact distribution.

**Use case fit.** DataLad is appropriate for research environments where dataset
provenance, versioning, and integration with code repositories are primary
concerns. For distributing pre-built genomics indices that are consumed as
opaque binary artifacts by pipeline executors, DataLad's Git-based overhead is
not well matched to the problem.

---

## Nextflow Fusion

**Brief description.** Nextflow Fusion is an S3-streaming capability for
Nextflow pipelines that presents cloud object store data as a virtual filesystem
during pipeline execution. It is designed to reduce the need to stage large
input files before a pipeline task runs.

**Key difference from sori.** Nextflow Fusion is not an OCI artifact format. It
is tightly coupled to AWS S3 (and compatible object stores) and to the Nextflow
pipeline framework. It does not use OCI registries, provides no chunk-level
deduplication for pushing large reference datasets, and has no partial-update
mechanism. Fusion addresses the runtime I/O problem (reading data during
execution); sori addresses the distribution and storage-layer problem (how data
is packaged, pushed, and fetched before or independently of execution).

**Use case fit.** Nextflow Fusion is appropriate for Nextflow pipelines running
on AWS or compatible cloud providers where input data lives in S3. It does not
address the problem of distributing large pre-built reference datasets across
registries or enabling partial-update pushes for organisations that do not use
Nextflow.

---

## ORAS (OCI Registry As Storage)

**Brief description.** ORAS is a library and CLI for pushing and pulling
arbitrary files and directories as OCI artifacts. It provides primitives for
blob push, blob fetch, manifest assembly, and tag management against any
OCI-compatible registry.

**Key difference from sori.** ORAS is the OCI transport layer that sori builds
on. ORAS itself is format-agnostic: it pushes and pulls blobs and manifests but
defines no domain schema, no chunk-level deduplication policy, and no integrity
verification beyond what the OCI content-addressable storage provides. sori
adds the chunked CAS format specification (chunk-index.json schema, fixed-size
chunking, file boundary alignment), Exists()-based chunk deduplication, three-
layer integrity verification, the dataset-metadata schema for catalog exposure,
and the dual-path legacy/chunked fetch detection on top of ORAS primitives.

**Use case fit.** ORAS alone is appropriate when an application needs raw OCI
artifact push/pull without domain-specific constraints. sori is appropriate
when the application is distributing large genomics reference datasets and
needs chunk-level deduplication, partial-update pushes, integrity verification,
and catalog-compatible metadata as a library-level API.

---

## Singularity / Apptainer Image Distribution

**Brief description.** Singularity (now Apptainer) distributes container images
for HPC (high-performance computing) workloads. Images can be pulled from OCI
registries using a SIF (Singularity Image Format) conversion layer. The
distribution model is similar to Docker/OCI image distribution: content-
addressed layers stored in an OCI registry.

**Key difference from sori.** Singularity/Apptainer is designed for execution
images (container filesystems), not reference data artifacts. It has no genomics
metadata schema, no dataset-metadata catalog projection, and no concept of
chunk-level partial update for data files. The OCI layer model used for
container images is not well-suited to multi-GiB data files that change at
the byte level: a single changed byte in a container layer requires re-uploading
the entire layer.

**Use case fit.** Singularity/Apptainer is appropriate for distributing
executable container environments on HPC systems that require a single-file
container image format. It is not designed for the genomics reference data
distribution problem that sori addresses.

---

## DVC (Data Version Control)

**Brief description.** DVC is a Git-based data pointer management system with
pluggable backend storage (S3, GCS, Azure Blob, SSH, HTTP, and others). It
tracks large files by storing their hashes in Git and syncing the actual data to
a configured remote. It provides commands similar to Git (`dvc push`, `dvc pull`)
for managing data alongside code.

**Key difference from sori.** DVC does not integrate with OCI registries. Data
is stored in generic object stores or file servers, not in OCI-compatible
registries with content-addressable layer semantics. There is no chunk-level
deduplication at the registry level: DVC deduplication operates at the file
level (a file with the same hash is not re-uploaded). DVC does not provide a
manifest format for structured binary artifacts or a catalog metadata schema
for pipeline editor integration.

**Use case fit.** DVC is appropriate for research and ML workflows where data
and code are managed together in Git repositories and backend storage is
flexible. For organisations that already operate OCI registries and want to
distribute large binary genomics artifacts with chunk-level guarantees and
catalog integration, sori addresses the specific gap that DVC does not cover.

---

## Summary

sori focuses on the gap between raw OCI push (ORAS) and full data management
systems (DataLad/DVC) — providing a push/fetch library with a format
specification, chunk-level deduplication and integrity guarantees, and catalog
exposure that any pipeline framework can consume. It does not replace general-
purpose data versioning systems or distributed filesystems; it addresses the
specific problem of distributing large, stable, binary genomics reference
artifacts via OCI registries that organisations already operate.
