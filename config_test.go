package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COUCHPILOT_CONFIG_DIR", dir)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Port != 7080 {
		t.Errorf("Port = %d, want 7080", cfg.Port)
	}
	if cfg.DefaultPermissionMode != "bypassPermissions" {
		t.Errorf("perm mode = %q", cfg.DefaultPermissionMode)
	}
	if !cfg.AuthEnabled {
		t.Error("auth should default on")
	}
	// First load writes the defaults to disk.
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Errorf("config.json not created: %v", err)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COUCHPILOT_CONFIG_DIR", dir)

	cfg, _ := LoadConfig()
	cfg.Port = 9999
	cfg.DefaultModel = "claude-opus-4-6"
	cfg.FavoriteDirs = []string{"~/a", "~/b"}
	cfg.ChannelsEnabled = true
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 9999 || got.DefaultModel != "claude-opus-4-6" {
		t.Errorf("roundtrip lost values: %+v", got)
	}
	if len(got.FavoriteDirs) != 2 || !got.ChannelsEnabled {
		t.Errorf("roundtrip lost slice/bool: %+v", got)
	}
}

func TestLoadConfigCorruptFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COUCHPILOT_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{ not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("corrupt config should not be fatal: %v", err)
	}
	// Falls back to known-good defaults rather than booting half-applied.
	if cfg.Port != 7080 || cfg.DefaultPermissionMode != "bypassPermissions" {
		t.Errorf("corrupt config did not fall back to defaults: %+v", cfg)
	}
	// configPath must still be wired so DataDir/Save work afterward.
	if cfg.DataDir() != dir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir(), dir)
	}
}

func TestDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COUCHPILOT_CONFIG_DIR", dir)
	cfg, _ := LoadConfig()
	if cfg.DataDir() != dir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir(), dir)
	}
}
