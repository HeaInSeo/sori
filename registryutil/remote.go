package registryutil

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

type RemoteConfig struct {
	PlainHTTP           bool
	InsecureTLS         bool
	Username            string
	Password            string
	Token               string
	CAFile              string
	HTTPClient          *http.Client
	Transport           http.RoundTripper
	AuthProvider        auth.CredentialFunc
	ReferrersCapability *bool
}

func NewRepository(remoteRepo string, cfg RemoteConfig) (*remote.Repository, error) {
	repo, err := remote.NewRepository(remoteRepo)
	if err != nil {
		return nil, transportError("NewRepository", "connect to remote repository", err)
	}
	repo.PlainHTTP = cfg.PlainHTTP

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient, err = NewRetryHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}

	credentialFunc := cfg.AuthProvider
	if credentialFunc == nil {
		cred := auth.Credential{
			Username: cfg.Username,
			Password: cfg.Password,
		}
		if cred.Username != "" && cred.Password == "" && cfg.Token != "" {
			cred.Password = cfg.Token
		} else if cfg.Token != "" {
			cred.AccessToken = cfg.Token
		}
		credentialFunc = auth.StaticCredential(repo.Reference.Registry, cred)
	}
	repo.Client = &auth.Client{
		Client:     httpClient,
		Cache:      auth.NewCache(),
		Credential: credentialFunc,
	}
	if cfg.ReferrersCapability != nil {
		if err := repo.SetReferrersCapability(*cfg.ReferrersCapability); err != nil && err != remote.ErrReferrersCapabilityAlreadySet {
			return nil, validationError("NewRepository", "set referrers capability", err)
		}
	}
	return repo, nil
}

func NewRetryHTTPClient(cfg RemoteConfig) (*http.Client, error) {
	baseClient := retry.NewClient()
	transport, ok := baseClient.Transport.(*retry.Transport)
	if !ok {
		return baseClient, nil
	}
	base := transport.Base
	if cfg.Transport != nil {
		base = cfg.Transport
	}
	if base == nil {
		base = http.DefaultTransport
	}
	baseTransport, ok := base.(*http.Transport)
	if ok {
		clonedTransport := baseTransport.Clone()
		tlsConfig := &tls.Config{}
		if clonedTransport.TLSClientConfig != nil {
			tlsConfig = clonedTransport.TLSClientConfig.Clone()
		}
		tlsConfig.InsecureSkipVerify = cfg.InsecureTLS
		if !cfg.PlainHTTP && cfg.CAFile != "" {
			pool, err := LoadCertPool(cfg.CAFile)
			if err != nil {
				return nil, err
			}
			tlsConfig.RootCAs = pool
		}
		if !cfg.PlainHTTP || cfg.InsecureTLS {
			clonedTransport.TLSClientConfig = tlsConfig
		}
		base = clonedTransport
	}
	baseClient.Transport = retry.NewTransport(base)
	return baseClient, nil
}

func LoadCertPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, transportError("LoadCertPool", "read CA file "+caFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if ok := pool.AppendCertsFromPEM(pemBytes); !ok {
		return nil, validationError("LoadCertPool", "no valid certificates found in "+caFile, nil)
	}
	return pool, nil
}
