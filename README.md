# couchpilot

A lightweight Go service for managing Claude Code remote control sessions from your phone, tablet, or any browser.

Launch sessions, monitor their status, and kill them — all from a dark, mobile-friendly web UI. Built for the couch.

## Features

- **Multi-session management** — launch and monitor multiple remote control sessions simultaneously
- **Auto-detection** — detects when sessions end (whether you kill them from the app, UI, or they time out)
- **Persistent state** — survives restarts; re-adopts running sessions on startup
- **Mobile-first dark UI** — designed for phone use with touch-friendly controls
- **Configurable directories** — set favorite project directories for quick session launch
- **Permission control** — toggle `--dangerously-skip-permissions` per session (default: on)
- **LaunchAgent support** — runs as a macOS service, starts on boot

## Install

```bash
go install github.com/TheOutdoorProgrammer/couchpilot@latest
```

Or build from source:

```bash
git clone https://github.com/TheOutdoorProgrammer/couchpilot.git
cd couchpilot
go build -o couchpilot .
```

## Usage

### Start the server

```bash
couchpilot
```

Opens at `http://localhost:7080`. Override the port:

```bash
couchpilot --port 8080
```

### Install as a macOS service

```bash
couchpilot install
```

This creates a LaunchAgent at `~/Library/LaunchAgents/com.couchpilot.server.plist` that starts on boot and stays running.

### Uninstall the service

```bash
couchpilot uninstall
```

## Configuration

Config lives at `~/.config/couchpilot/config.json`:

```json
{
  "port": 7080,
  "defaultDir": "~/",
  "favoriteDirs": ["~/", "~/projects/myapp"],
  "defaultSkipPermissions": true
}
```

You can also edit these from the Settings gear in the UI.

## How it works

Each session spawns a `claude remote-control --spawn session` process. couchpilot monitors the process lifecycle:

1. You tap **+ New Session** in the UI
2. couchpilot spawns a claude remote-control process with your chosen options
3. The session appears in your Claude mobile app / claude.ai/code session list
4. When the session ends (killed from UI, from the app, or via timeout), couchpilot detects it and updates the UI in real-time via SSE

Session state is persisted to `~/.config/couchpilot/sessions.json`. If couchpilot restarts, it checks which processes are still alive and re-adopts them.

## Requirements

- **Go 1.22+** (for building)
- **Claude Code v2.1.51+** with remote control support
- **macOS** (LaunchAgent support is macOS-specific; the server itself is cross-platform)
- A Claude Pro, Max, Team, or Enterprise subscription (remote control requires claude.ai auth, not API keys)

## License

MIT
