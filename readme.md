# sori

OCI 기반 참조 데이터(볼륨) 패키징 및 referrer push 라이브러리.  
디렉터리를 OCI 아티팩트로 변환하고, 로컬 OCI 스토어와 원격 레지스트리(Harbor 등) 사이의 push/fetch를 담당한다.

## 개요

`sori`는 바이오인포매틱스 파이프라인에서 사용하는 참조 데이터(genome, annotation 등)를  
OCI 이미지 형식으로 패키징하는 Go 라이브러리다.

- **Core**: OCI volume packaging + push/fetch + artifact metadata
- **Experimental**: NodeVault-oriented referrer push (toolspec / toolprofile / security / dataspec)

| 문서 | 내용 |
|------|------|
| [docs/public-api.md](docs/public-api.md) | 공개 API 안정도 분류 |
| [docs/generalization-sprint-plan.md](docs/generalization-sprint-plan.md) | 범용 라이브러리화 로드맵 |
| [docs/maturity-sprint-plan.md](docs/maturity-sprint-plan.md) | 성숙화 계획 |
| [docs/stable-api-promotion.md](docs/stable-api-promotion.md) | stable API 승격 검토 항목 |
| [docs/followup-sprint-plan.md](docs/followup-sprint-plan.md) | 후속 스프린트 계획 |
| [docs/registry-integration.md](docs/registry-integration.md) | registry 통합 테스트 골격 |
| [docs/operations.md](docs/operations.md) | 운영 환경 체크리스트 |
| [docs/stub-status.md](docs/stub-status.md) | 과거 stub 처리 내역 |

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
```

단계 `4~6`은 stable core 흐름이고, `7~9`는 experimental 계층이다.

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
      "auth": { "username": "admin", "password": "Harbor12345", "token": "" }
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
type PackageOptions struct { ConfigBlob []byte }
type PushOptions struct { Target RemoteTarget }
type FetchOptions struct {
    Concurrency int
    RequireEmptyDestination bool
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
func (c *Client) FetchVolumeSequential(ctx context.Context, destRoot, repo, tag string) (*VolumeIndex, error)
func (c *Client) FetchVolumeParallel(ctx context.Context, destRoot, repo, tag string, concurrency int) (*VolumeIndex, error)
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

type ArtifactMetadata struct { ... }
type ArtifactMetadataInput struct { ... }

func BuildArtifactMetadata(input ArtifactMetadataInput, pkg *PackageResult, push *PushResult) (*ArtifactMetadata, error)
func ArtifactMetadataToDataSpec(meta *ArtifactMetadata) *DataSpec
func ArtifactMetadataToRegisteredDataDefinition(meta *ArtifactMetadata, req DataRegisterRequest) *RegisteredDataDefinition
```

`ArtifactMetadata`는 core 계층의 중립 metadata 모델이다. `DataSpec`과 `RegisteredDataDefinition`은 이 모델을 NodeVault 친화 구조로 변환한 adapter 결과다.

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
- `FetchOptions.RequireEmptyDestination=true`이면 `destRoot`가 이미 존재하면(비어 있어도) `ErrConflict`를 반환한다. 또한 staging 디렉터리(`destRoot` 옆 `.staging-*`)에 먼저 추출한 뒤 성공 시 원자적 rename으로 `destRoot`를 채우므로, 중간 실패가 발생해도 `destRoot`는 untouched 또는 완전히 채워진 상태 중 하나임이 보장된다.

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

## API 안정도

| 계층 | 포함 항목 |
|------|-----------|
| **Stable** | `Config.NewClient`, `Client` 기반 package/push/fetch, `BuildArtifactMetadata`, typed error, option 모델 |
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
| **Streaming tar/push** | 레이어 전체를 메모리에 올림. 대용량 데이터셋에서 OOM 위험 | `io.Writer` 기반 스트리밍 파이프라인으로 교체 |
| **Remote registry fetch** | local OCI store에서만 추출 가능. 원격 레지스트리 직접 fetch 경로 없음 | `Client.FetchVolumeFromRemote` 추가 |
| **Staging 고도화** | `RequireEmptyDestination=true` + rename으로 fresh fetch는 원자적. 업데이트(덮어쓰기) 및 commit 전 무결성 검증은 미구현 | validate-before-commit, 3-phase overwrite 설계 필요 |

## 라이선스

Apache License 2.0
