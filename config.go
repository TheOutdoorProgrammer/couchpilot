package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Port                  int      `json:"port"`
	DefaultDir            string   `json:"defaultDir"`
	FavoriteDirs          []string `json:"favoriteDirs"`
	ProjectRoots          []string `json:"projectRoots"`
	DefaultPermissionMode string   `json:"defaultPermissionMode"`
	DefaultModel          string   `json:"defaultModel"`
	DefaultEffort         string   `json:"defaultEffort"`
	DefaultChannels       string   `json:"defaultChannels"`
	PluginDirs            []string `json:"pluginDirs"`
	ChannelsEnabled       bool     `json:"channelsEnabled"`

	configPath string
}

func LoadConfig() (*Config, error) {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".config", "couchpilot")
	configPath := filepath.Join(configDir, "config.json")

	cfg := &Config{
		Port:                  7080,
		DefaultDir:            "~/",
		FavoriteDirs:          []string{"~/"},
		DefaultPermissionMode: "bypassPermissions",
		configPath:            configPath,
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		os.MkdirAll(configDir, 0755)
		cfg.Save()
		return cfg, nil
	}

	json.Unmarshal(data, cfg)
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
