package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionPromptFile(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "session-prompt.md")
	if err := os.WriteFile(promptPath, []byte("you are a couchpilot session"), 0644); err != nil {
		t.Fatal(err)
	}
	base := func() *Config {
		return &Config{
			SessionPromptPath:     promptPath,
			SessionPromptOnNew:    true,
			SessionPromptOnResume: true,
		}
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		resume bool
		want   string
	}{
		{"new, enabled", nil, false, promptPath},
		{"resume, enabled", nil, true, promptPath},
		{"new, OnNew off", func(c *Config) { c.SessionPromptOnNew = false }, false, ""},
		{"resume, OnResume off", func(c *Config) { c.SessionPromptOnResume = false }, true, ""},
		{"new stays on when only resume off", func(c *Config) { c.SessionPromptOnResume = false }, false, promptPath},
		{"empty path", func(c *Config) { c.SessionPromptPath = "" }, false, ""},
		{"missing file", func(c *Config) { c.SessionPromptPath = filepath.Join(dir, "nope.md") }, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			if tt.mutate != nil {
				tt.mutate(c)
			}
			if got := c.sessionPromptFile(tt.resume); got != tt.want {
				t.Errorf("sessionPromptFile(%v) = %q, want %q", tt.resume, got, tt.want)
			}
		})
	}

	// An empty file must be treated as "nothing to inject".
	emptyPath := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(emptyPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	c := base()
	c.SessionPromptPath = emptyPath
	if got := c.sessionPromptFile(false); got != "" {
		t.Errorf("empty file: got %q, want empty", got)
	}
}
