package sori

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type (
	Config struct {
		Local   LocalStore    `json:"local"`
		Remotes []RemoteStore `json:"remotes"`
	}
	LocalStore struct {
		Type string `json:"type"` // "oci"
		Path string `json:"path"`
	}
	RemoteStore struct {
		Name       string     `json:"name"`
		Type       string     `json:"type"`       // "registry"
		Registry   string     `json:"registry"`   // e.g. harbor.local
		Repository string     `json:"repository"` // e.g. harbor 인 경우 project/repo
		TLS        TLSConfig  `json:"tls"`
		Auth       AuthConfig `json:"auth"`
	}
	TLSConfig struct {
		Insecure bool   `json:"insecure"`
		CAFile   string `json:"ca_file"`
	}
	AuthConfig struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
)

const (
	defaultDirPerm  fs.FileMode = 0o755
	defaultOCIStore             = "/var/lib/sori/oci"
)

// Deprecated: use LoadConfig followed by Config.NewClient so new code stays on
// the preferred client-based core path.
func InitConfig(path string) (*Config, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	Log.Infof("oci store path: %s", cfg.Local.Path)
	return cfg, nil
}

// NewClient constructs the preferred core client path from configuration.
func (conf *Config) NewClient(opts ...ClientOption) *Client {
	allOpts := make([]ClientOption, 0, len(opts)+1)
	allOpts = append(allOpts, WithLocalStorePath(conf.Local.Path))
	allOpts = append(allOpts, opts...)
	return NewClient(allOpts...)
}

// LoadConfig reads and validates the JSON configuration file used by the
// preferred core client path.
func LoadConfig(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, transportError("LoadConfig", "resolve path", err)
	}
	fi, err := os.Lstat(abs) // Lstat so symlinks are rejected as non-regular files
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFoundError("LoadConfig", fmt.Sprintf("config file not found: %s", abs), err)
		}
		return nil, transportError("LoadConfig", "stat config", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, validationError("LoadConfig", fmt.Sprintf("config is not a regular file: %s", abs), nil)
	}

	// #nosec G304 -- config path is supplied explicitly by the caller.
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFoundError("LoadConfig", fmt.Sprintf("config file not found: %s", abs), err)
		}
		return nil, transportError("LoadConfig", "open config", err)
	}
	defer func() {
		if cErr := f.Close(); cErr != nil {
			Log.Warnf("failed to close file %s: %v", abs, cErr)
		}
	}()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, validationError("LoadConfig", "decode json", err)
	}

	expandCredentials(cfg.Remotes)

	if cfg.Local.Path == "" {
		return nil, validationError("LoadConfig", "local.path is empty", nil)
	}

	if cfg.Local.Type != "oci" {
		return nil, validationError("LoadConfig", fmt.Sprintf("local.type must be 'oci', but got '%s'", cfg.Local.Type), nil)
	}
	for i, r := range cfg.Remotes {
		if r.Name == "" || r.Registry == "" || r.Repository == "" {
			return nil, validationError("LoadConfig", fmt.Sprintf("remotes[%d] missing required fields", i), nil)
		}
	}

	return &cfg, nil
}

// expandCredentials replaces ${ENV_VAR} references in the secret credential
// fields of every remote store.  Username is not expanded — it is not a secret
// and callers seldom need to parameterise it.
func expandCredentials(remotes []RemoteStore) {
	for i := range remotes {
		remotes[i].Auth.Password = os.ExpandEnv(remotes[i].Auth.Password)
		remotes[i].Auth.Token = os.ExpandEnv(remotes[i].Auth.Token)
	}
}

// EnsureDir creates the local OCI store directory defined in the config if it
// does not already exist.  Returns ErrTransport if the directory cannot be
// created (e.g. insufficient permissions on the parent path).
func (conf *Config) EnsureDir() error {
	if conf == nil {
		return validationError("EnsureDir", "cannot ensure directory from a nil config", nil)
	}
	if conf.Local.Path == "" {
		return validationError("EnsureDir", "local.path is empty", nil)
	}
	p := filepath.Clean(conf.Local.Path)
	info, err := os.Stat(p)
	if err == nil {
		if info.IsDir() {
			Log.Infof("%s is ready", p)
			return nil
		}
		return validationError("EnsureDir", fmt.Sprintf("path '%s' already exists but is not a directory", p), nil)
	}
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.MkdirAll(p, defaultDirPerm); mkdirErr != nil {
			return transportError("EnsureDir", fmt.Sprintf("create directory '%s'", p), mkdirErr)
		}
		Log.Infof("Created directory: %s", p)
		return nil
	}
	return transportError("EnsureDir", fmt.Sprintf("check directory '%s'", p), err)
}
