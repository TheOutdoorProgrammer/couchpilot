package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// Build metadata, injected at release time via -ldflags by GoReleaser.
// Defaults apply to `go build`/`go run` and local development.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// VersionInfo is surfaced over the API and in the UI.
type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func versionInfo() VersionInfo {
	return VersionInfo{Version: version, Commit: commit, Date: date}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "_shim":
			cmdShim()
			return
		case "_hook":
			cmdHook()
			return
		case "install":
			cmdInstall()
			return
		case "uninstall":
			cmdUninstall()
			return
		case "version", "--version", "-v":
			fmt.Printf("couchpilot %s (commit %s, built %s)\n", version, commit, date)
			return
		}
	}

	port := flag.Int("port", 0, "override listening port")
	flag.Parse()

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *port > 0 {
		cfg.Port = *port
	}

	hub := NewSSEHub()
	rm, err := NewReviewManager(cfg.DataDir(), hub)
	if err != nil {
		log.Fatalf("reviews: %v", err)
	}
	sm := NewSessionManager(cfg.DataDir(), hub)
	sm.SetHookEnv(cfg.Port, rm.HookToken())
	rm.reviewOn = sm.ReviewModeOn
	sm.onSessionDied = rm.CancelForSession
	sm.onSessionDismissed = rm.DropSession
	lm := NewLoginManager(hub)
	am, err := NewAuthManager(cfg.DataDir())
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	pm, err := NewPushManager(cfg.DataDir())
	if err != nil {
		log.Fatalf("push: %v", err)
	}
	srv := NewServer(cfg, sm, lm, hub, am, rm, pm)

	log.Printf("couchpilot listening on http://localhost:%d", cfg.Port)
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
}

// defaultSessionPrompt seeds ~/.config/couchpilot/session-prompt.md on install.
// It's appended to every couchpilot session's system prompt (see Config.SessionPrompt*),
// so a session knows it's running under couchpilot. Users edit it freely.
const defaultSessionPrompt = `<!-- couchpilot session prompt: appended to the system prompt of every couchpilot
     session (toggle per new/resume in Settings). Edit freely — never overwritten. -->

This is a couchpilot session — a Claude Code remote-control session launched and
managed from couchpilot's web UI, not a local terminal.

- The person driving it may be on their phone; favor concise, skimmable replies
  over long walls of text.
- Your output is read in a web UI and may be relayed through a messaging channel,
  so don't rely on rich terminal formatting.
- Confirm before long-running, expensive, or destructive actions.
`

func cmdInstall() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		log.Fatal(err)
	}

	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".config", "couchpilot")
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.couchpilot.server.plist")

	os.MkdirAll(configDir, 0755)

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.couchpilot.server</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
		<key>HOME</key>
		<string>%s</string>
	</dict>
</dict>
</plist>`, exePath, home,
		filepath.Join(configDir, "stdout.log"),
		filepath.Join(configDir, "stderr.log"),
		home)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		log.Fatal(err)
	}

	uid := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", uid, plistPath).Run()
	if err := exec.Command("launchctl", "bootstrap", uid, plistPath).Run(); err != nil {
		log.Fatalf("launchctl bootstrap failed: %v", err)
	}

	fmt.Println("couchpilot installed and started")
	fmt.Printf("  UI: http://localhost:7080\n")
	fmt.Printf("  Plist: %s\n", plistPath)

	// Seed a default session-prompt file so new installs get the behavior, but
	// never clobber a customized one.
	if cfg, err := LoadConfig(); err == nil && cfg.SessionPromptPath != "" {
		promptPath := expandPath(cfg.SessionPromptPath)
		if _, statErr := os.Stat(promptPath); os.IsNotExist(statErr) {
			os.MkdirAll(filepath.Dir(promptPath), 0755)
			if werr := os.WriteFile(promptPath, []byte(defaultSessionPrompt), 0644); werr == nil {
				fmt.Printf("  Session prompt: %s (created — edit it to change what every couchpilot session is told)\n", promptPath)
			}
		} else {
			fmt.Printf("  Session prompt: %s (edit to customize what every couchpilot session is told)\n", promptPath)
		}
	}
}

func cmdUninstall() {
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.couchpilot.server.plist")

	uid := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", uid, plistPath).Run()
	os.Remove(plistPath)

	fmt.Println("couchpilot uninstalled")
}
