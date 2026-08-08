// Package config holds the Boop CLI's configuration model.
//
// Config is resolved from three layers, lowest to highest precedence:
//
//  1. Built-in defaults (the zero values below plus a default API URL).
//  2. A JSON file at $XDG_CONFIG_HOME/boop/config.json (falls back to
//     ~/.config/boop/config.json). The file is optional; a `boop config
//     write` command materializes it.
//  3. Environment variables BOOP_API_URL, BOOP_RUNNER_TOKEN, and
//     BOOP_DASHBOARD_TOKEN. These let an AI agent or CI override
//     per-invocation without touching the user's config file.
//
// The runner token is the shared secret the receiver gates the
// runner-only POST endpoints with (X-BOOP-Runner-Token). The dashboard
// token gates /dashboard/*; the CLI doesn't currently need it, but we
// accept it so `boop config show` can verify the full picture.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileConfig is the on-disk shape of config.json. Field names are
// stable; the env-var names map to the exported Go fields one-to-one.
type FileConfig struct {
	// APIURL is the base URL of the boop receiver. Trailing
	// slashes are tolerated and stripped at resolution time.
	APIURL string `json:"api_url"`
	// RunnerToken is the X-BOOP-Runner-Token value forwarded
	// to POST endpoints (/api/runs/{id}/status, /telemetry,
	// /stages, /heartbeat, /lens_telemetry, /rerun).
	RunnerToken string `json:"runner_token,omitempty"`
	// DashboardToken is the BOOP_DASHBOARD_TOKEN equivalent
	// for the dashboard routes. The CLI does not use the
	// dashboard routes today, but storing it lets
	// `boop config show` verify the full credential surface
	// and keeps the file shape extensible.
	DashboardToken string `json:"dashboard_token,omitempty"`
}

// DefaultAPIURL is the assumed receiver location when no config or env
// var is present. Mirrors the receiver's in-cluster service as declared
// in apps/k8s/base/deployment.yaml.
const DefaultAPIURL = "http://boop-receiver.dev-tools.svc.cluster.local:8080"

// defaultFile returns the path to the config file, honoring
// XDG_CONFIG_HOME and falling back to ~/.config/boop/config.json.
func defaultFile() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "boop", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "boop", "config.json"), nil
}

// Load reads the config file (if present) and layers env overrides on
// top. A missing config file is not an error — the defaults + env vars
// alone are sufficient for an agent that sets BOOP_API_URL per job.
func Load() (FileConfig, error) {
	path, err := defaultFile()
	if err != nil {
		return FileConfig{}, err
	}
	cfg := FileConfig{}
	if b, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return FileConfig{}, fmt.Errorf("config: read %s: %w", path, readErr)
	}
	// Env vars win. An empty env var is a no-op (does not blank
	// a value from the file) so a stale empty export doesn't
	// clobber a populated file.
	if v := os.Getenv("BOOP_API_URL"); v != "" {
		cfg.APIURL = v
	}
	if v := os.Getenv("BOOP_RUNNER_TOKEN"); v != "" {
		cfg.RunnerToken = v
	}
	if v := os.Getenv("BOOP_DASHBOARD_TOKEN"); v != "" {
		cfg.DashboardToken = v
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}
	// Strip trailing slashes so URL joins don't emit double slashes.
	// (We intentionally do NOT call filepath.ToSlash here — that is a
	// filesystem-path operation and is semantically wrong on a URL.)
	for len(cfg.APIURL) > 0 && cfg.APIURL[len(cfg.APIURL)-1] == '/' {
		cfg.APIURL = cfg.APIURL[:len(cfg.APIURL)-1]
	}
	return cfg, nil
}

// Path returns the config file path (for diagnostics / `boop config
// path`).
func Path() (string, error) {
	return defaultFile()
}

// Write materializes the given config to the config file, creating
// ~/.config/boop (or $XDG_CONFIG_HOME/boop) if needed. Used by
// `boop config write`.
func Write(cfg FileConfig) error {
	path, err := defaultFile()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	// Mode 0o600: the runner token is a secret and must not
	// be world-readable on a shared dev box.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}
