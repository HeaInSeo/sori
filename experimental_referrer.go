package sori

// experimental_referrer.go contains the NodeVault-oriented referrer helpers.
// These APIs remain in the root package for compatibility, but they are not
// yet part of the preferred long-lived core surface.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/seoyhaein/sori/registryutil"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orasoras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const (
	// MediaTypeToolSpec is the current media type used by the experimental tool
	// referrer helpers.
	MediaTypeToolSpec = "application/vnd.nodevault.toolspec.v1+json"

	// MediaTypeDataSpec is the current media type used by the experimental data
	// referrer helpers.
	MediaTypeDataSpec = "application/vnd.nodevault.dataspec.v1+json"

	// MediaTypeToolProfile is the media type for observed dry-run profile referrers.
	// It carries validationHash, observedIoProfile, observedResourceProfile, and
	// contractCheck for a given tool image.
	MediaTypeToolProfile = "application/vnd.nodevault.toolprofile.v1+json"

	// MediaTypeSecurityScan is the media type for security scan result referrers.
	// It carries CVE summary, scanner identity, and policy evaluation results
	// for a given tool image.
	MediaTypeSecurityScan = "application/vnd.nodevault.security.v1+json"
)

// ReferrerTarget is the target interface accepted by the experimental referrer
// helpers.
//
// Experimental: this target abstraction is tied to the current referrer
// helpers and is not yet part of the frozen core contract.
type ReferrerTarget interface {
	orasoras.Target
}

// SpecReferrerResult reports the manifest uploaded by the experimental
// PushToolSpecReferrer and PushDataSpecReferrer helpers.
//
// Experimental: this type is not yet part of the frozen core contract.
type SpecReferrerResult struct {
	ReferrerDigest string
	SubjectDigest  string
	MediaType      string
}

// PushToolSpecReferrer uploads a tool-oriented OCI referrer artifact.
//
// Experimental: this helper is NodeVault-oriented, subject to change, and not
// yet part of the intended long-lived core surface.
func PushToolSpecReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, specJSON []byte) (SpecReferrerResult, error) {
	return pushSpecReferrer(ctx, target, subjectDigest, specJSON, MediaTypeToolSpec)
}

// PushDataSpecReferrer uploads a data-oriented OCI referrer artifact.
//
// Experimental: this helper is NodeVault-oriented, subject to change, and not
// yet part of the intended long-lived core surface.
func PushDataSpecReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, specJSON []byte) (SpecReferrerResult, error) {
	return pushSpecReferrer(ctx, target, subjectDigest, specJSON, MediaTypeDataSpec)
}

// PushToolProfileReferrer uploads an observed dry-run profile referrer artifact
// for the given tool image. The profileJSON must be a valid JSON document
// conforming to the toolprofile payload defined in OBSERVED_PROFILE_SPEC.md.
//
// Experimental: this helper is NodeVault-oriented, subject to change, and not
// yet part of the intended long-lived core surface.
func PushToolProfileReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, profileJSON []byte) (SpecReferrerResult, error) {
	return pushSpecReferrer(ctx, target, subjectDigest, profileJSON, MediaTypeToolProfile)
}

// PushSecurityReferrer uploads a security scan result referrer artifact for the
// given tool image. The securityJSON must be a valid JSON document conforming
// to the security referrer payload defined in SECURITY_SCAN_SPEC.md.
//
// Experimental: this helper is NodeVault-oriented, subject to change, and not
// yet part of the intended long-lived core surface.
func PushSecurityReferrer(ctx context.Context, target ReferrerTarget, subjectDigest string, securityJSON []byte) (SpecReferrerResult, error) {
	return pushSpecReferrer(ctx, target, subjectDigest, securityJSON, MediaTypeSecurityScan)
}

// NewReferrerLocalStore opens an OCI layout store for the experimental
// referrer helpers.
//
// Experimental: this helper is specific to the current referrer API.
func NewReferrerLocalStore(path string) (ReferrerTarget, error) {
	store, err := oci.New(path)
	if err != nil {
		return nil, transportError("NewReferrerLocalStore", "open OCI layout store", err)
	}
	return store, nil
}

// NewReferrerRemoteRepository creates a remote repository for the experimental
// referrer helpers.
//
// Experimental: this helper is specific to the current referrer API.
func NewReferrerRemoteRepository(repoRef string, plainHTTP bool, credential *auth.Credential) (ReferrerTarget, error) {
	cfg := registryutil.RemoteConfig{PlainHTTP: plainHTTP}
	if credential != nil {
		cfg.Username = credential.Username
		cfg.Password = credential.Password
		cfg.Token = credential.AccessToken
	}
	repo, err := registryutil.NewRepository(repoRef, cfg)
	if err != nil {
		return nil, transportError("NewReferrerRemoteRepository", "create remote repository "+repoRef, err)
	}
	return repo, nil
}

// MarshalSpec marshals the payload used by the experimental referrer helpers.
//
// Experimental: this helper remains available for the current referrer API but
// is not yet part of the frozen core contract.
func MarshalSpec(v interface{}) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, transportError("MarshalSpec", "marshal spec JSON", err)
	}
	return data, nil
}

func pushSpecReferrer(
	ctx context.Context,
	target ReferrerTarget,
	subjectDigest string,
	specJSON []byte,
	mediaType string,
) (SpecReferrerResult, error) {
	if subjectDigest == "" {
		return SpecReferrerResult{}, validationError("pushSpecReferrer", "subjectDigest must not be empty", nil)
	}
	if len(specJSON) == 0 {
		return SpecReferrerResult{}, validationError("pushSpecReferrer", "specJSON must not be empty", nil)
	}

	configDesc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    godigest.FromBytes(specJSON),
		Size:      int64(len(specJSON)),
	}
	if err := pushBlobIfAbsent(ctx, target, configDesc, specJSON); err != nil {
		return SpecReferrerResult{}, transportError("pushSpecReferrer", "push config blob", err)
	}

	subjectDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    godigest.Digest(subjectDigest),
	}
	manifestDesc, err := orasoras.PackManifest(
		ctx, target,
		orasoras.PackManifestVersion1_1,
		ocispec.MediaTypeImageManifest,
		orasoras.PackManifestOptions{
			Subject:          &subjectDesc,
			ConfigDescriptor: &configDesc,
			Layers:           []ocispec.Descriptor{},
		},
	)
	if err != nil {
		return SpecReferrerResult{}, transportError("pushSpecReferrer", "pack referrer manifest", err)
	}

	return SpecReferrerResult{
		ReferrerDigest: manifestDesc.Digest.String(),
		SubjectDigest:  subjectDigest,
		MediaType:      mediaType,
	}, nil
}

func pushBlobIfAbsent(ctx context.Context, target ReferrerTarget, desc ocispec.Descriptor, data []byte) error {
	if err := target.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		if !isExistError(err) {
			return err
		}
	}
	return nil
}

func isExistError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, needle := range []string{"already exists", "conflict", "409"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
