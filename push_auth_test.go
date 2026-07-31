package sori

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// maliciousUploadRedirectRegistry simulates the exact vulnerability fixed by
// oras-go v2.6.1 / GO-2026-5882 ("Blob upload vulnerable to credential
// forwarding via unvalidated Location header"): a registry (or an attacker
// who has compromised/impersonated one) answers the blob-upload-initiate
// POST with a 202 Accepted whose Location header points to a *different*
// host - attackerAddr - instead of staying on the registry's own host.
// oras-go's blobStore.completePushAfterInitialPost is supposed to reject
// this outright (sameUploadHost check) before ever making a request to that
// other host, so credentials are never sent there.
type maliciousUploadRedirectRegistry struct {
	attackerAddr string
}

func (r *maliciousUploadRedirectRegistry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/v2/" || req.URL.Path == "/v2":
			w.WriteHeader(http.StatusOK)
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/blobs/uploads/"):
			// Malicious: redirect the upload to an attacker-controlled host.
			w.Header().Set("Location", "http://"+r.attackerAddr+"/evil-upload/"+req.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		case req.Method == http.MethodGet || req.Method == http.MethodHead:
			// Blob/manifest existence checks: always "not found" so oras-go
			// proceeds to push rather than short-circuiting as already-present.
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// attackerServer records every request it receives, so the test can assert
// it received none - proving oras-go never even attempted the cross-host
// PUT, let alone forwarded credentials to it.
type attackerServer struct {
	mu       sync.Mutex
	requests []string
}

func (a *attackerServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		a.mu.Lock()
		a.requests = append(a.requests, req.Method+" "+req.URL.String())
		a.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (a *attackerServer) requestCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

// TestPushLocalToRemote_RejectsCrossHostUploadRedirect reproduces
// GO-2026-5882 directly: a registry redirects the blob-upload Location to a
// different host, and the push must fail *without ever contacting that
// host* - the credential-forwarding-via-redirect vulnerability class this
// package's oras-go version pin was specifically bumped to fix. Unlike
// TestPushLocalToRemote_Unauthorized above (which only proves auth headers
// are forwarded on a same-origin retry - a check the vulnerable version
// would also pass), this test would fail against oras-go v2.6.0 and pass
// only against the fixed v2.6.1+.
func TestPushLocalToRemote_RejectsCrossHostUploadRedirect(t *testing.T) {
	ctx := context.Background()

	attacker := &attackerServer{}
	attackerTS := httptest.NewServer(attacker.handler())
	t.Cleanup(attackerTS.Close)

	registry := &maliciousUploadRedirectRegistry{attackerAddr: attackerTS.Listener.Addr().String()}
	registryTS := httptest.NewServer(registry.handler())
	t.Cleanup(registryTS.Close)

	localRepo := newLocalPushFixture(t, "push-redirect-test.v1")
	remoteRepo := registryTS.Listener.Addr().String() + "/testrepo"

	_, err := PushLocalToRemote(ctx, localRepo, "push-redirect-test.v1", remoteRepo, "", "", true)
	if err == nil {
		t.Fatal("expected push to fail when the registry redirects the upload Location cross-host")
	}
	if attacker.requestCount() != 0 {
		t.Fatalf("attacker server received %d request(s) (%v) - credentials/content were forwarded to an untrusted host", attacker.requestCount(), attacker.requests)
	}
}
