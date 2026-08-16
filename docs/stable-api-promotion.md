# Stable API Promotion Notes

이 문서는 현재 `sori`에서 아직 experimental 또는 경계가 애매한 영역을
stable API로 올리기 전에 검토해야 할 항목을 정리한 문서다.

목적은 두 가지다.

1. 지금 이미 stable로 봐도 되는 영역과 아닌 영역을 분리한다.
2. stable 승격 전에 어떤 결정을 먼저 내려야 하는지 정리한다.

## 현재 stable에 가까운 영역

아래 영역은 현재 기준으로 이미 충분히 안정적이다.

- `Client` 기반 package / push / fetch
- `PackageRequest`, `PackageResult`, `RemoteTarget`, `PushResult`
- `BuildArtifactMetadata`
- typed error / option 모델의 core 경로
- deterministic archive / local OCI store / remote push 기본 흐름

즉, `Client` + `BuildArtifactMetadata`까지는 지금도 사실상 stable core로 볼 수 있다.

## 아직 stable 승격이 보류된 영역

아래 영역은 지금 문서상 `Experimental`로 두고 있는 범위다.

- `DataSpec`
- `BuildDataSpec`
- `PushRemoteDataSpecReferrer`
- `PushToolSpecReferrer`
- `PushDataSpecReferrer`
- `DataRegisterRequest`
- `RegisteredDataDefinition`
- `BuildRegisteredDataDefinition`
- `ArtifactMetadataToDataSpec`
- `ArtifactMetadataToRegisteredDataDefinition`
- `DisplaySpec`
- `ReferrerOptions`

## stable 승격 전에 남은 핵심 결정

### 1. NodeVault 친화 타입을 root package에 둘지 결정

질문:
- `DataSpec`
- registration 타입
- referrer helper

이들을 계속 root package의 public contract로 둘 것인지,
아니면 `adapters/nodevault` 같은 별도 계층으로 완전히 분리할 것인지 먼저 정해야 한다.

이 결정을 미루면 stable surface 자체가 계속 흔들린다.

### 2. registration 모델 구조를 고정할지 결정

아래 타입은 상위 시스템 계약과 직접 연결된다.

- `DataRegisterRequest`
- `RegisteredDataDefinition`
- `DisplaySpec`

stable로 올리려면 필드 의미와 유지 범위를 먼저 고정해야 한다.

특히 아래를 검토해야 한다.

- `StableRef` 규칙을 이대로 둘지
- `Checksum` 의미를 manifest digest 기준으로 고정할지
- `Display` 구조를 이대로 유지할지
- `LifecyclePhase`, `IntegrityHealth`를 core 타입으로 둘지

> **Removed**: 로컬 JSON 카탈로그(`DataCatalog`, `NewDataCatalog`, `RegisterPackagedData`,
> `DataRegisterResponse`)는 프로덕션 호출자가 없어 제거되었다(sori#3). 따라서 stable 승격
> 검토는 순수 변환 계층(`BuildRegisteredDataDefinition`,
> `ArtifactMetadataToRegisteredDataDefinition`)과 그 데이터 모델만 대상으로 한다 —
> 등록 레코드의 영속화는 sori의 책임이 아니다.

### 3. referrer API의 책임 범위를 고정할지 결정

현재 referrer helper는 실용적이지만, stable 계약으로 보기엔 아직 질문이 남아 있다.

- subject resolve까지 helper가 책임질지
- remote push helper와 local referrer helper를 같이 stable로 둘지
- 반환값에 무엇을 보장할지
- Referrers API / fallback 차이를 어디까지 숨길지

즉, “어디까지가 라이브러리 책임인가”를 먼저 고정해야 한다.

### 4. registry 실증 범위를 더 확보할지 결정

현재는 env-gated integration test와 Harbor 중심 운영 문서가 있다.
하지만 stable 승격 전에는 최소한 다음 판단이 필요하다.

- Harbor 외 추가 registry 검증이 필요한지
- stable 보장 범위를 Harbor 중심으로 한정할지
- referrer 관련 지원 범위를 명시적으로 제한할지

안정 API는 단지 시그니처가 아니라, 실제 상호운용성 기대도 포함한다.

### 5. 에러 계약을 문서 수준으로 잠글지 결정

typed error는 많이 정리됐지만, stable 승격 전에는 아래를 명시해야 한다.

- 어떤 경로가 어떤 `Err*` kind를 주로 반환하는지
- validation / transport / integrity 경계가 어디인지
- experimental 계층도 같은 규칙을 따를지

호출자가 `errors.Is`를 안정적으로 사용해도 되는지 문서로 못 박아야 한다.

### 6. option 모델을 더 확장할지, 지금 잠글지 결정

아래 옵션 타입은 이미 존재한다.

- `PackageOptions`
- `PushOptions`
- `FetchOptions`
- `ReferrerOptions`

stable 승격 전에는 두 방향 중 하나를 선택해야 한다.

- 더 필요한 정책 옵션을 먼저 추가한 뒤 잠근다
- 현재 범위를 stable 1차 버전으로 선언한다

즉, 옵션 surface를 언제 얼릴지 판단이 필요하다.

### 7. compatibility API를 언제 내릴지 결정

아래 API는 현재 compatibility 경로다.

- `InitConfig`
- `PackageVolume`
- `PushLocalToRemote`
- `(*VolumeIndex).PublishVolume`

stable 승격 자체와 직접 충돌하진 않지만, 이 경로들이 오래 남을수록
사용자 입장에서 어떤 API가 “정식 경로”인지 흐려진다.

## 실무적으로 남은 일

검토 관점에서 보면 결국 아래 세 가지다.

1. NodeVault 경계를 stable로 끌어올릴지, adapter/experimental로 남길지 결정
2. registration/referrer 타입 구조를 실제 계약으로 고정할지 결정
3. registry/에러/옵션 보장 범위를 어느 수준까지 약속할지 결정

## 추천 판단 기준

### stable로 바로 올려도 되는 조건

- 향후 1~2 minor 동안 필드/시그니처를 거의 바꾸지 않을 자신이 있음
- Harbor 외 환경에서도 기대 동작을 설명할 수 있음
- 상위 시스템 계약이 이미 거의 고정돼 있음

### 아직 experimental로 남겨야 하는 조건

- NodeVault 쪽 이름/필드/흐름이 바뀔 가능성이 큼
- referrer helper의 책임 범위가 아직 논쟁적임
- registration 모델이 아직 제품 정책에 따라 바뀔 수 있음

## 현재 권고

현재 시점에서의 권고는 이렇다.

- `Client` + `BuildArtifactMetadata`까지는 stable core로 유지
- `DataSpec` / registration / referrer 계층은 검토가 끝나기 전까지 experimental 유지

즉, 지금 남은 검토 포인트는 “코어를 stable로 만들 수 있느냐”가 아니라,
“NodeVault 친화 경계를 stable로 승격할 준비가 되었느냐”에 가깝다.
