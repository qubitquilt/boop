package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// withTempHome swaps in a temp dir as HOME and sets XDG_CONFIG_HOME
// so Load/Write are isolated from the developer's real config. The
// returned cleanup restores the originals.
func withTempHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOOP_API_URL", "")
	t.Setenv("BOOP_RUNNER_TOKEN", "")
	t.Setenv("BOOP_DASHBOARD_TOKEN", "")
	p, _ := defaultFile()
	return p
}

func TestLoadDefaultsWhenNoFile(t *testing.T) {
	path := withTempHome(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != DefaultAPIURL {
		t.Errorf("APIURL = %q, want %q", cfg.APIURL, DefaultAPIURL)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected no config file to exist at %s", path)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	withTempHome(t)
	// Write a file with a different URL.
	t.Setenv("BOOP_API_URL", "https://receiver.example.com:9090/")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Env should win over the default; trailing slash stripped.
	if cfg.APIURL != "https://receiver.example.com:9090" {
		t.Errorf("APIURL = %q, want https://receiver.example.com:9090", cfg.APIURL)
	}
}

func TestLoadEnvOverridesFileValue(t *testing.T) {
	path := withTempHome(t)
	t.Setenv("BOOP_API_URL", "https://from-env.example.com")
	// Write a file with a different URL; env should win.
	if err := Write(FileConfig{APIURL: "https://from-file.example.com", RunnerToken: "secret"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != "https://from-env.example.com" {
		t.Errorf("APIURL = %q, want https://from-env.example.com (env should win)", cfg.APIURL)
	}
	if cfg.RunnerToken != "secret" {
		t.Errorf("RunnerToken = %q, want secret", cfg.RunnerToken)
	}
	_ = path
}

func TestLoadIgnoresEmptyEnv(t *testing.T) {
	path := withTempHome(t)
	// Write a populated file.
	if err := Write(FileConfig{APIURL: "https://from-file.example.com", RunnerToken: "secret"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Now set an empty env var — it should NOT blank the file value.
	t.Setenv("BOOP_API_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != "https://from-file.example.com" {
		t.Errorf("APIURL = %q, want https://from-file.example.com (empty env should be a no-op)", cfg.APIURL)
	}
	if cfg.RunnerToken != "secret" {
		t.Errorf("RunnerToken = %q, want secret", cfg.RunnerToken)
	}
	_ = path
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	withTempHome(t)
	path, _ := defaultFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestWriteRoundTrip(t *testing.T) {
	path := withTempHome(t)
	in := FileConfig{APIURL: "https://roundtrip.example.com", RunnerToken: "tok123"}
	if err := Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// File mode must be 0600 — it carries a secret.
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", fi.Mode().Perm())
	}
	var got FileConfig
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RunnerToken != in.RunnerToken {
		t.Errorf("RunnerToken = %q, want %q", got.RunnerToken, in.RunnerToken)
	}
	// APIURL written empty (since the file only had what we passed);
	// Load should fall back to the default.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != "https://roundtrip.example.com" {
		t.Errorf("APIURL = %q, want https://roundtrip.example.com", cfg.APIURL)
	}
}
