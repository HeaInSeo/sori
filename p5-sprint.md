# P5 Sprint Tracker

**RFC**: `p5-rfc.md` (v6)  
**Target**: v0.6.0-experimental → v0.7.0-stable  
**Started**: 2026-05-30  

Legend: ✅ done · 🔄 in-progress · ⏳ pending · ❌ blocked

---

## Open Question Decisions

| OQ | Question | Decision | Resolved |
|---|---|---|---|
| OQ-1 | Registry layer caps (Harbor / GHCR / ECR) | **Proceed with MaxChunkedLayers=900**. ECR documented 1000-layer limit; Harbor/GHCR have no hard limit below 900 in practice. Empirically verify in P5-7 integration tests. | 2026-05-30 |
| OQ-2 | `PackageOptions.Format ArtifactFormat` vs separate `ChunkedPackageOptions` | **Use `PackageOptions.Format ArtifactFormat`**. Zero value = legacy (backward compatible). Simpler public API surface. Separate struct only if chunked-CAS-specific fields multiply significantly. | 2026-05-30 |
| OQ-3 | Symlink schema for V1.1 | Deferred — V1 rejects symlinks; schema design deferred to V1.1 RFC. | — |
| OQ-4 | `VolumeIndex.ArtifactFormat` field | **Defer to V1.1**. Adding now risks schema incompatibility; callers can re-fetch manifest to detect format. | 2026-05-30 |
| OQ-5 | Per-file vs per-artifact chunkSize | Deferred — V1 uses per-artifact. Schema change tracked in V1.1. | — |

---

## V1 Release Blockers

| # | Condition | Sprint | Status |
|---|---|---|---|
| B-1 | OQ-1 MaxChunkedLayers confirmed empirically | P5-7 | ⏳ |
| B-2 | All 12 functional test cases green | P5-5 | ⏳ |
| B-3 | Benchmark gate passes (all 7 fixtures) | P5-6 | ⏳ |
| B-4 | Harbor + GHCR + Local OCI integration tests pass | P5-7 | ⏳ |
| B-5 | dataset-metadata schema frozen | P5-1 | ⏳ |
| B-6 | Catalog projection contract documented | done (p5-rfc.md §10-4) | ✅ |
| B-7 | Atomic publish order verified in test | P5-2 | ✅ |
| B-8 | schemaVersion mismatch → ErrValidation | P5-4 | ✅ |

---

## P5-0: Open Question Resolution

**Status**: ✅ done (decisions recorded above)  
**Completed**: 2026-05-30

- [x] OQ-1: MaxChunkedLayers=900 confirmed safe; empirical verify deferred to P5-7
- [x] OQ-2: PackageOptions.Format ArtifactFormat chosen
- [x] OQ-4: VolumeIndex.ArtifactFormat deferred to V1.1

---

## P5-1: Schema & API Foundation

**Status**: ✅ done (2026-05-30)  
**Target files**:
- `options.go` — `ArtifactFormat`, `PackageOptions.{Format,DatasetMetadata,Progress}`, `ProgressFunc`, `ChunkProgress`
- `chunked/types.go` (new) — `ChunkIndex`, `ChunkIndexFile`, `ChunkEntry`, `OCIConfigDescriptor`, `DatasetMetadata`, `CompatibleInput`, `Organism`, `Reference`
- `chunked/validate.go` (new) — constants (media types, schema versions, MaxChunkedLayers), path validation, `MetadataLayerCount`, `EstimatedChunkCount`
- `chunked/validate_test.go` (new) — path validation, MaxChunkedLayers dynamic budget

**Checklist**:
- [x] `ArtifactFormat` type + constants in `options.go`
- [x] `PackageOptions` extensions
- [x] `ProgressFunc` / `ChunkProgress` types
- [x] `chunked` package created
- [x] All media type + schema version constants
- [x] `ValidatePath` / `ValidatePaths` per D-10
- [x] `MetadataLayerCount(hasDatasetMetadata, hasConfigBlob bool) int`
- [x] `EstimatedChunkCount(fileSizes []int64, chunkSize int64) int64`
- [x] Unit tests: path validation (valid, absolute, `..`, empty segment, duplicate)
- [x] Unit tests: MaxChunkedLayers dynamic budget
- [x] `go build ./...` passes
- [x] `go test ./chunked/...` green (17 tests)

**Exit criterion**: ✅ `go test ./...` passes with no regressions (all packages green)

---

## P5-2: Push Path — Core

**Status**: ✅ done (2026-05-30)  
**Target files**:
- `chunked/publish.go` (new) — `ChunkedPublish`, `walkAndValidate`, preflight, two-pass SectionReader, chunk-index build, all blob pushes, manifest assembly (§7-5 atomic order)
- `chunked/publish_test.go` (new)

**Checklist**:
- [x] `walkAndValidate(srcDir)` → sorted file list, symlink rejection
- [x] Preflight: chunk estimation + MaxChunkedLayers guard
- [x] Two-pass `io.SectionReader`: Pass 1 hash, Pass 2 push
- [x] Registry `Exists` dedup check (D-5)
- [x] chunk-index.json build + serialize + push
- [x] dataset-metadata push (if present)
- [x] configblob push as OCI layer (if present)
- [x] OCI config descriptor blob push
- [x] OCI manifest assembly + push (atomic order §7-5)
- [x] Basic tests with local OCI store

---

## P5-3: Push Path — Concurrency & Hardening

**Status**: ✅ done (2026-05-30)  
**Target files**:
- `chunked/publish.go` — worker pool (uploadConcurrency=2), backoff (§7-3), progress events, GC retry

**Checklist**:
- [x] Channel-semaphore worker pool (uploadConcurrency=2)
- [x] 5xx exponential backoff: 3 attempts, 500ms→2s, ±10% jitter
- [x] `ChunkSkipped` / `ChunkUploaded` / `ArtifactDone` progress events
- [ ] `Log.Infof` at chunk boundaries when Progress=nil (deferred to P5-5)
- [ ] GC retry: manifest push fail → retry from dedup-aware chunk flow (deferred to P5-5)
- [x] Test: dedup round-trip (ChunkSkipped on second push)
- [x] Test: MaxChunkedLayers guard before any push
- [ ] Test: 5xx mock → backoff → ErrTransport (requires fake registry; deferred to integration)

---

## P5-4: Fetch Path

**Status**: ✅ done (2026-05-30)  
**Target files**:
- `chunked/fetch.go` (new) — dual-path detection, chunk-index fetch+parse, worker pool (fetchConcurrency=4), file pre-alloc, integrity verification, `.sori/` metadata write
- `chunked/fetch_test.go` (new)
- `volume_publish_fetch.go` — integrate dual-path detection at manifest resolve

**Checklist**:
- [x] Dual-path detection via `manifest.Config.MediaType` (D-13)
- [x] Locate chunk-index layer by mediaType (not positional)
- [x] chunk-index.json fetch + parse + schemaVersion enforcement (§7-9)
- [x] Path validation on fetched chunk-index (D-10)
- [x] Worker pool: fetchConcurrency=4, channel semaphore
- [x] `os.File.Truncate(fileSize)` pre-allocation
- [x] Chunk fetch + write at correct offset
- [x] Chunk-level integrity: sha256 verify → ErrIntegrity
- [x] File-level integrity: whole-file sha256 → ErrIntegrity
- [ ] Tree-level verification (M-12) — deferred to P5-5
- [x] dataset-metadata → `destRoot/.sori/dataset-metadata.json`
- [x] configblob → `destRoot/configblob.json` (legacy path)
- [ ] volume-index.json write — deferred to P5-5
- [ ] Staging policy compliance (D-6) — deferred to P5-5
- [x] `ChunkFetched` / `FileDone` / `ArtifactDone` progress events
- [ ] Integrate dual-path into `volume_publish_fetch.go` — deferred to P5-5

---

## P5-5: Test Suite Completeness

**Status**: ⏳ pending (depends on P5-3 + P5-4)  
**Target files**:
- `chunked/e2e_test.go` (new)

**Checklist** (all 12 must be green):
- [ ] TC-01: First push — all chunks uploaded, manifest created
- [ ] TC-02: Identical second push — all chunks skipped (M-08=0)
- [ ] TC-03: Partial change push — only changed chunks uploaded
- [ ] TC-04: Full fetch — byte-identical reassembly (content + mode)
- [ ] TC-05: Chunk integrity failure → ErrIntegrity
- [ ] TC-06: Path validation (absolute/`..`/duplicate/symlink) → ErrValidation
- [ ] TC-07: MaxChunkedLayers → ErrValidation before any blob push
- [ ] TC-08: Legacy fetch — dual-path routes to existing fetch path
- [ ] TC-09: configblob round-trip
- [ ] TC-10: dataset-metadata round-trip (§10-6)
- [ ] TC-11: schemaVersion mismatch → ErrValidation (no blob download)
- [ ] TC-12: context cancel → immediate stop

---

## P5-6: Benchmark Gate

**Status**: ⏳ pending (depends on P5-4, parallel with P5-5)  
**Target files**:
- `internal/bench/fixtures.go` (new)
- `internal/bench/runner.go` (new)
- `internal/bench/gate.go` (new)
- `docs/bench/` (new directory)

**Checklist**:
- [ ] Fixture: synthetic-1GiB (1 file × 1 GiB)
- [ ] Fixture: synthetic-10GiB (5 files × 2 GiB)
- [ ] Fixture: synthetic-50GiB (10 files × 5 GiB)
- [ ] Fixture: genomics-fasta (~3 GiB, low entropy)
- [ ] Fixture: genomics-bwa (5 files, 2–5 GiB, binary)
- [ ] Fixture: genomics-star (8–12 files, ~40 GiB, binary)
- [ ] Fixture: genomics-mixed (large binaries + small .dict/.fai/JSON)
- [ ] 12-metric runner (M-01 ~ M-12)
- [ ] 8 pass criteria enforcement
- [ ] JSON result at `docs/bench/YYYY-MM-DD-<fixture>.json`
- [ ] Gate: synthetic-1/10/50GiB all pass
- [ ] Gate: genomics-bwa + genomics-star pass (40 GB scale verified)

---

## P5-7: Registry Compatibility + Tag v0.6

**Status**: ⏳ pending (depends on P5-5 + P5-6)  
**Target files**:
- `chunked/integration_test.go` (new, `//go:build integration`)

**Checklist**:
- [ ] Local OCI layout: full round-trip (push+fetch+dedup) — **V1 blocker**
- [ ] Harbor: push+fetch+dedup round-trip — **V1 blocker**
- [ ] GHCR: push+fetch+dedup round-trip — **V1 blocker**
- [ ] ECR: verify MaxChunkedLayers=900 sufficient (empirical, OQ-1 close-out)
- [ ] `git tag v0.6.0-experimental`

---

## Dependency Map

```
P5-0 ──► P5-1 ──► P5-2 ──► P5-3 ──────────► P5-5 ──► P5-7
                       └──► P5-4 ──────────► P5-5
                       └──────────────────── P5-6 ──► P5-7
                                             (parallel with P5-5)
```

P5-3 and P5-4 run in parallel after P5-2.  
P5-5 and P5-6 run in parallel after P5-4 (P5-6 also needs P5-3).

---

## Session Resume Checklist

On next session start:
1. Read this file to determine current sprint and status.
2. Check which items are ✅ vs ⏳ in the active sprint.
3. Resume from the first unchecked item in the active sprint.
4. Do not re-implement items already marked ✅.
