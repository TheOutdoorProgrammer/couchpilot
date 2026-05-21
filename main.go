package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			cmdInstall()
			return
		case "uninstall":
			cmdUninstall()
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
	sm := NewSessionManager(cfg.DataDir(), hub)
	srv := NewServer(cfg, sm, hub)

	log.Printf("couchpilot listening on http://localhost:%d", cfg.Port)
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
}

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
}

func cmdUninstall() {
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.couchpilot.server.plist")

	uid := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", uid, plistPath).Run()
	os.Remove(plistPath)

	fmt.Println("couchpilot uninstalled")
}
