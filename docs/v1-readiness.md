# Sori v1.0.0 Readiness Gate

이 문서는 `sori`를 `v1.0.0`으로 태그하기 전에 닫아야 하는 항목을
정의한다. 목적은 새 기능을 더 넣는 것이 아니라, 라이브러리 계약과 CLI UX,
registry 운영 신뢰성을 고정하는 것이다.

## Release Decision

`v1.0.0`을 찍으려면 아래 조건을 모두 만족해야 한다.

- Stable API 계약이 문서화되어 있고, v1 이후 patch/minor에서 불필요한
  breaking change를 하지 않겠다는 기준이 명확하다.
- [dataset-metadata-v1.md](dataset-metadata-v1.md)에
  `dataset-metadata.json` schema와 OCI media type의 역할이 고정되어 있다.
- `sorictl`의 기본 명령과 JSON/text output이 초보자와 자동화 사용자 모두에게
  충분히 예측 가능하다. 기준은 [sorictl-contract.md](sorictl-contract.md)이다.
- GHCR과 Harbor에서 작은 fixture와 실제 genomics fixture를 push, inspect,
  fetch까지 검증했다.
- [v1-test-plan.md](v1-test-plan.md)의 smoke, integration, real dataset,
  release acceptance 기준을 통과했다.
- `make lint`, `make test`, `make coverage`, `make vuln`이 통과한다.
- GitHub Actions `Lint`, `Tests`, `Release`가 green이다.
- GitHub Release asset과 checksum이 정상 생성된다.

## Current Status

현재 `v0.8.0-rc4`까지 다음 항목은 완료되어 있다.

- `sorictl` registry-neutral CLI 기본 흐름
  - `metadata init`
  - `dataset push`
  - `dataset inspect`
  - `dataset fetch`
- GHCR / Harbor 수동 smoke를 위한 CLI와 auth UX
- chunked CAS packaging/fetch 경로
- fetch atomic overwrite와 `--skip-if-current`
- dataset metadata attachment and inspection
- GitHub Release asset 자동화
- RC tag를 prerelease로 표시하는 release workflow
- 초보자용 quickstart

남은 항목은 아래 checklist를 기준으로 닫는다.

## Checklist

### 0. Test Layer Gate

Status: open

Required before `v1.0.0-rc1`:

- [x] v1 테스트 계층이 [v1-test-plan.md](v1-test-plan.md)에 문서화되어 있다.
- [x] CLI smoke를 반복 실행할 수 있는 `make smoke-cli` 타깃이 있다.
- [x] real dataset CLI test를 반복 실행할 수 있는 `make test-real-dataset`
      타깃이 있다.
- [x] registry integration을 반복 실행할 수 있는 `make test-registry-integration`
      타깃이 있다.
- [ ] GHCR smoke result가 기록되어 있다.
- [ ] Harbor smoke result가 기록되어 있다.
- [ ] GHCR real dataset result가 기록되어 있다.
- [ ] Harbor real dataset result가 기록되어 있다.
- [ ] release binary acceptance result가 기록되어 있다.

### 1. Stable API Freeze

Status: mostly complete

Required before `v1.0.0-rc1`:

- [ ] [public-api.md](public-api.md)의 Stable API 목록을 최종 검토한다.
- [ ] `PackageOptions`, `PushOptions`, `FetchOptions`, `RemoteTarget` 필드가
      v1 이후에도 의미를 유지할 수 있는지 확인한다.
- [ ] `PackageResult`, `PushResult`, `VolumeIndex`, `Partition`의 JSON/public field
      의미를 고정한다.
- [ ] `ArtifactFormatLegacy`와 `ArtifactFormatChunkedCAS`의 지원 정책을 문서화한다.
- [ ] Experimental API가 v1 compatibility promise에 포함되지 않는다고 README와
      public API 문서에 명시한다.

Exit criteria:

- Stable API 목록에 있는 함수와 타입은 v1 이후 patch/minor에서 삭제하거나
  시그니처를 바꾸지 않는다.
- Experimental API는 사용할 수 있지만 v1 안정 계약이 아님을 명확히 표시한다.

### 2. Dataset Metadata Contract

Status: open

Required before `v1.0.0-rc1`:

- [x] `sori.dataset.metadata.v1`의 필수 필드를 고정한다.
      현재 필수 필드는 `schemaVersion`, `kind`, `displayName`, `description`이다.
- [x] 선택 필드의 의미를 문서화한다.
      예: `organism`, `reference`, `dataTypes`, `fileFormats`,
      `compatibleTools`, `source`, `validationStatus`.
- [x] unknown JSON fields를 허용하는 현재 Go decoder 동작을 v1 정책으로 유지할지
      결정한다.
- [x] metadata가 SBOM이 아니라 dataset identity/catalog metadata라는 점을 명시한다.
- [x] CLI-generated metadata와 library validation이 같은 schema를 기준으로
      동작하는지 테스트한다.

Exit criteria:

- 사람이 읽는 설명과 시스템이 읽는 식별/호환성 필드가 분리되어 설명되어 있다.
- `ValidateDatasetMetadataJSON`의 v1 동작이 문서와 일치한다.

### 3. CLI Contract

Status: mostly complete

Required before `v1.0.0-rc1`:

- [x] v1 CLI 명령으로 아래 4개를 고정한다.
  - `sorictl metadata init`
  - `sorictl dataset push`
  - `sorictl dataset inspect`
  - `sorictl dataset fetch`
- [x] `--ref REGISTRY/REPOSITORY:TAG`를 기본 beginner path로 유지한다.
- [x] `--output json` 구조를 자동화 사용자가 파싱 가능한 계약으로 검토한다.
- [x] text output은 사람이 읽기 쉬운 상태로 유지하되, machine parsing 대상으로
      권장하지 않는다.
- [ ] auth hint와 error message가 GHCR/Harbor 초보자에게 충분히 직접적인지
      확인한다.

Exit criteria:

- Quickstart의 명령을 그대로 실행하면 작은 fixture를 push, inspect, fetch할 수 있다.
- JSON output schema 변경은 v1 이후 patch/minor에서 신중히 다룬다.

### 4. Registry Compatibility

Status: open

Required before `v1.0.0-rc1`:

- [ ] GHCR: small fixture push, inspect, fetch.
- [ ] GHCR: genomics-sized fixture push, inspect, fetch.
- [ ] Harbor: small fixture push, inspect, fetch.
- [ ] Harbor: genomics-sized fixture push, inspect, fetch.
- [ ] HTTP-only lab registry behavior with `--plain-http`.
- [ ] private CA behavior with `--ca-file`.

Recommended compatibility table:

| Registry | Auth | TLS mode | Small fixture | Genomics fixture | Notes |
|---|---|---|---|---|---|
| GHCR | username + PAT | HTTPS | pending | pending | public/private visibility policy 확인 |
| Harbor | username + password/token | HTTPS or lab HTTP | pending | pending | project/repository path 확인 |
| local registry | none/basic | HTTP | optional | optional | CI smoke 후보 |

Exit criteria:

- GHCR and Harbor both pass the same user-facing CLI flow.
- Registry-specific caveats are documented in [registry-integration.md](registry-integration.md)
  or [sorictl-quickstart.md](sorictl-quickstart.md).

### 5. Release and CI

Status: mostly complete

Required before `v1.0.0`:

- [x] Tag-triggered release workflow exists.
- [x] Release assets include Linux, macOS, Windows `sorictl` binaries.
- [x] `checksums.txt` is uploaded.
- [x] RC tags are marked as prerelease.
- [ ] `v1.0.0-rc1` release is created from the final readiness branch.
- [ ] `v1.0.0` release is created only after RC smoke passes.
- [ ] Final README version banner is updated from RC to `v1.0.0`.

Required local checks:

```bash
make lint
make test
make coverage
make vuln
make release-dist
```

Required GitHub checks:

- `Lint` success on `master`.
- `Tests` success on `master`.
- `Lint`, `Tests`, `Release` success on the release candidate tag.

### 6. Documentation

Status: open

Required before `v1.0.0-rc1`:

- [ ] README separates library usage from CLI usage.
- [ ] README version banner reflects the current release candidate.
- [ ] `docs/public-api.md` names the v1 stable contract.
- [x] `docs/dataset-metadata-v1.md` documents the metadata schema contract.
- [x] `docs/sorictl-contract.md` documents the v1 CLI contract.
- [ ] `docs/sorictl-quickstart.md` is checked against a real registry run.
- [ ] `docs/operations.md` includes v1 production guidance for auth, TLS,
      cache/store location, fetch safety, and metadata requirements.

Exit criteria:

- A beginner can publish and fetch a small reference by following the quickstart.
- A library user can identify the stable API without reading implementation code.

## Suggested Sequence

1. Freeze public API and metadata schema docs.
2. Run GHCR and Harbor small-fixture smoke with `make smoke-cli`.
3. Run registry integration with `make test-registry-integration`.
4. Run GHCR and Harbor real-dataset tests with `make test-real-dataset`.
5. Update registry compatibility notes with exact results.
6. Create `v1.0.0-rc1`.
7. Verify GitHub Actions and Release assets for `v1.0.0-rc1`.
8. Run release binary acceptance against GHCR and Harbor.
9. If no blocking UX or compatibility issues remain, create `v1.0.0`.

## Non-goals for v1.0.0

These should not block `v1.0.0` unless they expose a correctness or safety issue:

- Moving all NodeVault-oriented experimental APIs to a separate module.
- Adding a full registry test harness that starts Harbor/zot/distribution in CI.
- Supporting every registry implementation.
- Expanding metadata into a full SBOM model.
- Adding new CLI subcommands beyond the four v1 candidate commands.
