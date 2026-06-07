# sori

**v0.7.0-stable** — OCI 기반 참조 데이터(볼륨) 패키징 및 referrer push 라이브러리.  
디렉터리를 OCI 아티팩트로 변환하고, 로컬 OCI 스토어와 원격 레지스트리(Harbor 등) 사이의 push/fetch를 담당한다.

## 개요

`sori`는 바이오인포매틱스 파이프라인에서 사용하는 참조 데이터(genome, annotation 등)를  
OCI 이미지 형식으로 패키징하는 Go 라이브러리다.

- **Core (stable)**: OCI volume packaging + push/fetch + artifact metadata + chunked CAS
- **Experimental**: NodeVault-oriented referrer push (toolspec / toolprofile / security / dataspec)

| 문서 | 내용 |
|------|------|
| [docs/research/results-summary.md](docs/research/results-summary.md) | 벤치마크 결과 (v0.7.0-stable 실측값) |
| [docs/public-api.md](docs/public-api.md) | 공개 API 안정도 분류 |
| [docs/generalization-sprint-plan.md](docs/generalization-sprint-plan.md) | 범용 라이브러리화 로드맵 |
| [docs/maturity-sprint-plan.md](docs/maturity-sprint-plan.md) | 성숙화 계획 |
| [docs/stable-api-promotion.md](docs/stable-api-promotion.md) | stable API 승격 검토 항목 |
| [docs/followup-sprint-plan.md](docs/followup-sprint-plan.md) | 후속 스프린트 계획 |
| [docs/registry-integration.md](docs/registry-integration.md) | registry 통합 테스트 골격 |
| [docs/sorictl-quickstart.md](docs/sorictl-quickstart.md) | sorictl 초보자용 registry publish/fetch 가이드 |
| [docs/operations.md](docs/operations.md) | 운영 환경 체크리스트 |
| [docs/stub-status.md](docs/stub-status.md) | 과거 stub 처리 내역 |
| [docs/test-strategy.md](docs/test-strategy.md) | 테스트 범주 정의 · 패턴 · 다른 프로젝트 적용 가이드 |

내부 하위 패키지:

- `archiveutil`: deterministic tar.gz 생성과 안전한 untar
- `registryutil`: remote repository/TLS/auth/http client 구성
- `catalogutil`: JSON catalog load/save 공통 유틸
- `adapters/nodevault`: NodeVault 친화 metadata adapter 초안

## 의존성

| 패키지 | 버전 |
|--------|------|
| `oras.land/oras-go/v2` | v2.6.0 |
| `github.com/opencontainers/image-spec` | v1.1.1 |
| `github.com/opencontainers/go-digest` | v1.0.0 |
| `github.com/sirupsen/logrus` | v1.9.3 |

## 빠른 시작

```go
import "github.com/HeaInSeo/sori"

ctx := context.Background()

// 1) 설정 로드
cfg, err := sori.LoadConfig("sori-oci.json")
if err != nil { ... }
if err := cfg.EnsureDir(); err != nil { ... }

// 2) 컬렉션 매니저 초기화
cm, err := sori.NewCollectionManager(cfg.Local.Path)
if err != nil { ... }

// 또는 객체 기반 client 생성
client := cfg.NewClient()

// 3) 볼륨 디렉터리를 OCI로 패키징 + 컬렉션 등록
if err := cm.PublishVolumeFromDir(ctx, "./my-genome-dir", "HumanRef GRCh38", "grch38.v1.0.0"); err != nil { ... }

// 4) Stable core API: 패키징
pkg, err := sori.PackageVolumeToStore(ctx, cfg.Local.Path, sori.PackageRequest{
  SourceDir:   "./my-genome-dir",
  DisplayName: "HumanRef GRCh38",
  Tag:         "grch38.v1.0.0",
  Dataset:     "grch38",
  Version:     "v1.0.0",
})
if err != nil { ... }

// 5) Stable core API: 원격 레지스트리로 푸시
pushResult, err := sori.PushPackagedVolume(ctx, cfg.Local.Path, pkg, sori.RemoteTarget{
  Registry:   "harbor.local",
  Repository: "project/repo",
  Username:   "user",
  Password:   "pass",
  PlainHTTP:  true,
})
if err != nil { ... }
fmt.Println(pushResult.ManifestDigest)

// 6) Stable core API: generic metadata 생성
meta, err := sori.BuildArtifactMetadata(sori.ArtifactMetadataInput{
  Kind:        "dataset",
  Name:        "grch38-reference",
  Version:     "v1.0.0",
  DisplayName: "HumanRef GRCh38",
  Description: "Human reference genome",
  SourceDir:   "./my-genome-dir",
}, pkg, pushResult)
if err != nil { ... }
fmt.Println(meta.Identity.StableRef)

// 7) Experimental: NodeVault 친화 metadata 초안 생성
spec, err := sori.BuildDataSpec(pkg, pushResult, sori.PackageRequest{
  SourceDir:   "./my-genome-dir",
  DisplayName: "HumanRef GRCh38",
  Tag:         "grch38.v1.0.0",
  Dataset:     "grch38",
  Version:     "v1.0.0",
})
if err != nil { ... }

// 8) Experimental: Harbor subject manifest에 dataspec referrer push
referrerResult, err := sori.PushRemoteDataSpecReferrer(ctx, pushResult, sori.RemoteTarget{
  Registry:   "harbor.local",
  Repository: "project/repo",
  Username:   "user",
  Password:   "pass",
  PlainHTTP:  true,
}, spec)
if err != nil { ... }
fmt.Println(referrerResult.ManifestDigest)

// 9) Experimental: NodeVault/Catalog 친화적인 등록 객체 생성 및 저장
registerResp, err := sori.RegisterPackagedData(ctx, cfg.Local.Path, sori.DataRegisterRequest{
  DataName:    "grch38-reference",
  Version:     "v1.0.0",
  Description: "Human reference genome",
  Format:      "FASTA",
  SourceURI:   "s3://example/grch38.fa.gz",
}, pkg, pushResult)
if err != nil { ... }
fmt.Println(registerResp.CASHash)

// 10) Stable core API: 원격 레지스트리에서 직접 fetch (safe fetch 기본)
vi, err := client.FetchVolumeFromRemote(ctx, "./dest-dir", sori.RemoteTarget{
    Registry:   "harbor.local",
    Repository: "project/repo",
    PlainHTTP:  true,
}, "grch38.v1.0.0", sori.FetchOptions{})
if err != nil { ... }
fmt.Println(vi.DisplayName)

// AtomicOverwrite: 기존 destRoot를 백업 후 원자적 교체
vi, err = client.FetchVolumeFromRemote(ctx, "./dest-dir", sori.RemoteTarget{
    Registry:   "harbor.local",
    Repository: "project/repo",
}, "grch38.v1.1.0", sori.FetchOptions{AtomicOverwrite: true})
if err != nil { ... }
```

단계 `4~6`, `10`은 stable core 흐름이고, `7~9`는 experimental 계층이다.

## CLI UX 실험

`cmd/sorictl`은 `sori` 라이브러리를 사용하는 범용 OCI registry CLI 초안이다.  
GHCR, Harbor, local registry 등 특정 registry에 고정하지 않고 `--registry`와 `--repository`로 대상을 지정한다.
처음 사용하는 경우에는 [docs/sorictl-quickstart.md](docs/sorictl-quickstart.md)를 먼저 따라가는 것을 권장한다.

```bash
# 1) dataset-metadata.json 초안 생성
go run ./cmd/sorictl metadata init \
  --ref ghcr.io/OWNER/references:grch38-bwa-v1 \
  --name grch38-bwa \
  --display-name "GRCh38 BWA Index" \
  --description "Human GRCh38 BWA index." \
  > dataset-metadata.json

# 2) 데이터 디렉터리 패키징 + registry push
go run ./cmd/sorictl dataset push ./grch38-bwa \
  --ref ghcr.io/OWNER/references:grch38-bwa-v1 \
  --metadata dataset-metadata.json \
  --username USER \
  --password-env GHCR_TOKEN

# 3) 원격 artifact metadata 확인
go run ./cmd/sorictl dataset inspect \
  ghcr.io/OWNER/references:grch38-bwa-v1 \
  --username USER \
  --password-env GHCR_TOKEN

# 4) 원격 artifact fetch
go run ./cmd/sorictl dataset fetch \
  ghcr.io/OWNER/references:grch38-bwa-v1 ./refs/grch38-bwa \
  --username USER \
  --password-env GHCR_TOKEN
```

CLI는 UX 검증용 entrypoint이고, 안정 계약의 중심은 계속 Go library API다.

### Chunked CAS 경로 (v0.7.0-stable)

대용량 데이터셋(STAR index 40 GiB+ 등)에는 chunked CAS 포맷을 사용한다.  
`PackageOptions.Format`을 `ArtifactFormatChunkedCAS`로 지정하면 파일을 1 GiB 고정 청크로 분할해 OCI 레이어로 저장한다.  
fetch 측은 manifest의 `Config.MediaType`을 보고 자동으로 chunked 경로로 디스패치되므로 별도 설정이 필요 없다.

```go
// Chunked CAS push
pkg, err := client.PackageVolumeWithOptions(ctx, sori.PackageRequest{
    SourceDir:   "./star-index",
    DisplayName: "STAR index GRCh38",
    Tag:         "star:v2.7.10",
}, sori.PackageOptions{
    Format: sori.ArtifactFormatChunkedCAS,
})
if err != nil { ... }

// Fetch — chunked/legacy 자동 감지
vi, err := sori.FetchVolSeq(ctx, "./dest", storePath, "star:v2.7.10")
if err != nil { ... }

// 사후 무결성 검증 (M-12)
err = chunked.Fetch(ctx, storePath, "./dest", "star:v2.7.10", chunked.FetchOptions{
    VerifyTree: true, // fetch 완료 후 destRoot 전체 sha256 재검증
})
```

**실측 벤치마크** (Xeon E5-2683 v4, 1 GiB 청크, gate violations 없음):

| fixture | size | push | fetch | peak RSS |
|---|---|---|---|---|
| synthetic-1GiB | 1 GiB | 11.9 s | 9.3 s | 7.9 MiB |
| synthetic-10GiB | 10 GiB | 54.8 s | 41.1 s | 10.4 MiB |
| genomics-bwa | 15 GiB | 75.5 s | 53.8 s | 10.6 MiB |
| genomics-star | 40 GiB | 194.5 s | 147.7 s | 10.4 MiB |

RSS가 데이터 크기와 무관하게 11 MiB 이하로 유지된다 (스트리밍 청킹). 전체 결과: [`docs/research/results-summary.md`](docs/research/results-summary.md)

## 설정 파일 (`sori-oci.json`)

```json
{
  "local": {
    "type": "oci",
    "path": "/path/to/writable/oci-store"
  },
  "remotes": [
    {
      "name": "harbor",
      "type": "registry",
      "registry": "harbor.local",
      "repository": "project/repo",
      "tls": { "insecure": false, "ca_file": "" },
      "auth": { "username": "admin", "password": "${SORI_REGISTRY_PASSWORD}", "token": "" }
    }
  ]
}
```

> `/var/lib/sori/oci` (기본값)는 root 권한이 필요하다. 개발/테스트 시에는 `path`를 쓰기 가능한 경로로 설정할 것.

## 공개 API

새 코드는 `Stable API`로 분류된 경로를 우선 사용하고, 호환용 wrapper는 신규 사용처에서 피하는 편이 좋다.

### Config

```go
func LoadConfig(path string) (*Config, error)
func InitConfig(path string) (*Config, error)          // deprecated 호환용 로더
func (conf *Config) EnsureDir() error
func (conf *Config) NewClient(opts ...ClientOption) *Client
```

### Client

```go
type Client struct { ... }
type ClientOption func(*Client)

type PackageOptions struct {
    ConfigBlob        []byte
    RequireConfigBlob bool
    Format            ArtifactFormat  // 0 = ArtifactFormatLegacy (default), ArtifactFormatChunkedCAS
    DatasetMetadata   []byte          // optional; written as .sori/dataset-metadata.json on fetch
    Progress          chunked.ProgressFunc
}

type PushOptions struct { Target RemoteTarget }

type FetchOptions struct {
    Concurrency             int
    RequireEmptyDestination bool
    AtomicOverwrite         bool  // 3-phase overwrite: staging → backup → rename
}

type ReferrerOptions struct { Target RemoteTarget }

func NewClient(opts ...ClientOption) *Client
func WithLocalStorePath(path string) ClientOption
func WithHTTPClient(httpClient *http.Client) ClientOption
func WithClock(now func() time.Time) ClientOption

func (c *Client) LocalStorePath() string
func (c *Client) PackageVolume(ctx context.Context, req PackageRequest) (*PackageResult, error)
func (c *Client) PackageVolumeWithOptions(ctx context.Context, req PackageRequest, opts PackageOptions) (*PackageResult, error)
func (c *Client) PushPackagedVolume(ctx context.Context, pkg *PackageResult, target RemoteTarget) (*PushResult, error)
func (c *Client) PushPackagedVolumeWithOptions(ctx context.Context, pkg *PackageResult, opts PushOptions) (*PushResult, error)
func (c *Client) FetchVolume(ctx context.Context, destRoot, repo, tag string, opts FetchOptions) (*VolumeIndex, error)
func (c *Client) FetchVolumeFresh(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error)
func (c *Client) FetchVolumeSequential(ctx context.Context, destRoot, repo, tag string) (*VolumeIndex, error)
func (c *Client) FetchVolumeParallel(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error)
func (c *Client) FetchVolumeFromRemote(ctx context.Context, destRoot string, target RemoteTarget, tag string, opts FetchOptions) (*VolumeIndex, error)
func (c *Client) PublishVolume(ctx context.Context, vi *VolumeIndex, volPath, volName string, configBlob []byte) (*VolumeIndex, error)
func (c *Client) PublishVolumeFromDir(ctx context.Context, volDir, displayName, tag string) (*PackageResult, error)
```

### VolumeIndex / 생성

```go
func GenerateVolumeIndex(rootPath, displayName string) (*VolumeIndex, error)
func (vi *VolumeIndex) SaveToFile(rootPath string) error
func (vi *VolumeIndex) PublishVolume(ctx, volPath, volName string, configBlob []byte) (*VolumeIndex, error)
```

**파티션 모델**: `GenerateVolumeIndex`는 `rootPath` 바로 아래의 1단계 서브디렉터리만 파티션으로 등록한다. 중첩 디렉터리는 상위 파티션의 레이어에 포함된다. `rootPath` 바로 아래의 일반 파일(예: `README.md`)은 별도 root-files 레이어로 패키징되어 fetch 시 복원된다.

**fetch 복원 보장**: `FetchVolSeq` / `FetchVolParallel` 완료 후 `configblob.json`과 `volume-index.json`이 `destRoot` 하위에 원자적으로 기록된다. DisplayName과 CreatedAt은 OCI manifest annotation에서 복원된다.

`PublishVolume`은 각 레이어 descriptor에 `"org.example.partitionPath"` 어노테이션을 설정한다.  
이 어노테이션이 없으면 `FetchVolSeq` / `FetchVolParallel` 시 오류가 발생하므로 직접 descriptor를 만들 때 반드시 포함해야 한다.

### CollectionManager

```go
func NewCollectionManager(rootDir string, initial ...VolumeEntry) (*CollectionManager, error)
func (m *CollectionManager) AddOrUpdate(v VolumeEntry) error
func (m *CollectionManager) Remove(ref string) (bool, error)
func (m *CollectionManager) Get(ref string) (VolumeEntry, bool)
func (m *CollectionManager) GetSnapshot() VolumeCollection
func (m *CollectionManager) Flush() error
func (m *CollectionManager) PublishVolumeFromDir(ctx, volDir, displayName, tag string) error
```

### 상위 package / dataspec API

```go
type PackageRequest struct {
    SourceDir   string
    DisplayName string
    Tag         string
    Dataset     string
    Version     string
    StableRef   string
    Description string
    Annotations map[string]string
    ConfigBlob  []byte
}

type PackageResult struct {
    StableRef      string
    LocalTag       string
    ManifestDigest string
    ConfigDigest   string
    TotalSize      int64
    CreatedAt      string
    Partitions     []Partition
    VolumeIndex    VolumeIndex
}

type RemoteTarget struct {
    Registry    string
    Repository  string
    PlainHTTP   bool
    InsecureTLS bool
    Username    string
    Password    string
    Token       string
    CAFile      string
}

func PackageVolume(ctx context.Context, req PackageRequest) (*PackageResult, error)
func PackageVolumeToStore(ctx context.Context, localStorePath string, req PackageRequest) (*PackageResult, error)
func PushPackagedVolume(ctx context.Context, localStorePath string, pkg *PackageResult, target RemoteTarget) (*PushResult, error)
func BuildDataSpec(pkg *PackageResult, push *PushResult, req PackageRequest) (*DataSpec, error)
func PushRemoteDataSpecReferrer(ctx context.Context, push *PushResult, target RemoteTarget, spec *DataSpec) (*ReferrerPushResult, error)
```

이 계층은 `CollectionManager` 없이도 `NodeVault` 같은 상위 서비스가 `package → push → metadata 생성` 흐름을 바로 이어붙일 수 있게 하기 위한 API다.

### Generic Metadata

```go
const ArtifactMetadataSchemaVersion = "sori.artifact.v1"
const DatasetMetadataSchemaVersion = "sori.dataset.metadata.v1"
const MediaTypeDatasetMetadata = "application/vnd.sori.dataset.metadata.v1+json"
const MediaTypeChunkedConfig = "application/vnd.sori.chunked-cas.config.v1+json"
const MediaTypeChunkIndex = "application/vnd.sori.chunk-index.v1+json"

type ArtifactMetadata struct { ... }
type ArtifactMetadataInput struct { ... }
type DatasetMetadata struct { ... }
type ChunkIndex struct { ... }

func BuildArtifactMetadata(input ArtifactMetadataInput, pkg *PackageResult, push *PushResult) (*ArtifactMetadata, error)
func ValidateDatasetMetadata(meta *DatasetMetadata) error
func ValidateDatasetMetadataJSON(data []byte) error
func ArtifactMetadataToDataSpec(meta *ArtifactMetadata) *DataSpec
func ArtifactMetadataToRegisteredDataDefinition(meta *ArtifactMetadata, req DataRegisterRequest) *RegisteredDataDefinition
```

`ArtifactMetadata`는 core 계층의 중립 metadata 모델이다. `DataSpec`과 `RegisteredDataDefinition`은 이 모델을 NodeVault 친화 구조로 변환한 adapter 결과다.
`DatasetMetadata`는 OCI artifact에 `dataset-metadata.json` 레이어로 포함되는 catalog/operator용 설명 모델이며, 외부 CLI나 catalog indexer가 안정적으로 사용할 수 있는 public API다.

### Experimental: OCI Referrer Push

이미지 digest에 대한 referrer artifact를 Harbor 등의 OCI 레지스트리에 push하는 실험적 API.  
NodeVault의 toolspec / toolprofile / security referrer 축을 지원한다.

#### Media type 상수

```go
const (
    MediaTypeToolSpec    = "application/vnd.nodevault.toolspec.v1+json"
    MediaTypeDataSpec    = "application/vnd.nodevault.dataspec.v1+json"
    MediaTypeToolProfile = "application/vnd.nodevault.toolprofile.v1+json"
    MediaTypeSecurityScan = "application/vnd.nodevault.security.v1+json"
)
```

#### ReferrerTarget / SpecReferrerResult

```go
type ReferrerTarget interface {
    oras.Target
}

type SpecReferrerResult struct {
    ReferrerDigest string
    SubjectDigest  string
    MediaType      string
}
```

#### Push 함수

```go
// toolspec: 등록 시점 declared spec referrer
func PushToolSpecReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, specJSON []byte) (SpecReferrerResult, error)

// dataspec: data artifact referrer
func PushDataSpecReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, specJSON []byte) (SpecReferrerResult, error)

// toolprofile: observed dry-run profile referrer (validationHash, observedIoProfile 등)
func PushToolProfileReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, profileJSON []byte) (SpecReferrerResult, error)

// security: CVE scan result referrer (trivy-operator 연동)
func PushSecurityReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, securityJSON []byte) (SpecReferrerResult, error)
```

#### Target 생성 헬퍼

```go
func NewReferrerLocalStore(path string) (ReferrerTarget, error)
func NewReferrerRemoteRepository(repoRef string, plainHTTP bool, credential *auth.Credential) (ReferrerTarget, error)
func MarshalSpec(v interface{}) ([]byte, error)
```

#### 사용 예시

```go
import "github.com/HeaInSeo/sori"

// 원격 Harbor에 toolspec referrer push
target, err := sori.NewReferrerRemoteRepository(
    "harbor.local/project/bwa-mem2:latest",
    true,
    &auth.Credential{Username: "user", Password: "pass"},
)
if err != nil { ... }

specJSON, _ := sori.MarshalSpec(myToolSpec)
result, err := sori.PushToolSpecReferrer(ctx, target, "sha256:IMAGE_DIGEST", specJSON)
if err != nil { ... }
fmt.Println(result.ReferrerDigest)

// toolprofile referrer push (dry-run 결과)
profileJSON, _ := sori.MarshalSpec(myProfile)
profileResult, err := sori.PushToolProfileReferrer(ctx, target, "sha256:IMAGE_DIGEST", profileJSON)

// security scan referrer push
secJSON, _ := sori.MarshalSpec(mySecuritySummary)
secResult, err := sori.PushSecurityReferrer(ctx, target, "sha256:IMAGE_DIGEST", secJSON)
```

### 검증 유틸리티

```go
func ValidateVolumeDir(volDir string) ([]byte, error)
// - 빈 디렉터리 → 에러
// - configblob.json 없으면 빈 JSON으로 생성
// - configblob.json 있으면 로드해 반환
```

### 원격 push / fetch

```go
type PushResult struct {
    Reference      string
    Repository     string
    Tag            string
    ManifestDigest string
}

func PushLocalToRemote(ctx, localRepoPath, tag, remoteRepo, user, pass string, plainHTTP bool) (*PushResult, error)
func FetchVolSeq(ctx, destRoot, repo, tag string) (*VolumeIndex, error)
func FetchVolParallel(ctx, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error)
```

`PushLocalToRemote`, `PackageVolume`, `VolumeIndex.PublishVolume`은 호환용 low-level wrapper다.  
새 코드는 `Client` 기반 API 사용을 권장한다.

원격 Harbor가 HTTPS와 사설 CA를 사용하는 경우 `RemoteTarget.CAFile`에 PEM 경로를 주면 TLS root CA에 반영된다.

### Error 모델

```go
var (
    ErrValidation error
    ErrNotFound   error
    ErrConflict   error
    ErrIntegrity  error
    ErrTransport  error
    ErrAuth       error
)
```

주요 public 함수는 이 에러 종류를 감싼 typed error를 반환한다. 호출자는 `errors.Is(err, sori.ErrValidation)` 같은 식으로 분기할 수 있다.

추가 정책:
- `PackageOptions.RequireConfigBlob=true`이면 `configblob.json` 자동 생성을 허용하지 않고, 호출자가 config blob을 명시적으로 제공해야 한다.

**Fetch 안전도 모드** (안전한 순서):

| 모드 | 설정 | 동작 |
|------|------|------|
| **AtomicOverwrite** | `FetchOptions{AtomicOverwrite: true}` | staging 추출 → 기존 `destRoot` 백업 → staging rename. 갱신 워크플로에 권장. |
| **FetchVolumeFresh** | `Client.FetchVolumeFresh(...)` | `RequireEmptyDestination:true` 묵시적 wrapper. 최초 설치용 zero-config API. |
| **Safe fetch** | `FetchOptions{RequireEmptyDestination: true}` | `destRoot`가 없어야 시작. staging 추출 후 성공 시 rename. |
| **Remote fetch** | `Client.FetchVolumeFromRemote` | 기본값이 safe fetch. `AtomicOverwrite` opt-in 지원. |
| **Legacy direct** | `FetchOptions{}` (기본) | `destRoot`에 직접 추출. 중간 실패 시 부분 상태 가능. 하위 호환용만. |

`RequireEmptyDestination`과 `AtomicOverwrite`를 동시에 설정하면 `ErrValidation`을 반환한다.

### 등록 / Catalog API

```go
type DataRegisterRequest struct {
    RequestID   string
    DataName    string
    Version     string
    Description string
    Format      string
    SourceURI   string
    Checksum    string
    StorageURI  string
    StableRef   string
    Display     DisplaySpec
}

type RegisteredDataDefinition struct {
    CASHash         string
    DataName        string
    Version         string
    Description     string
    Format          string
    SourceURI       string
    Checksum        string
    StorageURI      string
    StableRef       string
    Display         DisplaySpec
    RegisteredAt    int64
    LifecyclePhase  string
    IntegrityHealth string
}

func BuildRegisteredDataDefinition(req DataRegisterRequest, pkg *PackageResult, push *PushResult) (*RegisteredDataDefinition, error)
func RegisterPackagedData(ctx context.Context, rootDir string, req DataRegisterRequest, pkg *PackageResult, push *PushResult) (*DataRegisterResponse, error)
func NewDataCatalog(rootDir string) *DataCatalog
func (c *DataCatalog) Get(casHash string) (*RegisteredDataDefinition, error)
func (c *DataCatalog) List(stableRef string) ([]RegisteredDataDefinition, error)
```

이 계층은 `NodeKit`의 `DataRegisterRequest`와 `Catalog`의 `AdminDataList` 사이를 잇는 최소 로컬 구현이다.  
현재는 `rootDir/registered-data.json`에 저장한다.

### tar.gz 유틸리티

```go
func TarGzDir(fsDir, prefixPath string) ([]byte, error)   // 디렉터리 전체 결정론적 tar.gz 생성
func UntarGzDir(gzipStream io.Reader, dest string) error   // tar.gz 해제 (path traversal 방어 포함)
```

`archiveutil.TarGzDirFiles(fsDir, prefixPath string, skipNames map[string]struct{})` — root-level 일반 파일만 tar.gz로 묶는다(서브디렉터리 제외). `PublishVolume`이 내부적으로 root-files 레이어 생성에 사용한다.

> **symlink 미지원**: `TarGzDir` / `TarGzDirFiles` 는 symlink를 만나면 `ErrValidation`을 반환한다. 패키징 대상 디렉터리에 symlink가 포함되어 있으면 사전에 제거하거나 실제 파일로 대체해야 한다.

## API 안정도

| 계층 | 포함 항목 |
|------|-----------|
| **Stable** | `Config.NewClient`, `Client` 기반 package/push/fetch, `ArtifactFormatChunkedCAS`, `chunked.Publish` / `chunked.Fetch`, `FetchOptions.VerifyTree`, `BuildArtifactMetadata`, typed error, option 모델 |
| **Compatibility** | `InitConfig`, `PackageVolume`, `PushLocalToRemote`, `VolumeIndex.PublishVolume` |
| **Experimental** | `DataSpec`, referrer API (`PushToolSpecReferrer` / `PushToolProfileReferrer` / `PushSecurityReferrer` / `PushDataSpecReferrer`), registration/catalog API |

자세한 목록은 [docs/public-api.md](docs/public-api.md)를 따른다.

## 테스트 실행

```bash
# 단위 테스트 (외부 인프라 불필요)
go test -short ./...

# 특정 테스트만
go test -v -run "TestGenerateAndSaveVolumeIndex|TestTarGzDirDeterministic|TestExtractTarGz|TestMerge|TestLoadOrNewCollection_New|TestManager|TestValidateVolumeDir|TestLoadConfig_TempDir|TestPublishFetchRoundTrip" ./...

# 전체 테스트 (로컬 OCI 스토어 필요)
go test -v ./...
```

root 권한이 없는 환경에서는 `TestLoadConfig` / `TestInitConfig`가 자동으로 skip된다.  
`TestLoadConfig_TempDir`으로 동일 기능을 검증할 수 있다.

## 알려진 제한 사항

- referrer 조회(list referrers) API는 미구현. 현재는 push까지만 제공한다.
- `toolprofile` / `security` referrer push는 payload 구조를 호출자가 직접 구성해야 한다. payload 스펙은 NodeVault의 `docs/OBSERVED_PROFILE_SPEC.md`, `docs/SECURITY_SCAN_SPEC.md` 참조.
- 과거 stub였던 `local-registry.go`, `pipeline-index.go`, `oci-crud.go`는 제거했다. 판단 배경은 [docs/stub-status.md](docs/stub-status.md) 참조.

### P3 backlog (연기된 항목)

상세 설계는 [p3-backlog.md](p3-backlog.md) 참조.

| 항목 | 현재 상태 | 해결 방향 |
|------|-----------|-----------|
| **Streaming tar/push** | Step 1 완료: 레이어를 temp 파일에 기록(메모리 버퍼 제거). Step 2(진정한 스트리밍 push)는 미설계 | ORAS `Store.Push` streaming 모드 검토 후 구현 |
| ✅ **Chunked CAS** | `ArtifactFormatChunkedCAS` stable 승격 완료 (v0.7.0-stable). 1 GiB 청크, 스트리밍, M-12 tree verify 포함. | — |
| ✅ **Remote registry fetch** | `Client.FetchVolumeFromRemote` 추가 완료 | — |
| ✅ **Staging 고도화** | safe fetch(`RequireEmptyDestination`) + 3-phase `AtomicOverwrite` + `validateStagingDir` 모두 완료 | — |

## 라이선스

Apache License 2.0
