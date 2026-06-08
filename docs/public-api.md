# Sori Public API Status

이 문서는 `sori`의 public API를 안정도 기준으로 분류한다.
`v1.0.0`의 안정 계약은 이 문서의 Stable API 목록을 기준으로 한다.

분류 기준:

- Stable: 새 사용자는 이 경로를 기본 선택으로 사용해도 된다.
- Compatibility: 기존 사용자를 위해 유지하지만, 새 코드는 다른 경로를 우선한다.
- Experimental: 구조는 잡혔지만 향후 breaking change 가능성이 상대적으로 높다.

## Stable API

### Config / Client

- `LoadConfig`
- `(*Config).EnsureDir`
- `(*Config).NewClient`
- `NewClient`
- `WithLocalStorePath`
- `WithHTTPClient`
- `WithClock`
- `(*Client).LocalStorePath`
- `(*Client).PackageVolume`
- `(*Client).PackageVolumeWithOptions`
- `(*Client).PushPackagedVolume`
- `(*Client).PushPackagedVolumeWithOptions`
- `(*Client).FetchVolume`
- `(*Client).FetchVolumeSequential`
- `(*Client).FetchVolumeParallel`
- `(*Client).FetchVolumeFresh`
- `(*Client).FetchVolumeFromRemote`
- `(*Client).PublishVolume`
- `(*Client).PublishVolumeFromDir`

### Core packaging / fetch

- `GenerateVolumeIndex`
- `ValidateVolumeDir`
- `TarGzDir`
- `UntarGzDir`
- `PackageVolumeToStore`
- `PushPackagedVolume`
- `FetchVolSeq`
- `FetchVolParallel`

### Generic metadata

- `ArtifactMetadata`
- `ArtifactMetadataInput`
- `BuildArtifactMetadata`
- `DatasetMetadata`
- `Organism`
- `DatasetReference`
- `CompatibleInput`
- `DatasetMetadataSchemaVersion`
- `MediaTypeDatasetMetadata`
- `MediaTypeChunkedConfig`
- `MediaTypeChunkIndex`
- `ValidateDatasetMetadata`
- `ValidateDatasetMetadataJSON`
- `ChunkIndex`

### Stable core result types

- `PackageRequest`
- `PackageResult`
- `RemoteTarget`
- `PushResult`
- `Partition`
- `VolumeIndex`

### Error / option model

- `ErrValidation`
- `ErrNotFound`
- `ErrConflict`
- `ErrIntegrity`
- `ErrTransport`
- `ErrAuth`
- `PackageOptions`
- `PushOptions`
- `FetchOptions`

## Compatibility API

이 API는 당장 제거하지 않지만, 새 코드는 Stable API를 우선한다.

- `InitConfig`
- `PackageVolume`
- `PushLocalToRemote`
- `(*VolumeIndex).PublishVolume`

이유:

- 객체 기반 `Client` 도입 이전 경로다.
- 내부적으로는 Stable API 위 thin wrapper로 유지된다.

## Experimental API

이 API는 NodeVault adapter 성격이 강하거나, 향후 구조 이동 가능성이 있다.

- `DataSpec`
- `BuildDataSpec`
- `PushRemoteDataSpecReferrer`
- `PushToolSpecReferrer`
- `PushDataSpecReferrer`
- `MarshalSpec`
- `ReferrerTarget`
- `SpecReferrerResult`
- `DataRegisterRequest`
- `RegisteredDataDefinition`
- `DataRegisterResponse`
- `DataCatalog`
- `RegisterPackagedData`
- `BuildRegisteredDataDefinition`
- `ArtifactMetadataToDataSpec`
- `ArtifactMetadataToRegisteredDataDefinition`
- `DisplaySpec`
- `ReferrerOptions`

이유:

- NodeVault / Catalog 경계에 더 가깝다.
- Sprint 8에서 generic metadata 아래 adapter로 내렸지만, 향후 `adapters/nodevault`로 이동할 가능성이 남아 있다.

추가 기준:

- 새 코드는 가능하면 `ArtifactMetadata`까지만 core 계약으로 사용한다.
- `DataSpec`, referrer, registration/catalog 계층은 상위 시스템이 실제로 필요할 때만 붙인다.

## Compatibility Promise

- Stable API는 v1 이후 patch/minor 수준에서 시그니처를 불필요하게 바꾸지 않는다.
- Stable API의 exported type field는 documented meaning을 유지한다.
- Compatibility API는 유지하되, 새 기능은 Stable API 중심으로 추가한다.
- Experimental API는 필요 시 breaking change가 가능하다. 변경 시 README와 이 문서에 먼저 반영한다.

## v1.0.0 Gate

`v1.0.0`을 태그하기 전에는 [v1-readiness.md](v1-readiness.md)의
Stable API, metadata, CLI, registry compatibility 항목을 닫아야 한다.
특히 `PackageOptions`, `PushOptions`, `FetchOptions`, `RemoteTarget`,
`DatasetMetadata`, `ChunkIndex`는 v1 이후 사용자가 직접 의존할 가능성이 큰
계약으로 보고 최종 검토한다.

## Breaking Change 후보

아래는 다음 major에서 정리 대상이다.

- `InitConfig`
- `PackageVolume`
- `PushLocalToRemote`
- `(*VolumeIndex).PublishVolume`
- NodeVault adapter 타입의 별도 패키지 이동

## 사용 권장 순서

새 프로젝트는 아래 순서로 선택한다.

1. `LoadConfig` + `Config.NewClient`
2. `Client.PackageVolumeWithOptions`
3. `Client.PushPackagedVolumeWithOptions`
4. `BuildArtifactMetadata`
5. 필요 시에만 experimental 계층 (`BuildDataSpec`, `RegisterPackagedData`, referrer push`)을 붙인다

## 바로 써도 되는 영역

- `Client` 기반 package / push / fetch
- `PackageResult`, `PushResult`
- `BuildArtifactMetadata`
- typed error / option 모델

## 아직 주의가 필요한 영역

- `DataSpec`
- referrer push helper
- registration / catalog helper
- root package에 남아 있는 NodeVault 친화 타입
