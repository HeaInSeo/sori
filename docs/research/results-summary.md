# Benchmark Results Summary

## How to Populate This File

1. Run the full-scale benchmarks as described in [benchmark-plan.md](benchmark-plan.md).
2. Locate the result JSON files in `docs/bench/YYYY-MM-DD-<fixture>.json`.
3. Copy the 12 metric values from each result into the table templates below.
4. Record the reproducibility checklist fields (commit hash, Go version, OS,
   hardware) in the notes column.

---

## Current Status

- **Small-scale smoke tests**: all 7 fixtures pass at 0.001x scale as of
  2026-05-30 (B-3 pre-condition satisfied).
- **Full-scale results**: pending. Manual runs required for all 7 fixtures at
  1x scale before V1 gate (B-3) can be closed.

Full-scale results are required for:
- V1 release gate B-3
- Any paper submission to JOSS / Genomics & Informatics / BMC Bioinformatics

---

## Results Table Template

Replace each `—` with the measured value. Add a row for each completed run.
The `path` column is `chunked` (chunked CAS) or `legacy` (legacy tar.gz, once
implemented).

### synthetic-1GiB

| Date | Commit | Path | M-01 firstPush (s) | M-02 secondPush (s) | M-03 partialUpdate (s) | M-05 peakRSS (MiB) | M-06 peakTempDisk (MiB) | M-07 sourceBytesRead | M-08 uploadedBytes | M-09 blobsCreated | M-10 fetch (s) | M-11 fetchPeakDisk (MiB) | M-12 treeVerify (ms) | passed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| — | — | chunked | — | — | — | — | — | — | — | — | — | — | — | — |
| — | — | legacy | — | — | — | — | — | — | — | — | — | — | — | — |

### synthetic-10GiB

| Date | Commit | Path | M-01 firstPush (s) | M-02 secondPush (s) | M-03 partialUpdate (s) | M-05 peakRSS (MiB) | M-06 peakTempDisk (MiB) | M-07 sourceBytesRead | M-08 uploadedBytes | M-09 blobsCreated | M-10 fetch (s) | M-11 fetchPeakDisk (MiB) | M-12 treeVerify (ms) | passed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| — | — | chunked | — | — | — | — | — | — | — | — | — | — | — | — |
| — | — | legacy | — | — | — | — | — | — | — | — | — | — | — | — |

### synthetic-50GiB

| Date | Commit | Path | M-01 firstPush (s) | M-02 secondPush (s) | M-03 partialUpdate (s) | M-05 peakRSS (MiB) | M-06 peakTempDisk (MiB) | M-07 sourceBytesRead | M-08 uploadedBytes | M-09 blobsCreated | M-10 fetch (s) | M-11 fetchPeakDisk (MiB) | M-12 treeVerify (ms) | passed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| — | — | chunked | — | — | — | — | — | — | — | — | — | — | — | — |
| — | — | legacy | — | — | — | — | — | — | — | — | — | — | — | — |

### genomics-fasta

| Date | Commit | Path | M-01 firstPush (s) | M-02 secondPush (s) | M-03 partialUpdate (s) | M-05 peakRSS (MiB) | M-06 peakTempDisk (MiB) | M-07 sourceBytesRead | M-08 uploadedBytes | M-09 blobsCreated | M-10 fetch (s) | M-11 fetchPeakDisk (MiB) | M-12 treeVerify (ms) | passed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| — | — | chunked | — | — | — | — | — | — | — | — | — | — | — | — |
| — | — | legacy | — | — | — | — | — | — | — | — | — | — | — | — |

### genomics-bwa

| Date | Commit | Path | M-01 firstPush (s) | M-02 secondPush (s) | M-03 partialUpdate (s) | M-05 peakRSS (MiB) | M-06 peakTempDisk (MiB) | M-07 sourceBytesRead | M-08 uploadedBytes | M-09 blobsCreated | M-10 fetch (s) | M-11 fetchPeakDisk (MiB) | M-12 treeVerify (ms) | passed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| — | — | chunked | — | — | — | — | — | — | — | — | — | — | — | — |
| — | — | legacy | — | — | — | — | — | — | — | — | — | — | — | — |

### genomics-star

| Date | Commit | Path | M-01 firstPush (s) | M-02 secondPush (s) | M-03 partialUpdate (s) | M-05 peakRSS (MiB) | M-06 peakTempDisk (MiB) | M-07 sourceBytesRead | M-08 uploadedBytes | M-09 blobsCreated | M-10 fetch (s) | M-11 fetchPeakDisk (MiB) | M-12 treeVerify (ms) | passed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| — | — | chunked | — | — | — | — | — | — | — | — | — | — | — | — |
| — | — | legacy | — | — | — | — | — | — | — | — | — | — | — | — |

### genomics-mixed

| Date | Commit | Path | M-01 firstPush (s) | M-02 secondPush (s) | M-03 partialUpdate (s) | M-05 peakRSS (MiB) | M-06 peakTempDisk (MiB) | M-07 sourceBytesRead | M-08 uploadedBytes | M-09 blobsCreated | M-10 fetch (s) | M-11 fetchPeakDisk (MiB) | M-12 treeVerify (ms) | passed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| — | — | chunked | — | — | — | — | — | — | — | — | — | — | — | — |
| — | — | legacy | — | — | — | — | — | — | — | — | — | — | — | — |

---

## Notes

- The `legacy` rows in the tables above require the legacy benchmark path to be
  implemented (tracked as a future item in the benchmark runner).
- `M-04 retryPushSeconds` is omitted from the table because it is `0.0` in all
  current runs (retry simulation requires a mock 5xx server; deferred to
  integration).
- Full-scale results for `synthetic-50GiB` and `genomics-star` require
  approximately 100 GiB of free disk and are not run in CI.
