# v0.7.0 Stabilization Sprint

**RFC**: `p5-rfc.md` (v6) — same design basis  
**From**: v0.6.0-experimental  
**Target**: v0.7.0-stable  
**Started**: 2026-05-30  

Legend: ✅ done · 🔄 in-progress · ⏳ pending · ❌ blocked

---

## V1.0 Release Blockers

| # | Condition | Sprint | Status |
|---|---|---|---|
| B-1 | M-12 post-fetch tree verification in chunked.Fetch | P7-1 | ⏳ |
| B-2 | PackageVolumeToStore dispatches to chunked.Publish on ArtifactFormatChunkedCAS | P7-2 | ⏳ |
| B-3 | FetchVolSeq / FetchVolParallel auto-detect chunked-cas manifests | P7-2 | ⏳ |
| B-4 | ArtifactFormatChunkedCAS not marked experimental in comments | P7-3 | ⏳ |
| B-5 | ECR MaxLayers empirical (OQ-1) | deferred | ⏳ AWS creds required |

**Optional (not blocking)**:

| # | Condition | Sprint | Status |
|---|---|---|---|
| O-1 | Full-scale 1× benchmark (synthetic-1/10GiB, genomics-bwa, genomics-star) | P7-4 | ⏳ ~60 GiB disk required |

---

## P7-1: M-12 Post-fetch Tree Verification

**Status**: ⏳ pending  
**Target files**:
- `chunked/fetch.go` — `FetchOptions.VerifyTree bool` + post-fetch re-walk
- `chunked/fetch_test.go` — tree verify tests

**Background**: `chunked.Fetch` already verifies each file's sha256 during reconstruction
(per-file pass). M-12 requires an additional post-fetch re-walk after all files are
written, verifying the assembled tree is intact end-to-end. This detects FS-level
corruption introduced between chunk write and final directory state.

**API change**:
```go
type FetchOptions struct {
    Progress   ProgressFunc
    VerifyTree bool  // re-walk destRoot after fetch and sha256-verify each file; default false
}
```

**Checklist**:
- [ ] `FetchOptions.VerifyTree bool` — zero value = false (backward compatible)
- [ ] Post-fetch pass: after `g.Wait()`, walk idx.Files, re-open each file, sha256, compare to `idxFile.Digest`
- [ ] Return `ErrIntegrity` wrapping file path on mismatch
- [ ] Elapsed time recorded (report via ArtifactDone or separate event) for M-12 measurement
- [ ] `go test ./chunked/...` green
- [ ] Test: `VerifyTree=true`, clean round-trip → no error
- [ ] Test: `VerifyTree=true`, corrupt file after fetch (os.WriteFile into destRoot) → ErrIntegrity

---

## P7-2: Dual-Path Integration (Main sori API)

**Status**: ⏳ pending  
**Target files**:
- `volume-index.go` — `packageVolumeToStoreWithOptions`: branch on `ArtifactFormatChunkedCAS`
- `volume_publish_fetch.go` — `fetchVolSeqFrom` / `fetchVolParallelFrom`: manifest detect → dispatch

### Push side

`packageVolumeToStoreWithOptions` currently always takes the legacy tar.gz path.
When `opts.Format == ArtifactFormatChunkedCAS`:

1. Build `chunked.PublishOptions{ChunkSize, ConfigBlob: configBlob, Progress: opts.Progress}`
2. Call `chunked.Publish(ctx, localStorePath, req.SourceDir, req.Tag, chunkedOpts)`
3. Return `&PackageResult{VolumeIndex: &VolumeIndex{VolumeRef: manifestDesc.Digest.String()}}`
   — Partitions is nil (no partition concept in chunked CAS; chunk-index.json is the file map)

`req.DatasetMetadata` (if we add it) → `chunkedOpts.DatasetMetadata` (future; not needed for B-2).

### Fetch side

`fetchVolSeqFrom` and `fetchVolParallelFrom` both open an OCI store and pull layers.
Insert dual-path detection before existing layer-walk logic:

```
resolve tag → manifest → switch manifest.Config.MediaType:
  case MediaTypeLegacyConfig:  → existing legacy path (no change)
  case chunked.MediaTypeConfig: → chunked.Fetch(ctx, storePath, destRoot, tag, opts)
                                   return &VolumeIndex{VolumeRef: manifestDesc.Digest.String()}, nil
  default: → ErrValidation (unknown format)
```

Import `"github.com/HeaInSeo/sori/chunked"` in `volume_publish_fetch.go`.

**Checklist**:
- [ ] `packageVolumeToStoreWithOptions`: `ArtifactFormatChunkedCAS` branch → `chunked.Publish`
- [ ] Returns `PackageResult` with `VolumeRef = manifestDesc.Digest.String()`
- [ ] `fetchVolSeqFrom`: manifest detect + chunked dispatch
- [ ] `fetchVolParallelFrom`: manifest detect + chunked dispatch
- [ ] Test: `PackageVolumeToStore` with `opts.Format=ArtifactFormatChunkedCAS` → chunked manifest (verify Config.MediaType)
- [ ] Test: `FetchVolSeq` on chunked artifact → byte-identical files in destRoot
- [ ] Test: `FetchVolParallel` on chunked artifact → byte-identical files in destRoot
- [ ] Regression test: `FetchVolSeq` on legacy artifact still works
- [ ] `go test ./...` green

---

## P7-3: API Stabilization

**Status**: ⏳ pending  
**Target files**:
- `options.go`
- `chunked/validate.go`, `chunked/types.go`
- `p5-rfc.md`

**Checklist**:
- [ ] `options.go`: `ArtifactFormatChunkedCAS` — remove "is experimental"
- [ ] `chunked/publish.go`: `PublishOptions.ChunkSize` doc — "zero defaults to DefaultChunkSize (1 GiB)"
- [ ] `chunked/fetch.go`: `FetchOptions.VerifyTree` doc (carried from P7-1)
- [ ] `go vet ./...` clean
- [ ] `staticcheck ./...` clean (or document known suppressions)
- [ ] `go test ./...` green
- [ ] `p5-rfc.md` status line: `Draft (v6 ...)` → `Final (v6)`
- [ ] `git tag v0.7.0-stable` after all blockers closed

---

## P7-4: Full-Scale Benchmark Evidence (Hardware-Gated)

**Status**: ⏳ pending — requires ~60 GiB free disk  
**Target files**: `docs/bench/`, `docs/research/results-summary.md`

Run on a machine with sufficient disk (e.g., k8s node or dedicated bench host).
Results land in `docs/bench/YYYY-MM-DD-<fixture>.json` via `WriteResult`.

```bash
# On the bench machine (ensure ~60 GiB free):
go test -tags bench -bench=BenchmarkGate_Synthetic1GiB  -benchtime=1x ./internal/bench/... -v
go test -tags bench -bench=BenchmarkGate_Synthetic10GiB -benchtime=1x ./internal/bench/... -v
go test -tags bench -bench=BenchmarkGate_GenomicsBWA    -benchtime=1x ./internal/bench/... -v
go test -tags bench -bench=BenchmarkGate_GenomicsSTAR   -benchtime=1x ./internal/bench/... -v
```

After each run, copy the JSON result to `docs/bench/` and fill in
`docs/research/results-summary.md`.

**Checklist**:
- [ ] synthetic-1GiB passes at 1× scale
- [ ] synthetic-10GiB passes at 1× scale
- [ ] genomics-bwa passes at 1× scale
- [ ] genomics-star passes at 1× scale (~40 GiB fixture)
- [ ] `docs/research/results-summary.md` filled with actual M-01~M-12 numbers
- [ ] Legacy vs chunked CAS comparison column populated

---

## Dependency Map

```
P7-1 ──────────────────────► P7-3 ──► tag v0.7.0-stable
P7-2 ──────────────────────► P7-3
P7-4 (optional, parallel)
```

P7-1 and P7-2 are independent and can run in parallel.  
P7-3 (stabilization + tag) runs after both P7-1 and P7-2 are complete.  
P7-4 is hardware-gated and does not block the tag.

---

## Session Resume Checklist

On next session start:
1. Read this file to determine current sprint and status.
2. Read `p5-sprint.md` for carry-over context from P5.
3. Resume from the first unchecked blocker.
4. Do not re-implement items already marked ✅.
