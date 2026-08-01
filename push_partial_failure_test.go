package sori

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HeaInSeo/sori/chunked"
)

// blobFailAfterNRegistry simulates a network drop partway through a
// multi-blob push: the first n blob-upload cycles (POST-initiate then
// PUT-to-Location, both on this same host) succeed normally; the (n+1)th
// upload-initiate POST fails outright (as a dropped connection would). It
// also records whether any request ever reached the manifest path, to prove
// the push aborts before committing a manifest that references
// not-fully-uploaded blobs.
type blobFailAfterNRegistry struct {
	mu               sync.Mutex
	n                int
	uploadInitiates  int
	sawManifestWrite bool
}

func (r *blobFailAfterNRegistry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/v2/" || req.URL.Path == "/v2":
			w.WriteHeader(http.StatusOK)
			return
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/blobs/uploads/"):
			r.mu.Lock()
			r.uploadInitiates++
			attempt := r.uploadInitiates
			r.mu.Unlock()
			if attempt > r.n {
				// Simulate a dropped connection: close without a response
				// rather than returning a well-formed error, matching a
				// real network failure more closely than a clean 5xx would.
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
						return
					}
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Location", "http://"+req.Host+req.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/blobs/uploads/"):
			w.WriteHeader(http.StatusCreated)
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/manifests/"):
			r.mu.Lock()
			r.sawManifestWrite = true
			r.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case req.Method == http.MethodGet || req.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (r *blobFailAfterNRegistry) manifestWasWritten() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sawManifestWrite
}

func (r *blobFailAfterNRegistry) uploadAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uploadInitiates
}

// TestPushLocalToRemote_PartialFailureDoesNotCommitManifest publishes a
// chunked artifact (config blob + chunk-index blob + at least one chunk
// blob, so at least 3 separate blob uploads precede the manifest write),
// then pushes it to a registry that fails partway through the blob
// sequence - simulating a network drop mid-push. Nothing in this package
// previously tested partial-push failure at all: no mock server ever failed
// mid-push, and there was no assertion about resulting state after an
// interrupted push.
func TestPushLocalToRemote_PartialFailureDoesNotCommitManifest(t *testing.T) {
	ctx := context.Background()
	storePath := t.TempDir()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "data.bin"), bytes.Repeat([]byte{0xAB}, 4096), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	if _, err := chunked.Publish(ctx, storePath, srcDir, "partial-fail.v1", chunked.PublishOptions{
		ChunkSize: chunked.MinChunkSize,
	}); err != nil {
		t.Fatalf("chunked.Publish: %v", err)
	}

	// Let the first blob upload succeed, fail on the second - proving the
	// push aborts partway through rather than silently completing or
	// retrying past a real failure.
	registry := &blobFailAfterNRegistry{n: 1}
	ts := httptest.NewServer(registry.handler())
	t.Cleanup(ts.Close)

	remoteRepo := ts.Listener.Addr().String() + "/testrepo"
	_, err := PushLocalToRemote(ctx, storePath, "partial-fail.v1", remoteRepo, "", "", true)
	if err == nil {
		t.Fatal("expected error from a push interrupted mid-sequence, got nil")
	}
	// Prove the failure actually happened on the second upload attempt
	// (the one this test is designed to exercise), not on the first -
	// otherwise this would pass just as well against a push that fails
	// immediately, without ever exercising the partial-upload branch.
	if attempts := registry.uploadAttempts(); attempts <= registry.n {
		t.Fatalf("only %d upload attempt(s) were made, want more than %d - the second blob upload was never reached", attempts, registry.n)
	}
	if registry.manifestWasWritten() {
		t.Fatal("manifest was written despite an interrupted blob upload - push is not all-or-nothing")
	}
}
