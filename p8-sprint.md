# P8 Sprint: Cache-Aware / Idempotent Fetch

**근거**: k8s 유전체 분석 환경에서 동일 레퍼런스(STAR index 40GiB 등)를 다수의 job/pod가
반복적으로 fetch하는 패턴이 있음. V1 현재 `FetchVolumeFromRemote`는 매번 무조건 전체 다운로드.
`volume-index.json`에 이미 `VolumeRef`(manifest digest)가 저장되어 있으므로,
remote 태그를 resolve한 digest와 비교해 "이미 같은 버전"이면 skip할 수 있음.

**Sprint doc**: `p8-sprint.md`  
**Status**: 선행 조사 완료 · 미착수  
**Depends on**: v0.7.1 이상 (chunked + AtomicOverwrite 통합 완료 전제)

Legend: ✅ done · 🔄 in-progress · ⏳ pending · ❌ blocked

---

## 선행 조사 요약

### 1. 현재 코드베이스 상태

**`volume-index.json` 이미 존재**  
`writeVolumeIndex(destRoot, vi)` (`volume_validation.go:124`) 가 fetch 성공 후
`destRoot/volume-index.json`에 JSON을 씀. `VolumeRef` 필드는 manifest digest 문자열
(`sha256:abc...`).

**tag resolve는 이미 fetch 전에 수행**  
`detectManifestMediaType` (`volume_publish_fetch.go:57`) 가 `src.Resolve(ctx, tag)`를
호출해 manifest descriptor를 얻음 — 이 descriptor의 `Digest.String()`이 바로 VolumeRef.
`fetchRemoteWithDualPath` 첫 단계에서 이미 호출됨.

**결론**: 파운데이션이 이미 갖춰져 있다.  
새로 필요한 것은 ①로컬 `volume-index.json` 읽기 ②digest 비교 ③match 시 skip — 세 줄 수준의 로직.

---

### 2. 유전체 분석 k8s 사용 패턴

```
┌────────────────────────── k8s Node ───────────────────────────┐
│                                                                │
│  PVC / hostPath: /data/refs/                                   │
│    hg38-star/      ← STAR index 40GiB                         │
│      volume-index.json  (VolumeRef = sha256:aaa...)            │
│    hg38-bwa/       ← BWA index 15GiB                          │
│                                                                │
│  ┌── init-container ──────────────────────────────────────┐   │
│  │  sori.FetchVolumeFromRemote(                           │   │
│  │    destRoot: "/data/refs/hg38-star",                   │   │
│  │    tag: "hg38-2.7.10a",                                │   │
│  │    opts: {AtomicOverwrite: true, SkipIfCurrent: true}  │   │
│  │  )                                                      │   │
│  │  → remote digest == local VolumeRef → SKIP (0ms)       │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                │
│  ┌── STAR aligner pod (×50 동시) ──────────────────────────┐   │
│  │  mount: /data/refs/hg38-star (ReadOnly)                 │   │
│  └─────────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────────┘
```

**Without `SkipIfCurrent`**: 50개 job이 뜰 때 각 init-container마다 149s fetch.  
**With `SkipIfCurrent`**: init-container는 Resolve 1회 (~200ms) + 파일 1개 읽기로 종료.

---

### 3. API 설계

#### 3-A. FetchOptions 확장

```go
// options.go
type FetchOptions struct {
    Concurrency             int
    RequireEmptyDestination bool
    AtomicOverwrite         bool
    SkipIfCurrent           bool  // NEW: remote digest == local VolumeRef이면 fetch skip
}
```

**제약**: `SkipIfCurrent=true`는 `RequireEmptyDestination=true`와 함께 쓸 수 없음
(`RequireEmptyDestination`은 "dest가 없을 것을 강제"하므로, dest가 있으면 conflict로 처리됨).
`AtomicOverwrite=true`와는 자연스럽게 조합 가능.

#### 3-B. VolumeIndex에 Skipped 필드 추가

```go
// volume-index.go
type VolumeIndex struct {
    VolumeRef   string      `json:"volume_ref"`
    DisplayName string      `json:"display_name"`
    CreatedAt   string      `json:"created_at"`
    Partitions  []Partition `json:"partitions"`
    Skipped     bool        `json:"-"`  // runtime-only; 디스크에 쓰지 않음
}
```

`json:"-"` 태그로 `volume-index.json` 직렬화에서 제외. 반환값에서만 의미 있음.

#### 3-C. 구현 주입 위치

`fetchRemoteWithDualPath` 내부, `detectManifestMediaType` 호출 직후:

```go
func fetchRemoteWithDualPath(ctx context.Context, caller, destRoot string,
    src oras.ReadOnlyTarget, tag string, opts FetchOptions) (*VolumeIndex, error) {

    manifestDesc, mediaType, err := detectManifestMediaType(ctx, src, tag)
    if err != nil {
        return nil, err
    }

    // SkipIfCurrent: remote digest와 로컬 VolumeRef 비교
    if opts.SkipIfCurrent {
        if localVI, err := readLocalVolumeIndex(destRoot); err == nil &&
            localVI.VolumeRef == manifestDesc.Digest.String() {
            localVI.Skipped = true
            return localVI, nil
        }
        // 읽기 실패(파일 없음 등)는 무시하고 fetch 진행
    }

    // 기존 dispatch 로직...
    switch mediaType {
    case chunked.MediaTypeConfig:
        // ...
    }
}
```

#### 3-D. readLocalVolumeIndex 헬퍼

```go
// volume_validation.go (writeVolumeIndex와 같은 파일)
func readLocalVolumeIndex(destRoot string) (*VolumeIndex, error) {
    path := filepath.Join(destRoot, "volume-index.json")
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err  // 파일 없으면 err; 호출자가 무시
    }
    var vi VolumeIndex
    if err := json.Unmarshal(data, &vi); err != nil {
        return nil, err
    }
    return &vi, nil
}
```

---

### 4. 엣지 케이스 분석

| 상황 | 동작 | 안전한가? |
|---|---|---|
| `volume-index.json` 없음 | 읽기 실패 → 정상 fetch | ✅ |
| `VolumeRef` 비어 있음 | `""` ≠ remote digest → 정상 fetch | ✅ |
| digest 일치, 파일 일부 손상 | skip 후 손상된 데이터 반환 | ⚠️ (아래 주석) |
| `RequireEmptyDestination=true` + `SkipIfCurrent=true` | validation 오류 반환 | ✅ |
| `AtomicOverwrite=true` + `SkipIfCurrent=true` + 버전 일치 | skip (overwrite 불필요) | ✅ |
| tag가 mutable (`latest`) | `Resolve`가 항상 현재 digest 반환 → 비교 정상 작동 | ✅ |
| destRoot 존재하나 `volume-index.json` 없음 | 읽기 실패 → 정상 fetch | ✅ |

**⚠️ 파일 손상 주의**: `SkipIfCurrent=true`는 로컬 파일을 재검증하지 않음.
컨테이너 이미지 레이어 캐시와 동일한 신뢰 모델 — digest match = 신뢰.
손상 검증이 필요하면 `SkipIfCurrent=true` + `VerifyTree=true` (chunked path) 조합을 쓰거나,
별도로 `chunked.VerifyDestTree(destRoot, files)` 호출.

---

### 5. 진행 Progress 이벤트

```go
// 기존 ArtifactDone 대신 skip 시 별도 이벤트 필요
type FetchSkippedEvent struct {
    ManifestDigest string
    DestRoot       string
}
```

또는 기존 `ProgressFunc` 시그니처 내에서 `ArtifactDone`의 `DurationMs=0`으로 표현.
API 복잡도 고려해 **skip은 `VolumeIndex.Skipped=true`로만 표현하고 progress event는 생략** 권장.

---

### 6. 로컬 스토어 변형 (FetchVolSeq/FetchVolParallel)

remote fetch와 동일한 패턴 적용 가능하나 우선순위 낮음:
- k8s 환경에서 primary use case는 `FetchVolumeFromRemote`
- local OCI store 변형은 "같은 머신에서 동일 태그 재fetch"가 드문 케이스

---

### 7. 구현 규모 예상

| 파일 | 변경 내용 | 규모 |
|---|---|---|
| `options.go` | `SkipIfCurrent bool` 추가 | +1줄 |
| `volume-index.go` | `Skipped bool \`json:"-"\`` 추가 | +1줄 |
| `volume_validation.go` | `readLocalVolumeIndex()` 추가 | +15줄 |
| `volume_publish_fetch.go` | `fetchRemoteWithDualPath`에 skip 로직 추가 | +10줄 |
| `client.go` | `RequireEmptyDestination + SkipIfCurrent` validation | +4줄 |
| `dual_path_test.go` 또는 신규 | skip/stale/no-index 테스트 케이스 | +80줄 |

**총 구현 공수**: 반나절~하루.

---

### 8. 논문 임팩트

현재 claim:
> "sori reduces first-fetch time 8.7× vs legacy"

`SkipIfCurrent` 추가 후:
> "sori reduces first-fetch time 8.7× vs legacy, **and eliminates redundant re-fetch
> via digest-based cache-hit detection** — reducing repeated access from 149s to <1s
> for a 40 GiB STAR index on k8s init containers"

k8s 초기화 시간 개선이 생물정보학 파이프라인 실제 사용 맥락에서 직접적인 evidence.

---

## 스프린트 체크리스트

### P8-1: API 추가 및 skip 로직

| # | 항목 | 상태 |
|---|---|---|
| 1 | `FetchOptions.SkipIfCurrent bool` 추가 (`options.go`) | ⏳ |
| 2 | `VolumeIndex.Skipped bool \`json:"-"\`` 추가 (`volume-index.go`) | ⏳ |
| 3 | `readLocalVolumeIndex(destRoot string) (*VolumeIndex, error)` 구현 (`volume_validation.go`) | ⏳ |
| 4 | `fetchRemoteWithDualPath`에 skip 분기 추가 (`volume_publish_fetch.go`) | ⏳ |
| 5 | `client.go`: `RequireEmptyDestination=true + SkipIfCurrent=true` → `ErrValidation` | ⏳ |
| 6 | `go vet ./...` 통과 | ⏳ |

### P8-2: 테스트

| # | 항목 | 상태 |
|---|---|---|
| 7 | `TestSkipIfCurrent_ChunkedCAS`: 동일 태그 두 번 fetch → 두 번째 `Skipped=true` | ⏳ |
| 8 | `TestSkipIfCurrent_Legacy`: legacy 경로 동일 동작 확인 | ⏳ |
| 9 | `TestSkipIfCurrent_StaleVersion`: 새 버전 push 후 fetch → `Skipped=false`, 새 내용 | ⏳ |
| 10 | `TestSkipIfCurrent_NoLocalIndex`: `volume-index.json` 없을 때 → 정상 fetch | ⏳ |
| 11 | `TestSkipIfCurrent_EmptyVolumeRef`: VolumeRef="" 일 때 → 정상 fetch | ⏳ |
| 12 | `TestSkipIfCurrent_RequireEmpty_Conflict`: `RequireEmptyDestination + SkipIfCurrent` → `ErrValidation` | ⏳ |
| 13 | `go test ./...` 전체 green | ⏳ |

### P8-3: 문서 및 태깅

| # | 항목 | 상태 |
|---|---|---|
| 14 | `readme.md` FetchOptions 테이블 업데이트 | ⏳ |
| 15 | `docs/research/limitations.md` "No Fetch Resume" 아래에 "Cache-Aware Fetch" 섹션 추가 | ⏳ |
| 16 | 벤치마크: `SkipIfCurrent=true` 두 번째 fetch 시간 측정 추가 (M-13 후보) | ⏳ |
| 17 | `git tag v0.7.2` | ⏳ |

---

## 의존 관계

```
P8-1 ──► P8-2 ──► P8-3 ──► v0.7.2
```

P8-1과 P8-2는 순차 진행 (테스트가 구현에 의존).  
P8-3은 P8-1, P8-2 완료 후.

---

## 미결 사항 (착수 전 결정 필요)

| # | 질문 | 옵션 |
|---|---|---|
| Q-1 | skip 시 progress event 추가할지 여부 | (a) `VolumeIndex.Skipped`만으로 충분 (b) `FetchSkippedEvent` 별도 추가 |
| Q-2 | local OCI store 변형(FetchVolSeq/Parallel)에도 P8에서 적용할지 | (a) remote only (b) 모든 fetch 경로 |
| Q-3 | `EnsureVolumeFromRemote` 편의 함수 추가 여부 | (a) 없이 FetchOptions 직접 조합 (b) 추가 |
