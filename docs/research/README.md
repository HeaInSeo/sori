# sori Research Documentation

sori is a Go library for packaging and distributing large genomics reference
datasets — STAR indices, BWA indices, reference FASTA files, and similar
large binary artifacts — via OCI-compatible registries. The core technical
contribution is the **chunked CAS artifact format** (P5), which replaces
single-layer tar.gz packaging with fixed-size raw-byte chunks stored as
individual OCI layers. This eliminates the full-artifact temp-file disk spike,
enables chunk-level deduplication via OCI content-addressing, and provides
three-layer integrity verification (chunk, file, and tree) without changing
the underlying OCI registry infrastructure.

---

## Document Index

| File | Description |
|---|---|
| [problem.md](problem.md) | Scale problem, legacy format limitations, and why OCI registries are the right distribution primitive |
| [architecture.md](architecture.md) | Three-layer design, key design decisions, push/fetch flows, and integrity policy |
| [benchmark-plan.md](benchmark-plan.md) | How to run and reproduce benchmarks; environment setup, commands, result schema |
| [results-summary.md](results-summary.md) | Benchmark results placeholder; table template and current status |
| [limitations.md](limitations.md) | Known V1 limitations, scope boundaries, measurement caveats, registry coverage |
| [related-work-notes.md](related-work-notes.md) | Comparison with Refgenie, CVMFS, DataLad, Nextflow Fusion, ORAS, Singularity, DVC |

---

## Reproducing Benchmarks

**Prerequisites**: Go 1.24+, approximately 100 GiB of free disk for full-scale
runs, a writable directory for the OCI store.

### Small-scale smoke test (runs in CI, approximately 10 seconds)

```
go test -tags bench -run TestGate_SmallScale ./internal/bench/... -v
```

All 7 fixtures run at 0.001x scale. No special environment is required.

### Full-scale benchmarks (manual, per fixture)

```
go test -tags bench -bench=BenchmarkGate_Synthetic1GiB -benchtime=1x ./internal/bench/... -v
go test -tags bench -bench=BenchmarkGate_GenomicsSTAR -benchtime=1x ./internal/bench/... -v
```

Results are written to `docs/bench/YYYY-MM-DD-<fixture>.json`.

---

## Citation

> [Citation placeholder — to be filled in after submission.]
>
> sori: A Go library for chunked CAS distribution of large genomics reference
> datasets via OCI registries.
> HeaInSeo / sori, https://github.com/HeaInSeo/sori
