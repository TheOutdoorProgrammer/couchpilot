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

	// SessionPrompt* inject a context file into every spawned session via
	// `claude --append-system-prompt-file`, so sessions know they run under
	// couchpilot. Read live at spawn time and gated per new/resume, so toggling
	// takes effect on the next spawn without a respawn.
	SessionPromptPath     string `json:"sessionPromptPath"`
	SessionPromptOnNew    bool   `json:"sessionPromptOnNew"`
	SessionPromptOnResume bool   `json:"sessionPromptOnResume"`

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
		SessionPromptPath:     "~/.config/couchpilot/session-prompt.md",
		SessionPromptOnNew:    true,
		SessionPromptOnResume: true,
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
			SessionPromptPath:     "~/.config/couchpilot/session-prompt.md",
			SessionPromptOnNew:    true,
			SessionPromptOnResume: true,
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

// sessionPromptFile returns the expanded path of the session prompt to inject on
// this spawn, or "" if injection is off for the new/resume case, the path is
// unset, or the file is missing/empty — so a bad or absent path never breaks a
// spawn (the flag is simply omitted).
func (c *Config) sessionPromptFile(resume bool) string {
	if (resume && !c.SessionPromptOnResume) || (!resume && !c.SessionPromptOnNew) {
		return ""
	}
	if c.SessionPromptPath == "" {
		return ""
	}
	p := expandPath(c.SessionPromptPath)
	if info, err := os.Stat(p); err != nil || info.IsDir() || info.Size() == 0 {
		return ""
	}
	return p
}
