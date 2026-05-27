package sori

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HeaInSeo/sori/registryutil"
)

type registryIntegrationConfig struct {
	registry            string
	repository          string
	username            string
	password            string
	token               string
	tag                 string
	plainHTTP           bool
	insecureTLS         bool
	referrersCapability *bool
}

func TestRegistryIntegration_PackagePushOnly(t *testing.T) {
	cfg := loadRegistryIntegrationConfig(t)

	ctx := context.Background()
	client := NewClient(WithLocalStorePath(filepath.Join(t.TempDir(), "oci")))

	pkg, err := client.PackageVolume(ctx, PackageRequest{
		SourceDir:   "./test-vol",
		DisplayName: "Integration Test Volume",
		Tag:         cfg.tag,
		Dataset:     "integration-dataset",
		Version:     "v1",
	})
	if err != nil {
		t.Fatalf("PackageVolume: %v", err)
	}

	pushResult, err := client.PushPackagedVolume(ctx, pkg, cfg.target())
	if err != nil {
		t.Fatalf("PushPackagedVolume: %v", err)
	}
	if pushResult.ManifestDigest == "" {
		t.Fatal("expected manifest digest after push")
	}
	if pushResult.Repository == "" {
		t.Fatal("expected repository after push")
	}
	assertRegistryResolveMatchesPush(t, ctx, cfg, pushResult)
}

func TestRegistryIntegration_PackagePushReferrer(t *testing.T) {
	cfg := loadRegistryIntegrationConfig(t)

	ctx := context.Background()
	client := NewClient(WithLocalStorePath(filepath.Join(t.TempDir(), "oci")))

	pkg, err := client.PackageVolume(ctx, PackageRequest{
		SourceDir:   "./test-vol",
		DisplayName: "Integration Test Volume",
		Tag:         cfg.tag,
		Dataset:     "integration-dataset",
		Version:     "v1",
	})
	if err != nil {
		t.Fatalf("PackageVolume: %v", err)
	}

	pushResult, err := client.PushPackagedVolume(ctx, pkg, cfg.target())
	if err != nil {
		t.Fatalf("PushPackagedVolume: %v", err)
	}
	if pushResult.ManifestDigest == "" {
		t.Fatal("expected manifest digest after push")
	}
	assertRegistryResolveMatchesPush(t, ctx, cfg, pushResult)

	spec, err := BuildDataSpec(pkg, pushResult, PackageRequest{
		SourceDir:   "./test-vol",
		DisplayName: "Integration Test Volume",
		Tag:         cfg.tag,
		Dataset:     "integration-dataset",
		Version:     "v1",
	})
	if err != nil {
		t.Fatalf("BuildDataSpec: %v", err)
	}

	referrerResult, err := PushRemoteDataSpecReferrer(ctx, pushResult, cfg.target(), spec)
	if err != nil {
		t.Fatalf("PushRemoteDataSpecReferrer: %v", err)
	}
	if referrerResult.ManifestDigest == "" {
		t.Fatal("expected referrer manifest digest")
	}
}

func assertRegistryResolveMatchesPush(t *testing.T, ctx context.Context, cfg registryIntegrationConfig, push *PushResult) {
	t.Helper()

	repo, err := registryutil.NewRepository(push.Repository, registryutil.RemoteConfig{
		PlainHTTP:           cfg.plainHTTP,
		InsecureTLS:         cfg.insecureTLS,
		Username:            cfg.username,
		Password:            cfg.password,
		Token:               cfg.token,
		ReferrersCapability: cfg.referrersCapability,
	})
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}

	resolved, err := repo.Resolve(ctx, push.Tag)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", push.Tag, err)
	}
	if resolved.Digest.String() != push.ManifestDigest {
		t.Fatalf("resolved digest mismatch: got %q want %q", resolved.Digest.String(), push.ManifestDigest)
	}
}

func loadRegistryIntegrationConfig(t *testing.T) registryIntegrationConfig {
	t.Helper()
	if os.Getenv("SORI_RUN_REGISTRY_INTEGRATION") == "" {
		t.Skip("skipping registry integration; set SORI_RUN_REGISTRY_INTEGRATION=1 to enable")
	}

	registry := os.Getenv("SORI_REGISTRY_HOST")
	repository := os.Getenv("SORI_REGISTRY_REPOSITORY")
	username := os.Getenv("SORI_REGISTRY_USERNAME")
	password := os.Getenv("SORI_REGISTRY_PASSWORD")
	token := os.Getenv("SORI_REGISTRY_TOKEN")
	tag := os.Getenv("SORI_REGISTRY_TAG")
	if tag == "" {
		tag = "integration.v1"
	}
	if registry == "" || repository == "" {
		t.Fatal("SORI_REGISTRY_HOST and SORI_REGISTRY_REPOSITORY are required")
	}

	plainHTTP := os.Getenv("SORI_REGISTRY_PLAIN_HTTP") == "1"
	insecureTLS := os.Getenv("SORI_REGISTRY_INSECURE_TLS") == "1"
	var referrersCapability *bool
	if raw := os.Getenv("SORI_REGISTRY_REFERRERS_CAPABLE"); raw != "" {
		v := raw == "1" || raw == "true"
		referrersCapability = &v
	}

	return registryIntegrationConfig{
		registry:            registry,
		repository:          repository,
		username:            username,
		password:            password,
		token:               token,
		tag:                 tag,
		plainHTTP:           plainHTTP,
		insecureTLS:         insecureTLS,
		referrersCapability: referrersCapability,
	}
}

func (c registryIntegrationConfig) target() RemoteTarget {
	return RemoteTarget{
		Registry:            c.registry,
		Repository:          c.repository,
		PlainHTTP:           c.plainHTTP,
		InsecureTLS:         c.insecureTLS,
		Username:            c.username,
		Password:            c.password,
		Token:               c.token,
		ReferrersCapability: c.referrersCapability,
	}
}
