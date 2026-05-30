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
	// TODO 통일적으로 config 읽는 코드는 이런식으로 표준을 정하자.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, transportError("LoadConfig", "resolve path", err)
	}
	// (방어 코드) TODO 심볼릭 링크로 들어왔을대 읽지 않고 에러 리턴함. 방어적 코드. 이거 다른 코드에도 적용하자.
	fi, err := os.Lstat(abs) // 심볼릭 링크를 따라가지 않고 메타정보 조회
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notFoundError("LoadConfig", fmt.Sprintf("config file not found: %s", abs), err)
		}
		return nil, transportError("LoadConfig", "stat config", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, validationError("LoadConfig", fmt.Sprintf("config is not a regular file: %s", abs), nil)
	}

	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) { // 또는 fs.ErrNotExist
			return nil, notFoundError("LoadConfig", fmt.Sprintf("config file not found: %s", abs), err)
		}
		return nil, transportError("LoadConfig", "open config", err)
	}

	// TODO defer close 이렇게 하는 거 표준으로 정하자. 다른곳에서는 다소 다르게 하고 있는데.
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

// EnsureDir sori-oci.json 에 있는 path 에 실제 디렉토리가 있는지, TODO 수정해줘야 함. 루트 권한의 폴더에 대해서는 에러 리턴함. 오류는 아님.
func (conf *Config) EnsureDir() error {
	// 방어적 코드
	if conf == nil {
		return validationError("EnsureDir", "cannot ensure directory from a nil config", nil)
	}
	if conf.Local.Path == "" {
		return validationError("EnsureDir", "local.path is empty", nil)
	}
	// 해당 디렉토리가 있으면 넘어감.
	p := filepath.Clean(conf.Local.Path)
	info, err := os.Stat(p)
	if err == nil {
		if info.IsDir() {
			Log.Infof("%s is ready", p)
			return nil
		}
		return validationError("EnsureDir", fmt.Sprintf("path '%s' already exists but is not a directory", p), nil)
	}
	// 해당 디렉토리가 없으면 만들어줌. /var/lib/sori/oci 여기를 디폴트로 잡아주긴 하는데 이건 루트 사용자만 만들 수 있다.
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.MkdirAll(p, defaultDirPerm); mkdirErr != nil {
			return transportError("EnsureDir", fmt.Sprintf("create directory '%s'", p), mkdirErr)
		}
		Log.Infof("Created directory: %s", p)
		return nil
	}
	return transportError("EnsureDir", fmt.Sprintf("check directory '%s'", p), err)
}
