# Test Strategy

`sori`의 v1 테스트 전략은 smoke를 첫 관문으로 두되, smoke만으로
release 결정을 하지 않는다. 전체 v1 release gate는
[v1-test-plan.md](v1-test-plan.md)와 [v1-readiness.md](v1-readiness.md)를 따른다.

## v1 테스트 계층

| 계층 | 목적 | 실행 |
|---|---|---|
| Unit / short | 빠른 correctness, typed error, local safety | `make test` |
| Coverage | 주요 패키지 coverage 확인 | `make coverage` |
| Vulnerability | reachable vuln 확인 | `make vuln` |
| CLI smoke | 실제 registry에서 첫 사용자 흐름 확인 | `make smoke-cli` |
| Registry integration | Go library registry path 확인 | `make test-registry-integration` |
| Real dataset | 실제 유전체 fixture push/fetch 확인 | `make test-real-dataset` |
| Scale / performance | 대용량 chunked CAS 시간/RSS 확인 | benchmark |
| Reliability / failure | corrupt/cancel/rollback/retry 확인 | local tests + targeted integration |
| Release acceptance | release binary/checksum/asset 확인 | tag-triggered release + binary smoke |

smoke 통과 기준은 `metadata init -> push -> inspect -> fetch`가 실제
registry에서 성공하고, fetched directory가 source와 byte-level로 동일하며,
`--overwrite --skip-if-current`가 기대대로 동작하는 것이다.

## 테스트 범주 정의

| 범주 | 정의 | 핵심 검증 포인트 |
|---|---|---|
| **negative test** | 잘못된 입력·경계 조건을 넣어 거부/실패하는지 확인 | 예상 에러 타입이 반환되는가 |
| **error-path test** | 에러가 발생하는 코드 경로가 올바른 에러 타입·메시지로 전달되는지 확인 | `errors.Is`, `errors.As` 통과 여부 |
| **failure-path test** | 실패 상황(파일 없음, 권한 없음)에서 의도한 흐름으로 가는지 확인 | 올바른 에러 반환, 부작용 없음 |
| **fault injection test** | 의도적으로 장애 주입(read-only dir, corrupt data)으로 시스템 반응 확인 | 에러 리턴, 임시 파일 정리 |
| **resilience test** | 장애 후 상태가 일관적으로 유지·복구되는지 확인 | 상태 일관성, 리소스 정리 |
| **graceful failure test** | 패닉 없이 에러를 리턴하고 안전하게 종료하는지 확인 | `nil` panic 없음, temp 파일 없음 |
| **context cancel test** | 취소된 컨텍스트에서 데드락 없이 종료하는지 확인 | 타임아웃 이내 반환, goroutine 누수 없음 |
| **concurrent test** | 동시 접근 시 데이터 경쟁·패닉 없음을 확인 (`-race`) | 최종 상태 일관성, race-free |

---

## 현재 테스트 구성 현황 (2026-05-27 기준)

### 패키지별 커버리지

| 패키지 | 커버리지 |
|---|---|
| `github.com/HeaInSeo/sori` (root) | 72.1% |
| `adapters/nodevault` | 78.6% |
| `archiveutil` | 75.8% |
| `catalogutil` | 72.1% |
| `registryutil` | 80.6% |
| **전체** | **73.3%** |

### 범주별 테스트 분포

| 범주 | 수 | 대표 테스트 |
|---|---|---|
| negative test | 32 | `TestValidateVolumeDir_NonExistent`, `TestRemoveVolume_OutOfRange` |
| error-path test | 38 | `TestAllErrorSentinels`, `TestError_ErrorString_AllBranches` |
| failure-path test | 11 | `TestLoadOrNewCollection_ReadError`, `TestFetchVolParallel_NonExistentStore` |
| fault injection test | 8 | `TestWriteFileAtomic_NoTempSurvivorOnFailure`, `TestSave_CreateDirTypedError` |
| resilience test | 10 | `TestFetchWithStaging_CorruptLayer_StagingCleaned`, `TestWriteFileAtomic_NoTempSurvivorOnSuccess` |
| graceful failure test | 7 | `TestFetchVolSeq_CorruptLayer_GracefulError`, `TestFetchWithStaging_ValidThenCorruptLayer_StagingCleaned` |
| context cancel test | 5 | `TestFetchVolSeq_CancelledContext_NoDeadlock`, `TestFetchVolParallel_CancelledContext_NoGoroutineLeak` |
| concurrent test | 6 | `TestCollectionManager_ConcurrentAddOrUpdate`, `TestCollectionManager_ConcurrentRemove` |

---

## 범주별 작성 패턴

### 1. negative test

잘못된 입력을 주고 `errors.Is`로 에러 타입을 검증한다.

```go
func TestRemoveVolume_OutOfRange(t *testing.T) {
    vc := NewVolumeCollection()
    err := vc.RemoveVolume(0)                 // 빈 컬렉션에서 인덱스 0 제거 시도
    if !errors.Is(err, ErrValidation) {
        t.Fatalf("expected ErrValidation, got %v", err)
    }
}
```

**패턴 핵심**:
- 경계값(범위 초과, 빈 문자열, nil)을 직접 전달
- `errors.Is(err, ErrXxx)` 로 타입만 검증 (메시지 문자열 비교 지양)

---

### 2. error-path test

에러 구조체(Error)의 모든 포맷 분기를 직접 커버한다.

```go
func TestError_ErrorString_AllBranches(t *testing.T) {
    wrapped := fmt.Errorf("cause")
    cases := []struct{ e *Error; want string }{
        {nil,                                          "<nil>"},
        {&Error{Op:"op", Message:"msg", Err:wrapped},  "op: msg: cause"},
        {&Error{Op:"op", Message:"msg"},               "op: msg"},
        {&Error{Op:"op", Err:wrapped},                 "op: cause"},
        {&Error{Op:"op"},                              "op"},
    }
    for _, c := range cases {
        if got := c.e.Error(); got != c.want {
            t.Errorf("Error() = %q, want %q", got, c.want)
        }
    }
}
```

**패턴 핵심**:
- 에러 타입의 `Error()` / `Unwrap()` / `Is()` 메서드를 nil 케이스 포함 전부 테스트
- `errors.Is(outer, inner)` 로 Unwrap 체인을 검증

---

### 3. failure-path test

외부 조건(파일 권한, 비존재 경로)으로 실패를 유도한다.

```go
func TestLoadOrNewCollection_ReadError(t *testing.T) {
    if os.Getuid() == 0 {
        t.Skip("cannot test read permission as root")
    }
    root := t.TempDir()
    collPath := filepath.Join(root, CollectionJson)
    if err := os.WriteFile(collPath, []byte(`{"version":1}`), 0o000); err != nil {
        t.Fatalf("WriteFile: %v", err)
    }
    t.Cleanup(func() { _ = os.Chmod(collPath, 0o644) })

    _, err := LoadOrNewCollection(root)
    if !errors.Is(err, ErrTransport) {
        t.Fatalf("expected ErrTransport for unreadable file, got %v", err)
    }
}
```

**패턴 핵심**:
- `os.Chmod(path, 0o000)` / `0o555` 로 권한 주입
- root 실행 시 `t.Skip` (root는 권한 제한 우회)
- `t.Cleanup` 으로 권한 복구

---

### 4. fault injection test

의도적으로 잘못된 데이터(corrupt gzip, invalid JSON, 파일을 디렉터리 경로로 사용)를 주입한다.

```go
func TestFetchVolSeq_CorruptLayer_GracefulError(t *testing.T) {
    corruptData := []byte("this is not a valid gzip stream")
    corruptDesc := ocispec.Descriptor{
        MediaType: ocispec.MediaTypeImageLayerGzip,
        Digest:    digest.FromBytes(corruptData),
        Size:      int64(len(corruptData)),
        Annotations: map[string]string{
            annotationPartitionPath: "vol/part",
            annotationLayerKind:     layerKindPartition,
        },
    }
    // storePath에 corruptDesc를 push하고 manifest 태깅
    // ...

    _, err := FetchVolSeq(ctx, dest, storePath, "corrupt.v1")
    if !errors.Is(err, ErrIntegrity) {
        t.Fatalf("expected ErrIntegrity, got %T: %v", err, err)
    }
}
```

**패턴 핵심**:
- 디스크립터 해시는 실제 데이터 해시와 일치시켜 스토어 푸시는 성공
- 내용만 유효하지 않게 만들어 extraction 단계에서 에러 유발
- 에러 타입 검증 후 상태 정리까지 검증

---

### 5. resilience test

장애 발생 후 임시 파일·디렉터리가 정리되고 불변 상태가 유지되는지 확인한다.

```go
func TestFetchWithStaging_CorruptLayer_StagingCleaned(t *testing.T) {
    // ... corrupt layer OCI store 구성 ...

    destRoot := filepath.Join(tmp, "dest")
    _, err := client.FetchVolume(ctx, destRoot, storePath, "corrupt.v1", FetchOptions{
        RequireEmptyDestination: true,
    })
    if err == nil {
        t.Fatal("expected error")
    }

    // destRoot 미생성 확인
    if _, statErr := os.Stat(destRoot); !os.IsNotExist(statErr) {
        t.Error("destRoot must not exist after failed staged fetch")
    }
    // staging 디렉터리 잔재 없음 확인
    entries, _ := os.ReadDir(tmp)
    for _, e := range entries {
        if strings.HasPrefix(e.Name(), ".staging-") {
            t.Errorf("stale staging dir: %s", e.Name())
        }
    }
}
```

**패턴 핵심**:
- 실패 후 `os.Stat(destRoot)` 로 `os.ErrNotExist` 확인
- `os.ReadDir(parent)` 로 `.staging-*` / `.tmp-*` 잔재 스캔
- 성공 케이스에서도 임시 파일 없음 확인

---

### 6. graceful failure test

패닉 없이 에러를 반환하고, 상태가 일관적임을 확인한다.

```go
func TestFetchWithStaging_ValidThenCorruptLayer_StagingCleaned(t *testing.T) {
    // 첫 번째 레이어는 유효, 두 번째는 corrupt
    // staging fetch 실패 → destRoot 없음, staging 정리
    // 패닉 없이 ErrIntegrity 반환 확인
}
```

**패턴 핵심**:
- 부분 성공 후 실패(첫 레이어 OK, 두 번째 실패)도 포함
- `t.Fatal` 없이도 패닉을 감지하려면 `defer func() { if r := recover(); r != nil { t.Fatalf(...) } }()` 사용 가능

---

### 7. context cancel test

이미 취소된 컨텍스트로 호출 시 데드락 없이 반환하는지 타임아웃 감시로 확인한다.

```go
// withTimeout: 타임아웃 감시 헬퍼
func withTimeout(t *testing.T, d time.Duration, f func()) {
    t.Helper()
    done := make(chan struct{})
    go func() {
        defer close(done)
        f()
    }()
    select {
    case <-done:
    case <-time.After(d):
        t.Fatalf("operation did not complete within %v — likely deadlock", d)
    }
}

func TestFetchVolParallel_CancelledContext_NoDeadlock(t *testing.T) {
    // ... valid OCI store 구성 ...
    ctx, cancel := context.WithCancel(context.Background())
    cancel() // 사전 취소

    withTimeout(t, 5*time.Second, func() {
        _, _ = FetchVolParallel(ctx, dest, storePath, "par.v1", 4)
    })
}
```

**goroutine 누수 검증 패턴**:
```go
runtime.Gosched()
time.Sleep(50 * time.Millisecond)
before := runtime.NumGoroutine()

// ... 테스트 대상 호출 ...

time.Sleep(100 * time.Millisecond)
after := runtime.NumGoroutine()
if after > before+5 {  // 여유분 5
    t.Fatalf("goroutine leak: %d → %d", before, after)
}
```

**패턴 핵심**:
- `context.WithCancel` 후 즉시 `cancel()` 호출
- goroutine 카운트는 GC 등 노이즈 때문에 여유분(±5) 허용
- 타임아웃은 로컬 작업 기준 3~5초면 충분

---

### 8. concurrent test

`-race` 플래그와 함께 실행하면 데이터 경쟁을 자동 감지한다.

```go
func TestCollectionManager_ConcurrentAddOrUpdate(t *testing.T) {
    const n = 20
    cm, _ := NewCollectionManager(t.TempDir())

    var wg sync.WaitGroup
    wg.Add(n)
    for i := 0; i < n; i++ {
        go func(i int) {
            defer wg.Done()
            _ = cm.AddOrUpdate(VolumeEntry{
                Index: VolumeIndex{VolumeRef: fmt.Sprintf("ref-%d", i)},
            })
        }(i)
    }
    wg.Wait()

    if snap := cm.GetSnapshot(); len(snap.Volumes) != n {
        t.Fatalf("expected %d volumes, got %d", n, len(snap.Volumes))
    }
}
```

**패턴 핵심**:
- `go test -race ./...` 로 실행 (CI에서 필수)
- 쓰기/읽기 혼합(writer N개 + reader N개)으로 실제 경쟁 조건 재현
- 최종 상태 일관성 검증(카운트, 버전, JSON 파싱 가능 여부)
- `t.Errorf` 사용 (goroutine 안에서 `t.Fatal` 호출 금지)

---

## 다른 프로젝트에 적용하는 법

### 체크리스트

```
[ ] Error 타입에 Error()/Unwrap()/Is() 메서드가 있다면 → error-path test 추가
[ ] 파일 쓰기 함수가 있다면 → read-only dir fault injection test 추가
[ ] 디렉터리 생성·이동 함수가 있다면 → 실패 후 임시 디렉터리 잔재 없음 확인
[ ] goroutine worker pool이 있다면 → context cancel + goroutine 카운트 검증
[ ] 공유 상태(mutex 보호)가 있다면 → concurrent test + -race 실행
[ ] 멀티 스텝 트랜잭션(staging→rename)이 있다면 → graceful failure test 추가
[ ] 네트워크/IO 경로가 있다면 → cancelled context NoDeadlock test 추가
```

### CI 설정

```yaml
# .github/workflows/test.yml 에 추가
- run: go test -race -count=1 ./...
```

`-race` 없이 passing하더라도 `-race`에서 실패하는 케이스가 실제 운영에서 문제가 된다.

### 우선순위 가이드

| 상황 | 먼저 추가할 범주 |
|---|---|
| 에러 타입 미검증 | error-path test |
| 파일 I/O 코드 | fault injection + resilience |
| 공유 상태 | concurrent |
| goroutine 사용 | context cancel |
| 멀티 스텝 원자 작업 | graceful failure |
