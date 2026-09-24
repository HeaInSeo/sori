package acquisition

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const fakeRepoName = "data/ref"

// fakeRegistry is a hermetic in-process OCI distribution endpoint serving exactly one
// repository by digest. Behavior toggles model a lying/corrupting registry, an
// unavailable endpoint and basic-auth credentials.
type fakeRegistry struct {
	mu        sync.Mutex
	manifests map[digest.Digest][]byte
	blobs     map[digest.Digest][]byte
	user      string
	pass      string

	tamperManifest bool // serve different bytes under the pinned digest header
	tamperBlob     bool // flip a byte of every member blob (same length)
	failBlobs      bool // 500 on blob GET (models an interrupted transfer)
	contentHits    int  // manifest+blob GETs served with content

	srv *httptest.Server
}

// subjectFixture is one OCI subject: a manifest plus its config and member blobs.
type subjectFixture struct {
	manifest []byte
	digest   digest.Digest
	blobs    map[digest.Digest][]byte
}

func newSubjectFixture(t *testing.T, members map[string]string) subjectFixture {
	t.Helper()
	blobs := map[digest.Digest][]byte{ocispec.DescriptorEmptyJSON.Digest: ocispec.DescriptorEmptyJSON.Data}
	layers := make([]ocispec.Descriptor, 0, len(members))
	// Deterministic layer order.
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, title := range keys {
		data := []byte(members[title])
		d := digest.FromBytes(data)
		blobs[d] = data
		layers = append(layers, ocispec.Descriptor{
			MediaType:   "application/vnd.sori.test.member",
			Digest:      d,
			Size:        int64(len(data)),
			Annotations: map[string]string{ocispec.AnnotationTitle: title},
		})
	}
	cfg := ocispec.DescriptorEmptyJSON
	cfg.Data = nil
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.sori.test.dataset",
		Config:       cfg,
		Layers:       layers,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return subjectFixture{manifest: raw, digest: digest.FromBytes(raw), blobs: blobs}
}

func newFakeRegistry(t *testing.T, subjects ...subjectFixture) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{manifests: map[digest.Digest][]byte{}, blobs: map[digest.Digest][]byte{}}
	for _, s := range subjects {
		f.manifests[s.digest] = s.manifest
		for d, b := range s.blobs {
			f.blobs[d] = b
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// endpoint returns the repository reference for this registry.
func (f *fakeRegistry) endpoint(cred CredentialRef) Endpoint {
	return Endpoint{Repository: strings.TrimPrefix(f.srv.URL, "http://") + "/" + fakeRepoName, PlainHTTP: true, Credential: cred}
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeRegistry) set(fn func(*fakeRegistry)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeRegistry) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contentHits
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.user != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if r.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	prefix := "/v2/" + fakeRepoName + "/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	kind, ref, _ := strings.Cut(rest, "/")
	d, err := digest.Parse(ref)
	if err != nil {
		// Only digest references are served: a tag is never resolvable here.
		http.NotFound(w, r)
		return
	}
	switch kind {
	case "manifests":
		f.serveManifest(w, r, d)
	case "blobs":
		f.serveBlob(w, r, d)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request, d digest.Digest) {
	body, ok := f.manifests[d]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.tamperManifest {
		body = append(append([]byte(nil), body...), ' ')
	}
	w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
	// A lying registry: the header claims the pinned digest whatever the bytes are.
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	f.contentHits++
	_, _ = w.Write(body)
}

func (f *fakeRegistry) serveBlob(w http.ResponseWriter, r *http.Request, d digest.Digest) {
	if f.failBlobs {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body, ok := f.blobs[d]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.tamperBlob && d != ocispec.DescriptorEmptyJSON.Digest {
		body = append([]byte(nil), body...)
		body[len(body)-1] ^= 0xff
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	f.contentHits++
	_, _ = w.Write(body)
}
