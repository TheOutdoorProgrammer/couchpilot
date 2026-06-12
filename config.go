package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

type Config struct {
	Port                  int      `json:"port"`
	Host                  string   `json:"host"`
	DefaultDir            string   `json:"defaultDir"`
	FavoriteDirs          []string `json:"favoriteDirs"`
	ProjectRoots          []string `json:"projectRoots"`
	DefaultPermissionMode string   `json:"defaultPermissionMode"`
	DefaultModel          string   `json:"defaultModel"`
	DefaultEffort         string   `json:"defaultEffort"`
	DefaultChannels       string   `json:"defaultChannels"`
	PluginDirs            []string `json:"pluginDirs"`
	ChannelsEnabled       bool     `json:"channelsEnabled"`
	AuthEnabled           bool     `json:"authEnabled"`

	configPath string
}

func LoadConfig() (*Config, error) {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".config", "couchpilot")
	// Override for running isolated instances (tests, dev) without touching
	// the real state.
	if d := os.Getenv("COUCHPILOT_CONFIG_DIR"); d != "" {
		configDir = d
	}
	configPath := filepath.Join(configDir, "config.json")

	cfg := &Config{
		Port:                  7080,
		DefaultDir:            "~/",
		FavoriteDirs:          []string{"~/"},
		DefaultPermissionMode: "bypassPermissions",
		AuthEnabled:           true,
		configPath:            configPath,
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		os.MkdirAll(configDir, 0755)
		cfg.Save()
		return cfg, nil
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		// A corrupt config shouldn't silently boot with half-applied defaults —
		// surface it and keep the known-good defaults above.
		log.Printf("config: %s is invalid (%v); using defaults", configPath, err)
		cfg = &Config{
			Port:                  7080,
			DefaultDir:            "~/",
			FavoriteDirs:          []string{"~/"},
			DefaultPermissionMode: "bypassPermissions",
			AuthEnabled:           true,
		}
	}
	cfg.configPath = configPath
	return cfg, nil
}

func (c *Config) Save() error {
	os.MkdirAll(filepath.Dir(c.configPath), 0755)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.configPath, data, 0644)
}

func (c *Config) DataDir() string {
	return filepath.Dir(c.configPath)
}
