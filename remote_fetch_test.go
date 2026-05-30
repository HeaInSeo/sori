package sori

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"
)

// newMockOCIServer starts an httptest server that serves the content of a local
// OCI layout store over the OCI Distribution Spec v2 HTTP API.  repoName must
// not contain slashes.
func newMockOCIServer(t *testing.T, storePath, repoName, tag string) *httptest.Server {
	t.Helper()
	ctx := context.Background()

	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("oci.New: %v", err)
	}

	type entry struct {
		data      []byte
		mediaType string
	}
	blobs := make(map[string]*entry)

	manifestDesc, err := store.Resolve(ctx, tag)
	if err != nil {
		t.Fatalf("resolve tag %q: %v", tag, err)
	}
	rc, err := store.Fetch(ctx, manifestDesc)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	manifestBytes, readErr := io.ReadAll(rc)
	if closeErr := rc.Close(); closeErr != nil && readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		t.Fatalf("read manifest: %v", readErr)
	}
	blobs[manifestDesc.Digest.String()] = &entry{data: manifestBytes, mediaType: manifestDesc.MediaType}
	tagDigest := manifestDesc.Digest

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}

	for _, desc := range append(manifest.Layers, manifest.Config) {
		if _, exists := blobs[desc.Digest.String()]; exists {
			continue
		}
		brc, err := store.Fetch(ctx, desc)
		if err != nil {
			t.Fatalf("fetch blob %s: %v", desc.Digest, err)
		}
		data, readErr := io.ReadAll(brc)
		if closeErr := brc.Close(); closeErr != nil && readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			t.Fatalf("read blob %s: %v", desc.Digest, readErr)
		}
		blobs[desc.Digest.String()] = &entry{data: data, mediaType: desc.MediaType}
	}

	manifestPfx := "/v2/" + repoName + "/manifests/"
	blobPfx := "/v2/" + repoName + "/blobs/"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/v2/" || p == "/v2":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(p, manifestPfx):
			ref := p[len(manifestPfx):]
			dgst := ref
			if ref == tag {
				dgst = tagDigest.String()
			}
			e, ok := blobs[dgst]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", e.mediaType)
			w.Header().Set("Docker-Content-Digest", dgst)
			_, _ = w.Write(e.data)
		case strings.HasPrefix(p, blobPfx):
			dgst := p[len(blobPfx):]
			e, ok := blobs[dgst]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", dgst)
			_, _ = w.Write(e.data)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestFetchVolumeFromRemote_Success packages a volume, serves it via a mock OCI
// registry, fetches it with FetchVolumeFromRemote, and verifies the extracted
// layout.
func TestFetchVolumeFromRemote_Success(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	validDesc, validData := buildValidTarGzLayer(t, "vol/part")
	storePath := buildOCIStore(t, []struct {
		desc ocispec.Descriptor
		data []byte
	}{{validDesc, validData}}, "remote.v1")

	ts := newMockOCIServer(t, storePath, "testrepo", "remote.v1")
	host := ts.Listener.Addr().String()

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(t.TempDir()))
	vi, err := client.FetchVolumeFromRemote(ctx, destRoot, RemoteTarget{
		Registry:   host,
		Repository: "testrepo",
		PlainHTTP:  true,
	}, "remote.v1", FetchOptions{})
	if err != nil {
		t.Fatalf("FetchVolumeFromRemote: %v", err)
	}
	if vi == nil {
		t.Fatal("expected non-nil VolumeIndex")
	}
	if _, statErr := os.Stat(destRoot); statErr != nil {
		t.Errorf("destRoot must exist after successful remote fetch: %v", statErr)
	}
	partDir := filepath.Join(destRoot, "vol", "part")
	if info, statErr := os.Stat(partDir); statErr != nil || !info.IsDir() {
		t.Errorf("partition directory %q must exist under destRoot", partDir)
	}
	// No staging siblings must survive.
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Errorf("stale staging dir after successful remote fetch: %s", e.Name())
		}
	}
}

// TestFetchVolumeFromRemote_TagNotFound verifies that a 404 from the registry
// produces ErrNotFound.
func TestFetchVolumeFromRemote_TagNotFound(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`))
	}))
	t.Cleanup(ts.Close)

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.FetchVolumeFromRemote(ctx, destRoot, RemoteTarget{
		Registry:   ts.Listener.Addr().String(),
		Repository: "testrepo",
		PlainHTTP:  true,
	}, "notfound.v1", FetchOptions{})
	if err == nil {
		t.Fatal("expected error for tag not found")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %T: %v", err, err)
	}
}

// TestFetchVolumeFromRemote_CorruptManifest verifies that a manifest body that
// is not valid JSON produces ErrIntegrity.  The digest is correct so oras-go
// content verification passes; our JSON decode then fails.
func TestFetchVolumeFromRemote_CorruptManifest(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	corrupt := []byte("not valid json at all")
	corruptDigest := digest.FromBytes(corrupt)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/v2/" || p == "/v2" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasPrefix(p, "/v2/testrepo/manifests/") {
			ref := p[len("/v2/testrepo/manifests/"):]
			if ref != "corrupt.v1" && ref != corruptDigest.String() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", corruptDigest.String())
			_, _ = w.Write(corrupt)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.FetchVolumeFromRemote(ctx, destRoot, RemoteTarget{
		Registry:   ts.Listener.Addr().String(),
		Repository: "testrepo",
		PlainHTTP:  true,
	}, "corrupt.v1", FetchOptions{})
	if err == nil {
		t.Fatal("expected error for corrupt manifest")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %T: %v", err, err)
	}
}

// TestFetchVolumeFromRemote_Unauthorized verifies that a 401 response from the
// registry produces ErrAuth, not ErrNotFound or ErrTransport.
func TestFetchVolumeFromRemote_Unauthorized(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", `Basic realm="Registry Realm"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
	}))
	t.Cleanup(ts.Close)

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.FetchVolumeFromRemote(ctx, destRoot, RemoteTarget{
		Registry:   ts.Listener.Addr().String(),
		Repository: "testrepo",
		PlainHTTP:  true,
	}, "v1", FetchOptions{})
	if err == nil {
		t.Fatal("expected error for 401 Unauthorized")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %T: %v", err, err)
	}
}

// TestFetchVolumeFromRemote_Forbidden verifies that a 403 response from the
// registry produces ErrAuth.
func TestFetchVolumeFromRemote_Forbidden(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"access denied"}]}`))
	}))
	t.Cleanup(ts.Close)

	destRoot := filepath.Join(tmp, "dest")
	client := NewClient(WithLocalStorePath(t.TempDir()))
	_, err := client.FetchVolumeFromRemote(ctx, destRoot, RemoteTarget{
		Registry:   ts.Listener.Addr().String(),
		Repository: "testrepo",
		PlainHTTP:  true,
	}, "v1", FetchOptions{})
	if err == nil {
		t.Fatal("expected error for 403 Forbidden")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %T: %v", err, err)
	}
}

// TestFetchVolumeFromRemote_ValidationErrors checks input validation.
func TestFetchVolumeFromRemote_ValidationErrors(t *testing.T) {
	ctx := context.Background()
	client := NewClient(WithLocalStorePath(t.TempDir()))

	cases := []struct {
		name   string
		target RemoteTarget
		tag    string
		opts   FetchOptions
	}{
		{
			name:   "empty tag",
			target: RemoteTarget{Registry: "reg.example.com", Repository: "repo"},
			tag:    "",
		},
		{
			name:   "empty registry",
			target: RemoteTarget{Registry: "", Repository: "repo"},
			tag:    "v1",
		},
		{
			name:   "empty repository",
			target: RemoteTarget{Registry: "reg.example.com", Repository: ""},
			tag:    "v1",
		},
		{
			name:   "mutually exclusive options",
			target: RemoteTarget{Registry: "reg.example.com", Repository: "repo"},
			tag:    "v1",
			opts:   FetchOptions{RequireEmptyDestination: true, AtomicOverwrite: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.FetchVolumeFromRemote(ctx, t.TempDir(), tc.target, tc.tag, tc.opts)
			if err == nil {
				t.Fatal("expected ErrValidation, got nil")
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %T: %v", err, err)
			}
		})
	}
}
