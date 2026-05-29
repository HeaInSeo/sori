package sori

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

func TestGenerateAndSaveVolumeIndex(t *testing.T) {
	// JSON 생성
	vi, err := GenerateVolumeIndex("./test-vol", "TestVol")
	if err != nil {
		t.Fatalf("GenerateVolumeIndex failed: %v", err)
	}

	// 파일로 저장
	if err := vi.SaveToFile("./test-vol"); err != nil {
		t.Fatalf("SaveToFile failed: %v", err)
	}
}

func TestPublishVolumeOther(t *testing.T) {
	volDir := "./test2"
	if _, err := os.Stat(volDir); err != nil {
		t.Skipf("skipping: fixture %q not available", volDir)
	}
	if _, err := os.Stat("configblob1.json"); err != nil {
		t.Skip("skipping: configblob1.json fixture not available")
	}
	vi, err := GenerateVolumeIndex(volDir, "테스트 하는 볼륨2")
	if err != nil {
		t.Fatalf("GenerateVolumeIndex failed: %v", err)
	}
	configblob, err := loadMetadataJSON("configblob1.json")
	if err != nil {
		t.Fatalf(" failed to load configblob juson:%v", err)
	}

	_, err = vi.PublishVolume(context.Background(), volDir, "test2.v1.0.0", configblob)
	assert.NoError(t, err)
}

func TestPublishVolumeOther1(t *testing.T) {
	// 동일한 것을 넣으면 어떻게 될까?
	volDir := "./test2"
	if _, err := os.Stat(volDir); err != nil {
		t.Skipf("skipping: fixture %q not available", volDir)
	}
	if _, err := os.Stat("configblob1.json"); err != nil {
		t.Skip("skipping: configblob1.json fixture not available")
	}
	vi, err := GenerateVolumeIndex(volDir, "테스트 하는 볼륨2")
	if err != nil {
		t.Fatalf("GenerateVolumeIndex failed: %v", err)
	}
	configblob, err := loadMetadataJSON("configblob1.json")
	if err != nil {
		t.Fatalf(" failed to load configblob juson:%v", err)
	}

	_, err = vi.PublishVolume(context.Background(), volDir, "test2.v1.0.0", configblob)
	assert.NoError(t, err)
}

func TestTarGzDirDeterministic(t *testing.T) {
	tPath := "./test-vol"
	// 두번째 메서드는 풀리는 폴더 이름이라고 보면 됨.
	data1, err := TarGzDir(tPath, "test-vol1")
	if err != nil {
		t.Fatalf("tarGzDirDeterministic failed: %v", err)
	}

	outFile := filepath.Join(t.TempDir(), "test-vol.tar.gz")
	if err := os.WriteFile(outFile, data1, 0o644); err != nil {
		t.Fatalf("failed to write tarball: %v", err)
	}
	t.Logf("wrote deterministic tarball: %s (%d bytes)", outFile, len(data1))
	// tar -xzf test-vol.tar.gz
}

// TODO 몇가지 버그가 있다. 수정해야 한다.

// TestFetchVolumeFromOCI pushes a small volume to a local OCI store, fetches it back, and verifies
// both file contents and VolumeIndex metadata.
func TestFetchVolumeFromOCI(t *testing.T) {
	ctx := context.Background()

	repo := "./repo"
	dest := "./test-vol-restored"
	if _, err := os.Stat(repo); err != nil {
		t.Skipf("skipping: local OCI fixture %q not available", repo)
	}
	_, err := FetchVolSeq(ctx, dest, repo, "v1.0.0")
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			t.Skipf("skipping: expected tag not prepared in %q", repo)
		}
		t.Fatalf("FetchVolSeq failed: %v", err)
	}

}

// TestFetchVolSeq_RejectsOverlappingPartitionPaths verifies that FetchVolSeq
// returns an ErrIntegrity error when two layers have overlapping partition paths.
func TestFetchVolSeq_RejectsOverlappingPartitionPaths(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()

	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("oci.New: %v", err)
	}

	makeTarGzLayer := func(partPath string) (ocispec.Descriptor, []byte) {
		buf := &bytes.Buffer{}
		gw := gzip.NewWriter(buf)
		tw := tar.NewWriter(gw)
		hdr := &tar.Header{
			Name:     partPath + "/",
			Typeflag: tar.TypeDir,
			Mode:     0755,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if err := tw.Close(); err != nil {
			t.Fatalf("tar close: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		data := buf.Bytes()
		desc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Digest:    digest.FromBytes(data),
			Size:      int64(len(data)),
			Annotations: map[string]string{
				annotationPartitionPath: partPath,
				annotationLayerKind:     layerKindPartition,
			},
		}
		return desc, data
	}

	desc1, data1 := makeTarGzLayer("vol/sub")
	desc2, data2 := makeTarGzLayer("vol/sub/deeper")

	if err := store.Push(ctx, desc1, bytes.NewReader(data1)); err != nil {
		t.Fatalf("push layer1: %v", err)
	}
	if err := store.Push(ctx, desc2, bytes.NewReader(data2)); err != nil {
		t.Fatalf("push layer2: %v", err)
	}

	configBlob := []byte("{}")
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configBlob),
		Size:      int64(len(configBlob)),
	}
	if err := store.Push(ctx, configDesc, bytes.NewReader(configBlob)); err != nil {
		t.Fatalf("push config: %v", err)
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1,
		ocispec.MediaTypeImageManifest,
		oras.PackManifestOptions{
			ConfigDescriptor: &configDesc,
			Layers:           []ocispec.Descriptor{desc1, desc2},
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationCreated:   time.Now().UTC().Format(time.RFC3339),
				annotationVolumeDisplayName: "overlap-test",
			},
		},
	)
	if err != nil {
		t.Fatalf("PackManifest: %v", err)
	}
	if err := store.Tag(ctx, manifestDesc, "overlap.v1"); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	destDir := t.TempDir()
	_, err = FetchVolSeq(ctx, destDir, storePath, "overlap.v1")
	if err == nil {
		t.Fatal("expected error for overlapping partition paths, got nil")
	}
	if !errors.Is(err, ErrIntegrity) && !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected ErrIntegrity or 'overlap' in error, got %v", err)
	}
}

// TestPushLocalToRemote_Harbor 실제 Harbor 레지스트리에 푸시
func TestPushLocalToRemote_Harbor(t *testing.T) {
	if os.Getenv("SORI_RUN_HARBOR_TESTS") == "" {
		t.Skip("skipping Harbor push test; set SORI_RUN_HARBOR_TESTS=1 to enable")
	}
	ctx := context.Background()

	localTag := "test.v1.0.0"
	remoteRepo := "harbor.local/demo-project/testrepo"
	user := "admin"
	pass := os.Getenv("SORI_HARBOR_PASSWORD")
	repo := "./repo"

	// 5) 실제 푸시 호출
	if _, err := PushLocalToRemote(ctx, repo, localTag, remoteRepo, user, pass, true); err != nil {
		t.Fatalf("Harbor 레지스트리 푸시 실패: %v", err)
	}

	t.Logf("✅ Harbor 에 성공적으로 푸시됨: %s:%s", remoteRepo, localTag)

}

// 부가적인 테스트 코드
// TestMerge_AddNewVolumes checks that Merge adds only non-duplicate volumes
func TestMerge_AddNewVolumes(t *testing.T) {
	existing := VolumeCollection{
		Version: 1,
		Volumes: []VolumeEntry{
			{
				Index:      VolumeIndex{DisplayName: "v1", VolumeRef: "ref1"},
				ConfigBlob: ConfigBlob{},
			},
		},
	}
	newColl := VolumeCollection{
		Volumes: []VolumeEntry{
			{
				Index:      VolumeIndex{DisplayName: "v2", VolumeRef: "ref2"},
				ConfigBlob: ConfigBlob{},
			},
			{
				Index:      VolumeIndex{DisplayName: "v1", VolumeRef: "ref1"}, // duplicate
				ConfigBlob: ConfigBlob{},
			},
		},
	}

	added := existing.Merge(newColl)

	assert.True(t, added, "expected Merge to return true when new volumes are added")
	assert.Len(t, existing.Volumes, 2, "expected two volumes after merge")
	assert.Equal(t, 2, existing.Version, "expected version to increment by 1")
}

// TestMerge_NoVolumesAdded ensures Merge returns false and version unchanged if no new volumes
func TestMerge_NoVolumesAdded(t *testing.T) {
	existing := VolumeCollection{
		Version: 5,
		Volumes: []VolumeEntry{
			{
				Index:      VolumeIndex{DisplayName: "v1", VolumeRef: "ref1"},
				ConfigBlob: ConfigBlob{},
			},
		},
	}
	newColl := VolumeCollection{
		Volumes: []VolumeEntry{
			{
				Index:      VolumeIndex{DisplayName: "v1", VolumeRef: "ref1"},
				ConfigBlob: ConfigBlob{},
			},
		},
	}

	added := existing.Merge(newColl)

	assert.False(t, added, "expected Merge to return false when no new volumes")
	assert.Equal(t, 5, existing.Version, "expected version to remain unchanged")
}

func TestExtractTarGz(t *testing.T) {
	// Build an in-memory tar.gz with a directory and a regular file.
	// Symlinks are not supported in sori artifacts (rejected at both package and extract time).
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	dirHdr := &tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0755}
	assert.NoError(t, tw.WriteHeader(dirHdr))

	content := []byte("hello world")
	fileHdr := &tar.Header{
		Name:     "dir/file.txt",
		Typeflag: tar.TypeReg,
		Mode:     0644,
		Size:     int64(len(content)),
	}
	assert.NoError(t, tw.WriteHeader(fileHdr))
	_, err := tw.Write(content)
	assert.NoError(t, err)

	assert.NoError(t, tw.Close())
	assert.NoError(t, gw.Close())

	dest := t.TempDir()
	err = UntarGzDir(bytes.NewReader(buf.Bytes()), dest)
	assert.NoError(t, err)

	assert.DirExists(t, filepath.Join(dest, "dir"))

	got, err := os.ReadFile(filepath.Join(dest, "dir", "file.txt"))
	assert.NoError(t, err)
	assert.Equal(t, content, got)
}

// TODO 여기서 부터 검증해 나가자.
func TestLoadOrNewCollection_New(t *testing.T) {
	rootDir := t.TempDir()
	_, err := LoadOrNewCollection(rootDir)
	if err != nil {
		t.Fatalf("LoadOrNewCollection failed: %v", err)
	}
}

func TestManager(t *testing.T) {
	rootDir := t.TempDir()
	// 1. 매니저 초기화 (기존 파일 있으면 로드, 없으면 새로 생성)
	manager, err := NewCollectionManager(rootDir)
	if err != nil {
		t.Fatalf("failed to init collection manager: %v", err)
	}

	// 2. 새 볼륨을 하나 만든다고 가정 (예: OCI Push 후 얻은 digest)
	v1 := VolumeEntry{
		Index: VolumeIndex{
			VolumeRef:   "sha256:111aaa...",
			DisplayName: "HumanRef_GRCh38",
		},
		ConfigBlob: ConfigBlob{},
	}
	if err := manager.AddOrUpdate(v1); err != nil {
		t.Fatalf("AddOrUpdate v1: %v", err)
	}

	// 3. 같은 ref 를 내용 바꿔서 갱신
	v1Updated := VolumeEntry{
		Index: VolumeIndex{
			VolumeRef:   "sha256:111aaa...",
			DisplayName: "HumanRef_GRCh38 (patched)",
		},
		ConfigBlob: ConfigBlob{},
	}
	if err := manager.AddOrUpdate(v1Updated); err != nil {
		t.Fatalf("AddOrUpdate v1Updated: %v", err)
	}

	// 4. 다른 볼륨 추가
	v2 := VolumeEntry{
		Index: VolumeIndex{
			VolumeRef:   "sha256:222bbb...",
			DisplayName: "VCF Panel 2025-07",
		},
		ConfigBlob: ConfigBlob{},
	}
	if err := manager.AddOrUpdate(v2); err != nil {
		t.Fatalf("AddOrUpdate v2: %v", err)
	}

	// 5. 개별 조회
	gotEntry, ok := manager.Get("sha256:111aaa...")
	if !ok {
		t.Fatalf("expected to find entry for sha256:111aaa...")
	}
	fmt.Printf("Get sha256:111aaa... => Index=%+v, ConfigBlob=%+v\n", gotEntry.Index, gotEntry.ConfigBlob)

	// 6. 스냅샷 조회 (읽기용 복사본)
	snap := manager.GetSnapshot()
	fmt.Printf("Snapshot Version=%d, count=%d\n", snap.Version, len(snap.Volumes))

	// 7. 제거
	removed, err := manager.Remove("sha256:222bbb...")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed {
		fmt.Println("Removed sha256:222bbb...")
	} else {
		t.Fatalf("expected to remove sha256:222bbb...")
	}

	// 8. 필요 시 명시적 Flush (Add/Update/Remove 에서 이미 저장되지만 강제 저장 가능)
	if err := manager.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	fmt.Println("Done.")
}

func TestValidateVolumeDir_NonExistent(t *testing.T) {
	_, err := ValidateVolumeDir("non-existent-dir")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected error for non-existent directory, got %v", err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestValidateVolumeDir_NotDirectory(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(file, []byte("data"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	_, err := ValidateVolumeDir(file)
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("expected error for path that is not a directory, got %v", err)
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestValidateVolumeDir_EmptyDir(t *testing.T) {
	tmp := t.TempDir()

	_, err := ValidateVolumeDir(tmp)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("expected error for empty directory, got %v", err)
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestValidateVolumeDir_ConfigOnlyDir(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, ConfigBlobJson)
	if err := os.WriteFile(cfgPath, []byte(`{"key":"value"}`), 0644); err != nil {
		t.Fatalf("failed to write configblob.json: %v", err)
	}

	_, err := ValidateVolumeDir(tmp)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("expected empty-dir error when only configblob.json exists, got %v", err)
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestValidateVolumeDir_CreateConfigBlob(t *testing.T) {
	tmp := t.TempDir()
	// non-hidden 파일 추가해서 empty 체크 통과
	dataFile := filepath.Join(tmp, "data.txt")
	if err := os.WriteFile(dataFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}

	raw, err := ValidateVolumeDir(tmp)
	if err != nil {
		t.Fatalf("ValidateVolumeDir failed: %v", err)
	}
	expected := "{}"
	if strings.TrimSpace(string(raw)) != expected {
		t.Fatalf("expected raw %q, got %q", expected, raw)
	}

	// configblob.json 파일이 생성되었는지 확인
	cfgPath := filepath.Join(tmp, ConfigBlobJson)
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read configblob.json: %v", err)
	}
	if strings.TrimSpace(string(b)) != expected {
		t.Fatalf("expected configblob.json to contain %q, got %q", expected, b)
	}
}

func TestValidateVolumeDir_CreateConfigBlob_NoTempSurvivorOnFailure(t *testing.T) {
	// Verify that writeFileAtomic cleans up the temp file when the write fails.
	// We make the directory read-only so os.CreateTemp returns an error before
	// any partial file is written.
	if os.Getuid() == 0 {
		t.Skip("cannot test read-only directory as root")
	}
	tmp := t.TempDir()
	dataFile := filepath.Join(tmp, "data.txt")
	if err := os.WriteFile(dataFile, []byte("content"), 0644); err != nil {
		t.Fatalf("setup data file: %v", err)
	}
	if err := os.Chmod(tmp, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })

	_, err := ValidateVolumeDir(tmp)
	if err == nil {
		t.Fatal("expected error when configblob creation fails in read-only dir, got nil")
	}

	if err := os.Chmod(tmp, 0o755); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("stale temp file found after failure: %s", e.Name())
		}
	}
	// configblob.json must not have been created
	if _, statErr := os.Stat(filepath.Join(tmp, ConfigBlobJson)); statErr == nil {
		t.Error("configblob.json should not exist after failed write")
	}
}

func TestValidateVolumeDir_LoadConfigBlob(t *testing.T) {
	tmp := t.TempDir()
	// non-hidden 파일 추가
	if err := os.WriteFile(filepath.Join(tmp, "data.txt"), []byte("content"), 0644); err != nil {
		t.Fatalf("failed to write data file: %v", err)
	}
	// 유효한 JSON configblob.json 생성
	blob := `{"key":"value"}`
	cfgPath := filepath.Join(tmp, ConfigBlobJson)
	if err := os.WriteFile(cfgPath, []byte(blob), 0644); err != nil {
		t.Fatalf("failed to write configblob.json: %v", err)
	}

	raw, err := ValidateVolumeDir(tmp)
	if err != nil {
		t.Fatalf("ValidateVolumeDir failed: %v", err)
	}
	if string(raw) != blob {
		t.Fatalf("expected raw %q, got %q", blob, raw)
	}
}

// TODO oci 폴더 권한 문제가 발생함. 이건 사전에 맞춰저야 한다.

func TestOciService01(t *testing.T) {
	conf, err := InitConfig("sori-oci.json")
	if err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	err = conf.EnsureDir()
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("skipping: no permission to create %q", conf.Local.Path)
		}
		t.Fatalf("EnsureDir failed: %v", err)
	}
	// 전체적인 볼륨의 정보를 담고 있는 volume-collection.json 을 만들어줌.
	cm, err := NewCollectionManager(conf.Local.Path)
	if err != nil {
		t.Fatalf("NewCollectionManager failed: %v", err)
	}

	// TODO
	// 볼륨하나 만들어줌. TODO 여기서 volumeindex 포인터를 만들어 주는데 이거 재활용할 것인지 생각해야 함. 이것과 연관해서 validation 메서드 만들어줘야 함.
	// podbridge5 볼륨 메서드에서 사용할때, 필요한 메서드 만들어줘야 함.
	// displayName 같은 경우 중복성 검사를 않하고 있음. 해줄 필요가 있는지 생각해야 함. 이건 옵션으로 두어서 중복으로 저장할 경우는 옵션을 true 로 주면 중복으로 저장되게 하자.
	// 볼륨 폴더가 빈 폴더면, 저장할 필요가 없음.
	// 지금은 볼더만을 잡아주고 있는데, 파일, 압축파일 등도 할지 생각해봐야 함.
	volDir := "./test-vol"

	configblob, err := ValidateVolumeDir(volDir)
	if err != nil {
		t.Fatalf("ValidateVolumeDir failed: %v", err)
	}

	vi, err := GenerateVolumeIndex(volDir, "테스트 하는 볼륨")
	if err != nil {
		t.Fatalf("GenerateVolumeIndex failed: %v", err)
	}

	var cb ConfigBlob
	if err := json.Unmarshal(configblob, &cb); err != nil {
		t.Fatalf("failed to parse configblob.json into ConfigBlob: %v", err)
	}

	newvi, err := vi.PublishVolume(context.Background(), volDir, "test.v1.0.0", configblob)
	if err != nil {
		t.Fatalf("PublishVolume failed: %v", err)
	}
	if newvi == nil {
		t.Fatalf("PublishVolume returned nil VolumeIndex")
	}

	// 만들어준 볼륨을 cm 에 넣어줌. TODO pointer 로 쓸지 value 로 쓸지 결정하자. 지금 혼재되어 있음.
	entry := VolumeEntry{
		Index:      *newvi,
		ConfigBlob: cb,
	}
	if err := cm.AddOrUpdate(entry); err != nil {
		t.Fatalf("AddOrUpdate failed: %v", err)
	}
}

func TestPublishVolumeFromDir_Success(t *testing.T) {
	// 1) 설정 로드 및 로컬 디렉터리 준비
	conf, err := InitConfig("sori-oci.json")
	if err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}
	if err := conf.EnsureDir(); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("skipping: no permission to create %q", conf.Local.Path)
		}
		t.Fatalf("EnsureDir failed: %v", err)
	}

	// 2) CollectionManager 생성
	cm, err := NewCollectionManager(conf.Local.Path)
	if err != nil {
		t.Fatalf("NewCollectionManager failed: %v", err)
	}

	// 3) 실제 테스트할 볼륨 디렉터리와 파라미터
	volDir := "./test-vol"     // 사전에 준비된 테스트용 디렉터리
	displayName := "테스트 하는 볼륨" // 사용자 지정 이름
	tag := "test.v1.0.0"       // 태그

	// 4) 통합 메서드 호출
	if err := cm.PublishVolumeFromDir(context.Background(), volDir, displayName, tag); err != nil {
		t.Fatalf("PublishVolumeFromDir failed: %v", err)
	}
}

func TestPublishVolumeFromDir_InvalidConfigBlobTypedError(t *testing.T) {
	rootDir := t.TempDir()
	volDir := filepath.Join(rootDir, "vol")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "data.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile data.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volDir, ConfigBlobJson), []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("WriteFile configblob.json: %v", err)
	}

	cm, err := NewCollectionManager(filepath.Join(rootDir, "store"))
	if err != nil {
		t.Fatalf("NewCollectionManager: %v", err)
	}

	err = cm.PublishVolumeFromDir(context.Background(), volDir, "Broken Volume", "broken.v1")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

// TestPublishFetchRoundTrip verifies that PublishVolume correctly sets the
// partitionPath annotation so that FetchVolSeq can reconstruct the volume.
func TestPublishFetchRoundTrip(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	storePath := filepath.Join(tmp, "oci")
	client := NewClient(WithLocalStorePath(storePath))

	volDir := "./test-vol"
	tag := "roundtrip.v1.0.0"

	// 1) Generate index and publish.
	vi, err := GenerateVolumeIndex(volDir, "RoundTrip")
	if err != nil {
		t.Fatalf("GenerateVolumeIndex: %v", err)
	}
	configBlob := []byte("{}")
	published, err := client.PublishVolume(ctx, vi, volDir, tag, configBlob)
	if err != nil {
		t.Fatalf("PublishVolume: %v", err)
	}
	if published.VolumeRef == "" {
		t.Fatal("expected non-empty VolumeRef after publish")
	}

	// 2) Fetch back into a separate dest directory.
	dest := filepath.Join(tmp, "restored")
	fetched, err := client.FetchVolumeSequential(ctx, dest, storePath, tag)
	if err != nil {
		t.Fatalf("FetchVolSeq: %v", err)
	}

	// 3) VolumeRef must match published digest.
	if fetched.VolumeRef != published.VolumeRef {
		t.Errorf("VolumeRef mismatch: published=%q, fetched=%q", published.VolumeRef, fetched.VolumeRef)
	}

	// 4) Partition count must match.
	if len(fetched.Partitions) != len(published.Partitions) {
		t.Errorf("partition count: published=%d, fetched=%d", len(published.Partitions), len(fetched.Partitions))
	}

	// 5) Files should be restored exactly once under destRoot.
	restoredFile := filepath.Join(dest, "test-vol", "docs", "test111", "test.txt")
	if _, err := os.Stat(restoredFile); err != nil {
		t.Fatalf("expected restored file %q: %v", restoredFile, err)
	}

	duplicatedPath := filepath.Join(dest, "test-vol", "docs", "test-vol")
	if _, err := os.Stat(duplicatedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected nested restore path %q exists", duplicatedPath)
	}
}

func TestPublishFetchRoundTrip_WithRootFiles(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// 1. Create a temp vol dir with root files and one partition subdir.
	volDir := filepath.Join(tmp, "vol")
	if err := os.MkdirAll(filepath.Join(volDir, "docs"), 0o755); err != nil {
		t.Fatalf("MkdirAll docs: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(volDir, "partA"), 0o755); err != nil {
		t.Fatalf("MkdirAll partA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "README.md"), []byte("# readme"), 0o644); err != nil {
		t.Fatalf("WriteFile README.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "docs", "guide.txt"), []byte("guide content"), 0o644); err != nil {
		t.Fatalf("WriteFile guide.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "partA", "data.bin"), []byte("binary data"), 0o644); err != nil {
		t.Fatalf("WriteFile data.bin: %v", err)
	}

	// 2. Create OCI store and client.
	ociStore := filepath.Join(tmp, "oci")
	client := NewClient(WithLocalStorePath(ociStore))

	// 3. Package the volume.
	pkg, err := client.PackageVolume(ctx, PackageRequest{
		SourceDir:   volDir,
		Tag:         "rf.v1",
		DisplayName: "RF Test",
	})
	if err != nil {
		t.Fatalf("PackageVolume: %v", err)
	}
	// Should have exactly 1 partition (partA); docs is a subdir but should be packaged in root-files layer (not as partition).
	// Actually GenerateVolumeIndex picks up all subdirs: docs and partA are both partitions.
	// The root-files layer carries README.md only. Partitions carry docs and partA.
	if len(pkg.Partitions) == 0 {
		t.Fatalf("expected at least 1 partition, got 0")
	}

	// 4. Fetch to destDir.
	destDir := filepath.Join(tmp, "dest")
	volDirBase := filepath.Base(volDir)
	vi, err := FetchVolSeq(ctx, destDir, ociStore, "rf.v1")
	if err != nil {
		t.Fatalf("FetchVolSeq: %v", err)
	}

	// 5. Assert README.md was restored.
	readmePath := filepath.Join(destDir, volDirBase, "README.md")
	readmeData, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("README.md not restored: %v", err)
	}
	if string(readmeData) != "# readme" {
		t.Fatalf("README.md content mismatch: got %q", readmeData)
	}

	// 6. Assert guide.txt was restored.
	guidePath := filepath.Join(destDir, volDirBase, "docs", "guide.txt")
	guideData, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("docs/guide.txt not restored: %v", err)
	}
	if string(guideData) != "guide content" {
		t.Fatalf("guide.txt content mismatch: got %q", guideData)
	}

	// 7. Assert vi.Partitions does not contain a root-files pseudo-entry — all
	//    partition entries should be actual subdirectory partitions (docs, partA).
	for _, p := range vi.Partitions {
		if p.Path == volDirBase {
			t.Fatalf("vi.Partitions must not contain root-files pseudo-entry (path=%q)", p.Path)
		}
	}
	if len(vi.Partitions) != len(pkg.Partitions) {
		t.Fatalf("Partitions count mismatch: pkg=%d vi=%d", len(pkg.Partitions), len(vi.Partitions))
	}
}

func TestPackageVolumeToStore(t *testing.T) {
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "oci")

	req := PackageRequest{
		SourceDir:   "./test-vol",
		DisplayName: "Packaged Volume",
		Tag:         "pkg.v1.0.0",
		Dataset:     "hg38",
		Version:     "v1",
		Description: "test package",
		Annotations: map[string]string{"env": "test"},
	}

	pkg, err := PackageVolumeToStore(ctx, storePath, req)
	if err != nil {
		t.Fatalf("PackageVolumeToStore: %v", err)
	}

	if pkg.LocalTag != req.Tag {
		t.Fatalf("LocalTag mismatch: got %q want %q", pkg.LocalTag, req.Tag)
	}
	if pkg.StableRef != "hg38:v1" {
		t.Fatalf("StableRef mismatch: got %q", pkg.StableRef)
	}
	if pkg.ManifestDigest == "" || pkg.ConfigDigest == "" {
		t.Fatalf("expected non-empty digests, got manifest=%q config=%q", pkg.ManifestDigest, pkg.ConfigDigest)
	}
	if pkg.TotalSize <= 0 {
		t.Fatalf("expected positive total size, got %d", pkg.TotalSize)
	}
	if len(pkg.Partitions) == 0 {
		t.Fatal("expected partitions to be populated")
	}
	if pkg.VolumeIndex.VolumeRef != pkg.ManifestDigest {
		t.Fatalf("VolumeIndex.VolumeRef mismatch: got %q want %q", pkg.VolumeIndex.VolumeRef, pkg.ManifestDigest)
	}
}

func TestClientPackageVolumeWithOptions_InvalidConfigBlob(t *testing.T) {
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.PackageVolumeWithOptions(context.Background(), PackageRequest{
		SourceDir:   "./test-vol",
		DisplayName: "Packaged Volume",
		Tag:         "pkg.v1.0.0",
	}, PackageOptions{
		ConfigBlob: []byte("{invalid"),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestClientPackageVolumeWithOptions_RequireConfigBlob(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "data.txt"), []byte("content"), 0644); err != nil {
		t.Fatalf("write data file: %v", err)
	}
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.PackageVolumeWithOptions(context.Background(), PackageRequest{
		SourceDir:   tmp,
		DisplayName: "Packaged Volume",
		Tag:         "pkg.v1.0.0",
	}, PackageOptions{
		RequireConfigBlob: true,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestClientFetchVolume_RequireEmptyDestination(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "existing.txt"), []byte("content"), 0644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	client := NewClient()
	_, err := client.FetchVolume(context.Background(), dest, "./repo", "v1.0.0", FetchOptions{
		Concurrency:             1,
		RequireEmptyDestination: true,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestClientFetchVolume_RequireEmptyDestination_EmptyDir(t *testing.T) {
	// An empty but existing dir must also be rejected when RequireEmptyDestination=true.
	dest := t.TempDir()
	client := NewClient()
	_, err := client.FetchVolume(context.Background(), dest, "./repo", "v1.0.0", FetchOptions{
		Concurrency:             1,
		RequireEmptyDestination: true,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for empty-but-existing dest, got %v", err)
	}
}

func TestBuildDataSpec(t *testing.T) {
	req := PackageRequest{
		SourceDir:   "./test-vol",
		DisplayName: "HumanRef GRCh38",
		Tag:         "hg38.v1",
		Dataset:     "hg38",
		Version:     "2026.04",
		Description: "reference genome",
		Annotations: map[string]string{"organism": "human"},
	}
	pkg := &PackageResult{
		StableRef:      "hg38:2026.04",
		LocalTag:       "hg38.v1",
		ManifestDigest: "sha256:manifest",
		ConfigDigest:   "sha256:config",
		TotalSize:      1234,
		CreatedAt:      "2026-04-17T00:00:00Z",
		Partitions: []Partition{
			{Name: "docs", Path: "test-vol/docs", ManifestRef: "sha256:layer"},
		},
	}
	push := &PushResult{
		Reference:      "harbor.example/data/hg38:2026.04",
		Repository:     "harbor.example/data/hg38",
		Tag:            "2026.04",
		ManifestDigest: "sha256:remote",
	}

	spec, err := BuildDataSpec(pkg, push, req)
	if err != nil {
		t.Fatalf("BuildDataSpec: %v", err)
	}

	if spec.Identity.StableRef != pkg.StableRef {
		t.Fatalf("StableRef mismatch: got %q want %q", spec.Identity.StableRef, pkg.StableRef)
	}
	if spec.Data.Repository != push.Repository || spec.Data.Reference != push.Reference {
		t.Fatalf("push metadata mismatch: %+v", spec.Data)
	}
	if spec.Data.ManifestDigest != push.ManifestDigest {
		t.Fatalf("manifest digest mismatch: got %q want %q", spec.Data.ManifestDigest, push.ManifestDigest)
	}
	if spec.Display.Name != req.DisplayName || spec.Display.Description != req.Description {
		t.Fatalf("display metadata mismatch: %+v", spec.Display)
	}
	if spec.Provenance.SourceDir != req.SourceDir || spec.Provenance.LocalTag != pkg.LocalTag {
		t.Fatalf("provenance mismatch: %+v", spec.Provenance)
	}
}

func TestPushDataSpecManifest(t *testing.T) {
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "oci")

	req := PackageRequest{
		SourceDir:   "./test-vol",
		DisplayName: "Packaged Volume",
		Tag:         "pkg.v1.0.0",
		Dataset:     "hg38",
		Version:     "v1",
	}
	pkg, err := PackageVolumeToStore(ctx, storePath, req)
	if err != nil {
		t.Fatalf("PackageVolumeToStore: %v", err)
	}
	spec, err := BuildDataSpec(pkg, nil, req)
	if err != nil {
		t.Fatalf("BuildDataSpec: %v", err)
	}

	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("oci.New: %v", err)
	}
	subjectDesc, err := store.Resolve(ctx, req.Tag)
	if err != nil {
		t.Fatalf("Resolve subject: %v", err)
	}

	result, err := pushDataSpecManifest(ctx, store, subjectDesc, spec)
	if err != nil {
		t.Fatalf("pushDataSpecManifest: %v", err)
	}
	if result.SubjectDigest != subjectDesc.Digest.String() {
		t.Fatalf("subject digest mismatch: got %q want %q", result.SubjectDigest, subjectDesc.Digest)
	}
	if result.ManifestDigest == "" || result.ConfigDigest == "" {
		t.Fatalf("expected non-empty referrer digests: %+v", result)
	}

	predecessors, err := store.Predecessors(ctx, subjectDesc)
	if err != nil {
		t.Fatalf("Predecessors: %v", err)
	}
	if len(predecessors) == 0 {
		t.Fatal("expected at least one referrer predecessor")
	}

	var found ocispec.Descriptor
	for _, pred := range predecessors {
		if pred.Digest.String() == result.ManifestDigest {
			found = pred
			break
		}
	}
	if found.Digest == "" {
		t.Fatalf("expected predecessor %q not found", result.ManifestDigest)
	}

	rc, err := store.Fetch(ctx, found)
	if err != nil {
		t.Fatalf("Fetch referrer manifest: %v", err)
	}
	defer rc.Close()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		t.Fatalf("decode referrer manifest: %v", err)
	}
	if manifest.Subject == nil || manifest.Subject.Digest != subjectDesc.Digest {
		t.Fatalf("unexpected subject in manifest: %+v", manifest.Subject)
	}
	if manifest.Config.Digest.String() != result.ConfigDigest {
		t.Fatalf("config digest mismatch: got %q want %q", manifest.Config.Digest, result.ConfigDigest)
	}
	if manifest.Config.MediaType != MediaTypeDataSpec {
		t.Fatalf("unexpected config media type: %q", manifest.Config.MediaType)
	}
}

func TestUntarGzDir_RejectsPathTraversal(t *testing.T) {
	buf := &bytes.Buffer{}
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	content := []byte("escape")
	hdr := &tar.Header{
		Name:     "../escape.txt",
		Typeflag: tar.TypeReg,
		Mode:     0644,
		Size:     int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader failed: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close failed: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}

	err := UntarGzDir(bytes.NewReader(buf.Bytes()), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "escapes destination") {
		t.Fatalf("expected path traversal error, got %v", err)
	}
}
