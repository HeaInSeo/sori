# Registry Integration Guide

`sori`는 기본 단위 테스트 외에 env-gated registry 통합 테스트 골격을 제공한다.

테스트 파일:

- [integration_registry_test.go](/opt/go/src/github.com/HeaInSeo/sori/integration_registry_test.go:1)

현재 제공하는 통합 테스트는 두 단계로 나뉜다.

- `TestRegistryIntegration_PackagePushOnly`
- `TestRegistryIntegration_PackagePushReferrer`

기본 동작:

- 기본 `go test ./...`에서는 skip된다.
- 실제 registry에 붙일 때만 환경 변수를 설정해 실행한다.

## 환경 변수

- `SORI_RUN_REGISTRY_INTEGRATION=1`
- `SORI_REGISTRY_HOST`
  예: `harbor.example.com`
- `SORI_REGISTRY_REPOSITORY`
  예: `project/dataset`
- `SORI_REGISTRY_TAG`
  기본값: `integration.v1`
- `SORI_REGISTRY_USERNAME`
- `SORI_REGISTRY_PASSWORD`
- `SORI_REGISTRY_TOKEN`
- `SORI_REGISTRY_PLAIN_HTTP`
  `1`이면 HTTP 사용
- `SORI_REGISTRY_INSECURE_TLS`
  `1`이면 TLS 검증 완화
- `SORI_REGISTRY_REFERRERS_CAPABLE`
  지정하지 않으면 oras-go 자동 감지
  `1` 또는 `true`: Referrers API 강제
  `0` 또는 `false`: Referrers Tag fallback 강제

## 검증 흐름

`TestRegistryIntegration_PackagePushOnly`는 아래 순서로 동작한다.

1. temp OCI store에 package
2. remote registry로 push
3. pushed tag를 registry에서 다시 resolve

즉, artifact push와 digest 확정뿐 아니라, registry가 같은 tag를 올바른 manifest로
다시 resolve 하는지도 먼저 확인할 수 있다.

`TestRegistryIntegration_PackagePushReferrer`는 아래 순서로 동작한다.

1. temp OCI store에 package
2. remote registry로 push
3. pushed tag를 registry에서 다시 resolve
4. dataspec 생성
5. subject digest에 dataspec referrer push

즉, referrer 테스트는 최소한 아래 2가지를 한 번에 확인한다.

- manifest push 성공
- pushed tag resolve 성공
- referrer push 성공

## 권장 실행 예시

```bash
env \
  SORI_RUN_REGISTRY_INTEGRATION=1 \
  SORI_REGISTRY_HOST=harbor.example.com \
  SORI_REGISTRY_REPOSITORY=project/dataset \
  SORI_REGISTRY_USERNAME=admin \
  SORI_REGISTRY_PASSWORD=secret \
  SORI_REGISTRY_PLAIN_HTTP=0 \
  SORI_REGISTRY_REFERRERS_CAPABLE=true \
  go test -run TestRegistryIntegration_PackagePushReferrer ./...
```

push 단계만 먼저 확인하려면 아래처럼 실행한다.

```bash
env \
  SORI_RUN_REGISTRY_INTEGRATION=1 \
  SORI_REGISTRY_HOST=harbor.example.com \
  SORI_REGISTRY_REPOSITORY=project/dataset \
  go test -run TestRegistryIntegration_PackagePushOnly ./...
```

Makefile target으로는 아래처럼 실행할 수 있다.

```bash
env \
  SORI_RUN_REGISTRY_INTEGRATION=1 \
  SORI_REGISTRY_HOST=harbor.example.com \
  SORI_REGISTRY_REPOSITORY=project/dataset \
  SORI_REGISTRY_USERNAME=admin \
  SORI_REGISTRY_TOKEN=secret \
  make test-registry-integration
```

CLI smoke와 real dataset 검증은 [v1-test-plan.md](v1-test-plan.md)를 따른다.

## Registry capability 해석

- `ReferrersCapability=nil`
  oras-go가 registry capability를 자동 감지한다.
- `ReferrersCapability=true`
  Referrers API 사용을 강제한다.
- `ReferrersCapability=false`
  Referrers Tag schema fallback을 강제한다.

운영 환경에서는 먼저 `nil`로 검증하고, registry별 동작 차이가 있을 때만 명시 설정을 권장한다.

## 단계별 진단 기준

- `PushPackagedVolume` 실패
  auth, TLS, repository 경로, push 권한 문제일 가능성이 높다.
- push는 성공하지만 resolve mismatch
  registry tag 상태, proxy/cache, repository 경로 불일치 가능성을 먼저 본다.
- push/resolve는 성공하지만 referrer push 실패
  referrers capability, subject digest 접근, registry 구현 차이를 먼저 본다.

실제 운영에서는 `TestRegistryIntegration_PackagePushOnly`를 먼저 돌리고,
그 다음 `TestRegistryIntegration_PackagePushReferrer`로 넘어가는 순서를 권장한다.

## 현재 상태

현재 `sori` 저장소에는 실제 Harbor / distribution / zot를 자동으로 띄우는 테스트 harness는 없다.
즉, 이 문서와 테스트는 “외부에 준비된 registry를 상대로 검증하는 골격”까지 제공한다.
