package sori

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

// newLocalPushFixture builds a minimal local OCI store with one tagged
// manifest, suitable as the source for a PushLocalToRemote call.
func newLocalPushFixture(t *testing.T, tag string) string {
	t.Helper()
	ctx := context.Background()
	storePath := t.TempDir()

	store, err := oci.New(storePath)
	if err != nil {
		t.Fatalf("oci.New: %v", err)
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
		oras.PackManifestOptions{ConfigDescriptor: &configDesc},
	)
	if err != nil {
		t.Fatalf("PackManifest: %v", err)
	}
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		t.Fatalf("Tag: %v", err)
	}
	return storePath
}

// authRejectingRegistry answers /v2/ pings with 200 (so oras-go proceeds past
// the initial capability probe) and every other request with 401 + a
// WWW-Authenticate challenge, recording whether any request ever carried an
// Authorization header - proving credentials were actually forwarded on the
// push path, not just that push eventually failed for some unrelated reason.
type authRejectingRegistry struct {
	mu            sync.Mutex
	sawAuthHeader bool
}

func (r *authRejectingRegistry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/" || req.URL.Path == "/v2" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if req.Header.Get("Authorization") != "" {
			r.mu.Lock()
			r.sawAuthHeader = true
			r.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", `Basic realm="Registry Realm"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
	}
}

func (r *authRejectingRegistry) authHeaderWasSent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sawAuthHeader
}

// TestPushLocalToRemote_Unauthorized covers the push credential path, which
// - unlike the fetch path (TestFetchVolumeFromRemote_Unauthorized/_Forbidden
// above) - previously had no CI-run auth-failure coverage at all. This is
// the same push path (pushLocalTagToRepository -> registryutil.NewRepository)
// implicated in the historical credential-forwarding CVE fix; a regression
// of that exact class would not have been caught by any automated test
// before this.
func TestPushLocalToRemote_Unauthorized(t *testing.T) {
	ctx := context.Background()
	registry := &authRejectingRegistry{}
	ts := httptest.NewServer(registry.handler())
	t.Cleanup(ts.Close)

	localRepo := newLocalPushFixture(t, "push-auth-test.v1")

	remoteRepo := ts.Listener.Addr().String() + "/testrepo"
	_, err := PushLocalToRemote(ctx, localRepo, "push-auth-test.v1", remoteRepo, "testuser", "testpass", true)
	if err == nil {
		t.Fatal("expected error for 401 Unauthorized on push")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth, got %T: %v", err, err)
	}
	if !registry.authHeaderWasSent() {
		t.Fatal("push never sent an Authorization header - credentials were not forwarded to the push path")
	}
}
