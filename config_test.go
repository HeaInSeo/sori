package sori

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	conf, err := LoadConfig("sori-oci.json")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	err = conf.EnsureDir()
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("skipping EnsureDir: no permission to create %q (run as root or set a writable path)", conf.Local.Path)
		}
		t.Fatalf("EnsureDir failed: %v", err)
	}
}

func TestInitConfig(t *testing.T) {
	conf, err := InitConfig("sori-oci.json")
	if err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	err = conf.EnsureDir()
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("skipping EnsureDir: no permission to create %q (run as root or set a writable path)", conf.Local.Path)
		}
		t.Fatalf("EnsureDir failed: %v", err)
	}
}

// TestLoadConfig_TempDir verifies LoadConfig+EnsureDir with a writable temp path.
func TestLoadConfig_TempDir(t *testing.T) {
	tmp := t.TempDir()
	localPath := filepath.Join(tmp, "oci")

	cfg := Config{
		Local:   LocalStore{Type: "oci", Path: localPath},
		Remotes: []RemoteStore{{Name: "test", Registry: "reg.example.com", Repository: "test/repo"}},
	}
	cfgPath := filepath.Join(tmp, "test-sori.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	conf, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if err := conf.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir failed: %v", err)
	}

	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("expected directory to exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %q to be a directory", localPath)
	}
}

func TestConfigNewClient_UsesLocalPath(t *testing.T) {
	tmp := t.TempDir()
	localPath := filepath.Join(tmp, "oci")
	cfg := &Config{
		Local: LocalStore{Type: "oci", Path: localPath},
	}

	client := cfg.NewClient()
	if got := client.LocalStorePath(); got != localPath {
		t.Fatalf("LocalStorePath mismatch: got %q want %q", got, localPath)
	}
}

func TestLoadConfig_NotFoundTypedError(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// writeTestConfig writes a Config as JSON to a temp file and returns the path.
func writeTestConfig(t *testing.T, cfg Config) string {
	t.Helper()
	tmp := t.TempDir()
	p := filepath.Join(tmp, "sori.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func baseTestConfig(localPath string, remotes ...RemoteStore) Config {
	return Config{
		Local:   LocalStore{Type: "oci", Path: localPath},
		Remotes: remotes,
	}
}

func TestLoadConfig_EnvVarSubstitution_Password(t *testing.T) {
	t.Setenv("SORI_TEST_PASSWORD", "secret123")
	tmp := t.TempDir()
	cfg := baseTestConfig(filepath.Join(tmp, "oci"), RemoteStore{
		Name: "r1", Registry: "reg.example.com", Repository: "proj/repo",
		Auth: AuthConfig{Username: "user", Password: "${SORI_TEST_PASSWORD}"},
	})
	conf, err := LoadConfig(writeTestConfig(t, cfg))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := conf.Remotes[0].Auth.Password; got != "secret123" {
		t.Fatalf("Password: got %q want %q", got, "secret123")
	}
}

func TestLoadConfig_EnvVarSubstitution_Token(t *testing.T) {
	t.Setenv("SORI_TEST_TOKEN", "tok-abc")
	tmp := t.TempDir()
	cfg := baseTestConfig(filepath.Join(tmp, "oci"), RemoteStore{
		Name: "r1", Registry: "reg.example.com", Repository: "proj/repo",
		Auth: AuthConfig{Token: "${SORI_TEST_TOKEN}"},
	})
	conf, err := LoadConfig(writeTestConfig(t, cfg))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := conf.Remotes[0].Auth.Token; got != "tok-abc" {
		t.Fatalf("Token: got %q want %q", got, "tok-abc")
	}
}

func TestLoadConfig_EnvVarSubstitution_UndefinedVar(t *testing.T) {
	// An undefined variable expands to empty string (os.ExpandEnv behaviour).
	const envKey = "SORI_TEST_UNDEFINED_XYZ_99"
	if err := os.Unsetenv(envKey); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	tmp := t.TempDir()
	cfg := baseTestConfig(filepath.Join(tmp, "oci"), RemoteStore{
		Name: "r1", Registry: "reg.example.com", Repository: "proj/repo",
		Auth: AuthConfig{Password: "${" + envKey + "}"},
	})
	conf, err := LoadConfig(writeTestConfig(t, cfg))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := conf.Remotes[0].Auth.Password; got != "" {
		t.Fatalf("Password: got %q want empty string", got)
	}
}

func TestLoadConfig_EnvVarSubstitution_LiteralValue(t *testing.T) {
	// A plain string with no ${...} markers passes through unchanged.
	tmp := t.TempDir()
	cfg := baseTestConfig(filepath.Join(tmp, "oci"), RemoteStore{
		Name: "r1", Registry: "reg.example.com", Repository: "proj/repo",
		Auth: AuthConfig{Password: "plaintext-pass"},
	})
	conf, err := LoadConfig(writeTestConfig(t, cfg))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := conf.Remotes[0].Auth.Password; got != "plaintext-pass" {
		t.Fatalf("Password: got %q want %q", got, "plaintext-pass")
	}
}

func TestLoadConfig_EnvVarSubstitution_MultipleRemotes(t *testing.T) {
	t.Setenv("SORI_TEST_PASS_A", "passA")
	t.Setenv("SORI_TEST_PASS_B", "passB")
	tmp := t.TempDir()
	cfg := baseTestConfig(filepath.Join(tmp, "oci"),
		RemoteStore{Name: "r1", Registry: "reg.example.com", Repository: "a/repo",
			Auth: AuthConfig{Password: "${SORI_TEST_PASS_A}"}},
		RemoteStore{Name: "r2", Registry: "reg2.example.com", Repository: "b/repo",
			Auth: AuthConfig{Password: "${SORI_TEST_PASS_B}"}},
	)
	conf, err := LoadConfig(writeTestConfig(t, cfg))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := conf.Remotes[0].Auth.Password; got != "passA" {
		t.Fatalf("Remotes[0].Password: got %q want %q", got, "passA")
	}
	if got := conf.Remotes[1].Auth.Password; got != "passB" {
		t.Fatalf("Remotes[1].Password: got %q want %q", got, "passB")
	}
}
