# Benchmark Plan

## Prerequisites

- Go 1.24 or later
- Approximately 100 GiB of free disk space for full-scale runs (synthetic-50GiB
  fixture requires ~50 GiB source + ~50 GiB destination + OCI store blobs)
- A writable directory for the OCI store (defaults to a temp dir created by the
  test runner; override via the `OCI_STORE_PATH` environment variable if
  needed)
- No live registry required for unit-level benchmarks — all benchmarks use a
  local OCI layout store

---

## Environment Variables

| Variable | Purpose | Default |
|---|---|---|
| `GOROOT` | Go installation root; set if not on `PATH` | System default |
| `GOTOOLCHAIN` | Pin Go toolchain version (e.g. `go1.24.0`) | System default |
| `OCI_STORE_PATH` | Override OCI store directory for benchmark runs | OS temp dir |

---

## Small-Scale Smoke Test

Runs all 7 fixtures at 0.001x scale. Completes in approximately 10 seconds.
Suitable for CI. No special environment or large disk is required.

```
go test -tags bench -run TestGate_SmallScale ./internal/bench/... -v
```

All 7 fixtures must pass the gate criteria at 0.001x scale before any
full-scale run is attempted. This is the B-3 pre-condition.

---

## Full-Scale Benchmarks

Full-scale runs require manual execution. Each command runs one fixture at 1x
scale and writes the result to `docs/bench/YYYY-MM-DD-<fixture>.json`.

```
go test -tags bench -bench=BenchmarkGate_Synthetic1GiB -benchtime=1x ./internal/bench/... -v
go test -tags bench -bench=BenchmarkGate_GenomicsSTAR -benchtime=1x ./internal/bench/... -v
```

Available benchmark names:

| Benchmark name | Fixture | Approximate size |
|---|---|---|
| `BenchmarkGate_Synthetic1GiB` | synthetic-1GiB | 1 GiB |
| `BenchmarkGate_Synthetic10GiB` | synthetic-10GiB | 10 GiB |
| `BenchmarkGate_Synthetic50GiB` | synthetic-50GiB | 50 GiB |
| `BenchmarkGate_GenomicsFasta` | genomics-fasta | ~3 GiB |
| `BenchmarkGate_GenomicsBwa` | genomics-bwa | ~15 GiB |
| `BenchmarkGate_GenomicsSTAR` | genomics-star | ~40 GiB |
| `BenchmarkGate_GenomicsMixed` | genomics-mixed | ~8 GiB |

---

## Result Location and Format

Each gate run writes one JSON file to `docs/bench/`:

```
docs/bench/YYYY-MM-DD-<fixture>.json
```

Example: `docs/bench/2026-05-30-synthetic-10GiB.json`

### Result JSON Schema

All 12 fields from the `Metrics` struct are present in every result:

```json
{
  "date": "2026-05-30",
  "soriVersion": "v0.6.0-dev",
  "fixture": "synthetic-10GiB",
  "chunkSizeBytes": 1073741824,
  "uploadConcurrency": 2,
  "metrics": {
    "firstPushSeconds":          42.1,
    "secondPushSeconds":          1.3,
    "partialUpdatePushSeconds":  12.7,
    "retryPushSeconds":          14.2,
    "pushPeakRSSMiB":            48.3,
    "pushPeakTempDiskMiB":        0.1,
    "sourceBytesRead":    10737418240,
    "uploadedBytes":      10737418240,
    "blobsCreated":               10,
    "fetchSeconds":              38.6,
    "fetchPeakDiskMiB":        9960.0,
    "treeVerifyMs":              210
  },
  "passed": true,
  "violations": []
}
```

| Field | Metric ID | Unit | Notes |
|---|---|---|---|
| `firstPushSeconds` | M-01 | seconds | Wall-clock time for first push |
| `secondPushSeconds` | M-02 | seconds | Second push (all chunks skipped) |
| `partialUpdatePushSeconds` | M-03 | seconds | Push after modifying first file |
| `retryPushSeconds` | M-04 | seconds | 0.0 if not exercised in this run |
| `pushPeakRSSMiB` | M-05 | MiB | Sampled via `runtime.MemStats` (HeapInuse + StackInuse) |
| `pushPeakTempDiskMiB` | M-06 | MiB | Directory-size sampling; 0.0 expected for chunked CAS |
| `sourceBytesRead` | M-07 | bytes | Total source fixture size |
| `uploadedBytes` | M-08 | bytes | Bytes reported by `ChunkUploaded` progress events |
| `blobsCreated` | M-09 | count | `len(manifest.Layers) + 1` (config blob) |
| `fetchSeconds` | M-10 | seconds | Wall-clock time for full fetch |
| `fetchPeakDiskMiB` | M-11 | MiB | Peak destination directory size during fetch |
| `treeVerifyMs` | M-12 | milliseconds | Time to re-walk and verify all file digests |

---

## Interpretation Guide

The following metrics are the primary gate criteria for V1:

- **M-06 `pushPeakTempDiskMiB`**: must remain near zero (below 10 MiB) for all
  fixtures. The chunked CAS path creates no full-artifact temp file during push.
  A value approaching the fixture size indicates a regression to the legacy
  behaviour.

- **M-05 `pushPeakRSSMiB`**: must stay below 256 MiB at `uploadConcurrency=2`
  for all fixtures, and must be independent of chunk size. If this value grows
  proportionally with fixture size, a per-chunk buffer allocation is leaking
  into the push path.

- **M-08 `uploadedBytes` on second push**: must be zero (or limited to manifest
  and metadata blob sizes). Any non-zero chunk bytes on the second push indicate
  a regression in the Exists() dedup check.

- **M-12 `treeVerifyMs`**: expected to grow linearly with total dataset size.
  Very high values may indicate a file-system or I/O bottleneck in the
  destination, not a sori bug.

---

## Legacy Comparison

The legacy tar.gz benchmark path is not yet implemented in V1. When
`ArtifactFormatLegacy` benchmarks are added, they will be invoked with:

```
go test -tags bench -bench=BenchmarkGateLegacy_<fixture> -benchtime=1x ./internal/bench/... -v
```

The expected comparison values (from the p5-rfc.md problem analysis):

| Metric | Legacy expected | Chunked CAS expected |
|---|---|---|
| `pushPeakTempDiskMiB` | ~= fixture size (GiB x 1024) | < 10 MiB |
| `uploadedBytes` on second push | ~= fixture size | ~0 (chunk bytes) |
| `pushPeakRSSMiB` | scales with compress buffer | <256 MiB, size-independent |

---

## Reproducibility Checklist

Every full-scale benchmark result submitted for publication or the V1 gate must
record:

- [ ] Git commit hash (`git rev-parse HEAD`)
- [ ] Go version (`go version`)
- [ ] OS and kernel version (`uname -a`)
- [ ] Fixture name and scale factor (always 1.0 for gate results)
- [ ] `chunkSizeBytes` (default: 1073741824)
- [ ] `uploadConcurrency` (default: 2)
- [ ] OCI store backend (local OCI layout for all gate runs)
- [ ] Machine hardware summary (CPU model, RAM, storage type)

The `date` and `soriVersion` fields in the result JSON are written automatically
by the benchmark runner.
