# v1 Test Plan

`v1.0.0` requires more than a smoke test. Smoke verifies that the first user
path works. The v1 test plan verifies registry compatibility, real dataset
behavior, scale characteristics, failure handling, and release asset usability.

## Test Layers

| Layer | Purpose | Trigger | Blocks v1 |
|---|---|---|---|
| Unit / short | Fast correctness, typed errors, safe local behavior | `make test` | yes |
| Coverage | Ensure important packages stay covered | `make coverage` | yes |
| Vulnerability | Check reachable known vulnerabilities | `make vuln` | yes |
| CLI smoke | First user path against a real registry | `make smoke-cli` | yes |
| Registry integration | Library API push/resolve/referrer behavior | `make test-registry-integration` | yes |
| Real dataset | Realistic genomics directory through CLI | `make test-real-dataset` | yes |
| Scale / performance | Large data and memory behavior | bench targets / recorded results | yes for regressions |
| Reliability / failure | Corrupt data, cancel, rollback, retry behavior | local tests + targeted integration | yes |
| Release acceptance | Downloaded release binary works, checksum verified | manual/CI release run | yes |

## Smoke Test

Smoke is the first gate, not the final gate.

Pass criteria:

- `metadata init -> push -> inspect -> fetch` succeeds.
- `inspect` reports `chunked-cas`.
- `inspect` reports metadata attached and valid.
- fetched data is byte-identical to the source fixture.
- `volume-index.json` is written.
- `.sori/dataset-metadata.json` is written.
- `--overwrite --skip-if-current` skips an already-current destination.

Run:

```bash
make build-sorictl
SORI_SMOKE_REF=ghcr.io/OWNER/references:sori-small-smoke-v1 \
SORI_REGISTRY_USERNAME=USER \
SORI_REGISTRY_TOKEN=TOKEN \
make smoke-cli
```

For lab Harbor over HTTP:

```bash
SORI_SMOKE_REF=harbor.example.com/project/references:sori-small-smoke-v1 \
SORI_REGISTRY_USERNAME=USER \
SORI_REGISTRY_TOKEN=TOKEN \
SORI_SMOKE_EXTRA_FLAGS="--plain-http" \
make smoke-cli
```

## Registry Integration

Integration tests exercise the Go library registry path. They are env-gated and
skipped by default.

Pass criteria:

- package to temporary local OCI store.
- push to remote registry.
- resolve pushed tag.
- resolved digest equals push result digest.
- referrer path succeeds where enabled.

Run:

```bash
env \
  SORI_RUN_REGISTRY_INTEGRATION=1 \
  SORI_REGISTRY_HOST=harbor.example.com \
  SORI_REGISTRY_REPOSITORY=project/dataset \
  SORI_REGISTRY_USERNAME=USER \
  SORI_REGISTRY_TOKEN=TOKEN \
  make test-registry-integration
```

## Real Dataset Test

The real dataset test uses the same CLI flow as smoke, but the source directory
is a realistic genomics fixture.

Recommended size for v1 acceptance:

- Small enough to run repeatedly: tens to hundreds of MiB.
- Real enough to catch layout issues: nested files, FASTA/GTF/index-like files,
  and more than one file.
- Not a full benchmark workload.

Run:

```bash
make build-sorictl
SORI_SMOKE_REF=ghcr.io/OWNER/references:sori-real-smoke-v1 \
SORI_REAL_DATASET_DIR=/path/to/genomics-fixture \
SORI_REGISTRY_USERNAME=USER \
SORI_REGISTRY_TOKEN=TOKEN \
make test-real-dataset
```

Pass criteria:

- Push, inspect, fetch pass.
- fetched directory is byte-identical to `SORI_REAL_DATASET_DIR`.
- Metadata remains valid.
- The registry-specific result is recorded in [v1-readiness.md](v1-readiness.md)
  or [registry-integration.md](registry-integration.md).

## Scale / Performance

Scale tests are not smoke tests. They validate chunked CAS behavior under large
data.

Track:

- source size.
- file count.
- chunk count.
- push time.
- fetch time.
- peak RSS.
- chunk reuse count.
- registry used.

Existing benchmark artifacts live under [bench](bench).

## Reliability / Failure

The local test suite already covers many failure paths:

- corrupt layers and chunk digest mismatch.
- staging cleanup.
- atomic overwrite rollback.
- context cancellation.
- path traversal and symlink rejection.
- stale or missing `volume-index.json` for `skip-if-current`.

Before v1, run the full local gate:

```bash
make lint
make test
make coverage
make vuln
```

## Release Acceptance

Release acceptance validates the artifact users will actually download.

Pass criteria:

- GitHub Release exists for the release candidate tag.
- Release is marked prerelease for `v*-rc*` tags.
- Assets exist:
  - `sorictl_linux_amd64`
  - `sorictl_linux_arm64`
  - `sorictl_darwin_amd64`
  - `sorictl_darwin_arm64`
  - `sorictl_windows_amd64.exe`
  - `checksums.txt`
- checksum verification passes.
- downloaded `sorictl` can run `help`.
- downloaded `sorictl` passes GHCR and Harbor smoke.

## v1 Exit Rule

`v1.0.0` should not be tagged from smoke alone. The minimum release decision is:

```text
local gate
  + CLI smoke on GHCR and Harbor
  + registry integration on at least Harbor or GHCR
  + real dataset CLI test on GHCR and Harbor
  + release candidate asset acceptance
```
