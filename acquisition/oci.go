package acquisition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/HeaInSeo/sori/authority"
	"github.com/HeaInSeo/sori/registryutil"
)

// maxManifestBytes bounds the pinned manifest read.
const maxManifestBytes = 4 << 20

const stagingDirPerm = 0o750

// transfer pulls the pinned subject from ep into a fresh temporary OCI layout,
// verifies it against the pinned digests, and atomically renames it into the
// operation's staged path. Nothing is published on any failure.
func (a *Acquirer) transfer(ctx context.Context, op Operation, ep Endpoint) (string, []authority.Member, error) {
	repo, err := a.repository(ep)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(a.StagingRoot, stagingDirPerm); err != nil {
		return "", nil, fmt.Errorf("acquisition: create staging root: %w", err)
	}
	key := stagingKey(op.ID)
	tmp, err := os.MkdirTemp(a.StagingRoot, key+".staging-")
	if err != nil {
		return "", nil, fmt.Errorf("acquisition: create staging dir: %w", err)
	}
	published := false
	defer func() {
		if !published {
			removeStaged(tmp)
		}
	}()

	manifestBytes, manifest, err := fetchPinnedManifest(ctx, repo, op.Subject)
	if err != nil {
		return "", nil, err
	}
	if _, err := matchMembers(manifest, op.Members); err != nil {
		return "", nil, err
	}
	if err := stageContent(ctx, repo, tmp, op.Subject, manifestBytes, manifest); err != nil {
		return "", nil, err
	}
	// Independent re-verification of the staged bytes (does not trust the transfer).
	members, err := verifyStaged(tmp, op.Subject, op.Members)
	if err != nil {
		return "", nil, err
	}
	final := filepath.Join(a.StagingRoot, key)
	// A previous attempt may have published final and crashed before its
	// checkpoint; that copy is not recorded, so it is replaced.
	removeStaged(final)
	if err := os.Rename(tmp, final); err != nil {
		return "", nil, fmt.Errorf("acquisition: publish staged copy: %w", err)
	}
	published = true
	return final, members, nil
}

// repository builds the ORAS remote for ep through registryutil. The credential is
// resolved lazily from the reference and only offered to the endpoint's own registry.
func (a *Acquirer) repository(ep Endpoint) (*remote.Repository, error) {
	ref, err := registry.ParseReference(ep.Repository)
	if err != nil {
		return nil, fmt.Errorf("%w: endpoint repository: %v", ErrInvalidRequest, err)
	}
	cfg := registryutil.RemoteConfig{
		PlainHTTP:  ep.PlainHTTP,
		HTTPClient: a.HTTPClient,
	}
	if ep.Credential != "" {
		if a.Credentials == nil {
			return nil, fmt.Errorf("%w: no resolver for %q", ErrCredentialUnavailable, ep.Credential)
		}
		resolve, credRef, host := a.Credentials, ep.Credential, ref.Registry
		cfg.AuthProvider = func(ctx context.Context, hostport string) (auth.Credential, error) {
			if hostport != host {
				return auth.EmptyCredential, nil
			}
			cred, err := resolve(ctx, credRef)
			if err != nil {
				return auth.EmptyCredential, fmt.Errorf("%w: %q: %v", ErrCredentialUnavailable, credRef, err)
			}
			return cred, nil
		}
	}
	return registryutil.NewRepository(ep.Repository, cfg)
}

// fetchPinnedManifest fetches the manifest BY the pinned digest and verifies the
// bytes hash to it. The registry's descriptor/headers are not trusted.
func fetchPinnedManifest(ctx context.Context, repo *remote.Repository, subject digest.Digest) ([]byte, ocispec.Manifest, error) {
	_, rc, err := repo.Manifests().FetchReference(ctx, subject.String())
	if err != nil {
		return nil, ocispec.Manifest{}, fmt.Errorf("acquisition: fetch pinned manifest %s: %w", subject, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil {
		return nil, ocispec.Manifest{}, fmt.Errorf("acquisition: read pinned manifest %s: %w", subject, err)
	}
	if len(body) > maxManifestBytes {
		return nil, ocispec.Manifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrSubjectInvalid, maxManifestBytes)
	}
	return parsePinnedManifest(body, subject)
}

func parsePinnedManifest(body []byte, subject digest.Digest) ([]byte, ocispec.Manifest, error) {
	if got := subject.Algorithm().FromBytes(body); got != subject {
		return nil, ocispec.Manifest{}, fmt.Errorf("%w: manifest is %s, pinned %s", ErrDigestMismatch, got, subject)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, ocispec.Manifest{}, fmt.Errorf("%w: decode manifest: %v", ErrSubjectInvalid, err)
	}
	if manifest.MediaType != ocispec.MediaTypeImageManifest {
		return nil, ocispec.Manifest{}, fmt.Errorf("%w: unsupported manifest media type %q", ErrSubjectInvalid, manifest.MediaType)
	}
	if err := manifest.Config.Digest.Validate(); err != nil || manifest.Config.Size < 0 {
		return nil, ocispec.Manifest{}, fmt.Errorf("%w: invalid config descriptor", ErrSubjectInvalid)
	}
	return body, manifest, nil
}

// matchMembers requires the manifest layers to close exactly over the declared
// members (by title annotation) and returns the members with content proofs taken
// from the layer digests, in declaration order.
func matchMembers(manifest ocispec.Manifest, decls []MemberDecl) ([]authority.Member, error) {
	byTitle := make(map[string]ocispec.Descriptor, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		title := layer.Annotations[ocispec.AnnotationTitle]
		if title == "" {
			return nil, fmt.Errorf("%w: layer %s has no title annotation", ErrSubjectInvalid, layer.Digest)
		}
		if _, dup := byTitle[title]; dup {
			return nil, fmt.Errorf("%w: duplicate layer title %q", ErrSubjectInvalid, title)
		}
		if err := layer.Digest.Validate(); err != nil || layer.Size < 0 {
			return nil, fmt.Errorf("%w: layer %q has an invalid descriptor", ErrSubjectInvalid, title)
		}
		byTitle[title] = layer
	}
	if len(byTitle) != len(decls) {
		return nil, fmt.Errorf("%w: manifest has %d members, %d declared", ErrSubjectInvalid, len(byTitle), len(decls))
	}
	members := make([]authority.Member, 0, len(decls))
	for _, d := range decls {
		layer, ok := byTitle[d.SemanticKey]
		if !ok {
			return nil, fmt.Errorf("%w: declared member %q not in manifest", ErrSubjectInvalid, d.SemanticKey)
		}
		members = append(members, authority.Member{
			SemanticKey: d.SemanticKey,
			Role:        d.Role,
			Proof:       authority.ContentProof{Algorithm: layer.Digest.Algorithm().String(), Digest: layer.Digest.String()},
			DataFormat:  d.DataFormat,
			Cardinality: d.Cardinality,
		})
	}
	return members, nil
}

// stageContent writes the member blobs and the pinned manifest into an OCI layout
// at dir via the ORAS OCI store, which verifies each blob against its descriptor.
func stageContent(ctx context.Context, repo *remote.Repository, dir string, subject digest.Digest, manifestBytes []byte, manifest ocispec.Manifest) error {
	store, err := oci.New(dir)
	if err != nil {
		return fmt.Errorf("acquisition: open staging layout: %w", err)
	}
	for _, blob := range subjectBlobs(manifest) {
		if err := stageBlob(ctx, repo, store, blob); err != nil {
			return err
		}
	}
	desc := ocispec.Descriptor{MediaType: manifest.MediaType, Digest: subject, Size: int64(len(manifestBytes))}
	if err := store.Push(ctx, desc, bytes.NewReader(manifestBytes)); err != nil {
		return classifyVerifyError(fmt.Sprintf("manifest %s", subject), err)
	}
	return nil
}

func stageBlob(ctx context.Context, repo *remote.Repository, store *oci.Store, layer ocispec.Descriptor) error {
	rc, err := repo.Blobs().Fetch(ctx, layer)
	if err != nil {
		return fmt.Errorf("acquisition: fetch member blob %s: %w", layer.Digest, err)
	}
	defer rc.Close()
	if err := store.Push(ctx, layer, rc); err != nil {
		return classifyVerifyError(fmt.Sprintf("member blob %s", layer.Digest), err)
	}
	return nil
}

func classifyVerifyError(what string, err error) error {
	if errors.Is(err, content.ErrMismatchedDigest) || errors.Is(err, content.ErrTrailingData) {
		return fmt.Errorf("%w: %s: %v", ErrDigestMismatch, what, err)
	}
	return fmt.Errorf("acquisition: stage %s: %w", what, err)
}

// verifyStaged re-reads the staged OCI layout at dir and proves that the manifest
// hashes to the pinned subject and every declared member blob hashes to (and has the
// size of) its manifest descriptor. It returns the members with content proofs.
func verifyStaged(dir string, subject digest.Digest, decls []MemberDecl) ([]authority.Member, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: no staged path recorded", ErrDigestMismatch)
	}
	// #nosec G304 -- path is built from the operation's own staging dir and a validated digest.
	body, err := os.ReadFile(blobPath(dir, subject))
	if err != nil {
		return nil, fmt.Errorf("%w: read staged manifest: %v", ErrDigestMismatch, err)
	}
	_, manifest, err := parsePinnedManifest(body, subject)
	if err != nil {
		return nil, err
	}
	members, err := matchMembers(manifest, decls)
	if err != nil {
		return nil, err
	}
	for _, blob := range subjectBlobs(manifest) {
		if err := verifyBlobFile(blobPath(dir, blob.Digest), blob); err != nil {
			return nil, err
		}
	}
	return members, nil
}

// subjectBlobs lists every blob the pinned manifest references (config + layers), so
// the staged copy is the complete frozen subject. Blobs are content-addressed, so a
// digest referenced more than once is listed once.
func subjectBlobs(manifest ocispec.Manifest) []ocispec.Descriptor {
	all := append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...)
	seen := make(map[digest.Digest]struct{}, len(all))
	out := make([]ocispec.Descriptor, 0, len(all))
	for _, d := range all {
		if _, dup := seen[d.Digest]; dup {
			continue
		}
		seen[d.Digest] = struct{}{}
		out = append(out, d)
	}
	return out
}

func verifyBlobFile(path string, desc ocispec.Descriptor) error {
	// #nosec G304 -- path is built from the operation's own staging dir and a validated digest.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open staged blob %s: %v", ErrDigestMismatch, desc.Digest, err)
	}
	defer f.Close()
	verifier := desc.Digest.Verifier()
	n, err := io.Copy(verifier, f)
	if err != nil {
		return fmt.Errorf("%w: read staged blob %s: %v", ErrDigestMismatch, desc.Digest, err)
	}
	if n != desc.Size || !verifier.Verified() {
		return fmt.Errorf("%w: staged blob %s", ErrDigestMismatch, desc.Digest)
	}
	return nil
}

func blobPath(dir string, d digest.Digest) string {
	return filepath.Join(dir, ocispec.ImageBlobsDir, d.Algorithm().String(), d.Encoded())
}

// stagingKey maps an OperationID to a filesystem-safe staging directory name.
func stagingKey(id OperationID) string {
	sum := sha256.Sum256([]byte(id))
	return "op-" + hex.EncodeToString(sum[:16])
}

func removeStaged(path string) {
	if path != "" {
		_ = os.RemoveAll(path) // best-effort: an unrecorded staged copy is never trusted
	}
}
