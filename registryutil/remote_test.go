package registryutil

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func TestNewRetryHTTPClient_InsecureTLSAndCustomTransport(t *testing.T) {
	baseTransport := &http.Transport{}
	client, err := NewRetryHTTPClient(RemoteConfig{
		InsecureTLS: true,
		Transport:   baseTransport,
	})
	if err != nil {
		t.Fatalf("NewRetryHTTPClient: %v", err)
	}

	retryTransport, ok := client.Transport.(*retry.Transport)
	if ok {
		base, ok := retryTransport.Base.(*http.Transport)
		if !ok {
			t.Fatalf("expected base transport, got %T", retryTransport.Base)
		}
		if base.TLSClientConfig == nil || !base.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("expected insecure tls on cloned transport")
		}
		return
	}
	t.Fatalf("expected retry transport, got %T", client.Transport)
}

func TestNewRepository_UsesAuthProviderAndCapability(t *testing.T) {
	referrersCapable := false
	providerCalled := false
	provider := func(context.Context, string) (auth.Credential, error) {
		providerCalled = true
		return auth.Credential{AccessToken: "token"}, nil
	}

	repo, err := NewRepository("example.com/project/repo", RemoteConfig{
		AuthProvider:        provider,
		ReferrersCapability: &referrersCapable,
	})
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	if repo == nil {
		t.Fatal("expected repository")
	}
	if repo.Reference.Registry != "example.com" {
		t.Fatalf("unexpected registry: %q", repo.Reference.Registry)
	}
	authClient, ok := repo.Client.(*auth.Client)
	if !ok {
		t.Fatalf("expected auth.Client, got %T", repo.Client)
	}
	if _, err := authClient.Credential(context.Background(), "example.com"); err != nil {
		t.Fatalf("credential provider returned error: %v", err)
	}
	if !providerCalled {
		t.Fatal("expected auth provider to be called")
	}
}

func TestNewRepository_UsernameTokenUsesBasicPassword(t *testing.T) {
	repo, err := NewRepository("example.com/project/repo", RemoteConfig{
		Username: "user",
		Token:    "pat",
	})
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	authClient, ok := repo.Client.(*auth.Client)
	if !ok {
		t.Fatalf("expected auth.Client, got %T", repo.Client)
	}
	cred, err := authClient.Credential(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("credential provider returned error: %v", err)
	}
	if cred.Username != "user" || cred.Password != "pat" {
		t.Fatalf("credential = %+v, want username user with token as password", cred)
	}
	if cred.AccessToken != "" {
		t.Fatalf("expected no bearer access token, got %q", cred.AccessToken)
	}
}

func TestNewRepository_TokenWithoutUsernameUsesBearer(t *testing.T) {
	repo, err := NewRepository("example.com/project/repo", RemoteConfig{
		Token: "bearer",
	})
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	authClient, ok := repo.Client.(*auth.Client)
	if !ok {
		t.Fatalf("expected auth.Client, got %T", repo.Client)
	}
	cred, err := authClient.Credential(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("credential provider returned error: %v", err)
	}
	if cred.AccessToken != "bearer" {
		t.Fatalf("AccessToken = %q, want bearer", cred.AccessToken)
	}
}

func TestNewRetryHTTPClient_PlainHTTPLeavesTLSUnset(t *testing.T) {
	client, err := NewRetryHTTPClient(RemoteConfig{
		PlainHTTP: true,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: false}},
	})
	if err != nil {
		t.Fatalf("NewRetryHTTPClient: %v", err)
	}
	if client == nil {
		t.Fatal("expected client")
	}
}

func TestNewRepository_InvalidReferenceTypedError(t *testing.T) {
	_, err := NewRepository("not a valid reference", RemoteConfig{})
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("expected ErrTransport, got %v", err)
	}
}

func TestLoadCertPool_InvalidPEMTypedError(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadCertPool(caFile)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}
