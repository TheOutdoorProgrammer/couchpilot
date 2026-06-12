<div align="center">

# 🛋️ couchpilot

**The ultimate mobile vibe-coding cockpit for [Claude Code](https://www.anthropic.com/claude-code).**

Launch, monitor, and steer Claude Code remote-control sessions from your phone, tablet, or any browser — without ever opening a terminal. Built for the couch.

</div>

---

## What is couchpilot?

Claude Code can run as a **remote-control session**: a headless agent on your machine that you drive from the Claude mobile app or [claude.ai/code](https://claude.ai/code). The catch is starting and managing those sessions — that still means SSHing in, remembering flags, and babysitting processes from a laptop.

couchpilot is the missing front end. It's a tiny Go service that runs on your dev machine and gives you a **dark, mobile-first web UI** to:

- spin up new Claude Code sessions with the model, effort, permission mode, working directory, and git branch you want — all from a thumb-friendly sheet,
- see every running and recently-ended session at a glance, with live status,
- jump straight into any session in the Claude app, or kill it, with one tap,
- keep a persistent "channels" session alive that bridges **iMessage → Claude Code**, so you can text your agents.

The whole point: you're on the couch, you have an idea, you pull out your phone, and thirty seconds later an agent is working on it. That's the vibe.

> **One binary, no dependencies.** The entire web UI is embedded in the Go binary via `go:embed`. Download it, run it, open the page. That's the install.

---

## Features

- **🚀 One-tap session launch** — pick a project (with live git status), branch, model, effort level, and permission mode from a mobile-native bottom sheet. Create a new branch on the fly.
- **📡 Live session dashboard** — every session with a real-time status dot (starting / active / dead), driven by Server-Sent Events. No refresh button.
- **🔁 Auto-detection & recovery** — couchpilot notices when a session ends (killed from the app, the UI, or timed out) and updates instantly. Restart couchpilot and it re-adopts the sessions still running.
- **💬 iMessage channels** — keep a dedicated session alive that bridges iMessage to Claude Code, auto-restarting if it dies. Text your agents.
- **🔐 Password authentication** — on by default, with a one-tap setup. Disable it for a trusted private network if you prefer.
- **📁 Project roots & favorites** — point couchpilot at the folders where your projects live; they show up in the picker with branch, ahead/behind, and dirty-file counts.
- **🧠 Live model catalog** — the model picker is populated from the latest Claude models, with aliases and a custom-ID escape hatch.
- **🔑 Built-in `claude login`** — authenticate Claude Code from the UI via an interactive pseudo-terminal, complete with on-screen arrow/enter/esc keys.
- **🍎 Runs as a macOS service** — one command installs a LaunchAgent that starts on boot and stays up.
- **🧹 Clean process management** — sessions run in their own process group, so killing one reaps its whole child tree (node, MCP servers) instead of orphaning it.

---

## Requirements

- **[Claude Code](https://www.anthropic.com/claude-code)** v2.1.51+ with remote-control support (the `claude` binary must be on your `PATH`).
- A **Claude Pro, Max, Team, or Enterprise** subscription — remote control uses your claude.ai auth, not an API key.
- **macOS** for the `install`/LaunchAgent commands. The server itself is cross-platform (macOS + Linux); only the service installer is macOS-specific.
- **Go 1.26+** only if you're building from source.

---

## Install

### Option 1 — Download a release binary (recommended)

Grab the archive for your platform from the [latest release](https://github.com/TheOutdoorProgrammer/couchpilot/releases/latest), extract it, and put the binary on your `PATH`:

```bash
tar -xzf couchpilot_*_darwin_arm64.tar.gz
sudo mv couchpilot /usr/local/bin/
couchpilot version
```

### Option 2 — `go install`

```bash
go install github.com/TheOutdoorProgrammer/couchpilot@latest
```

### Option 3 — Build from source

```bash
git clone https://github.com/TheOutdoorProgrammer/couchpilot.git
cd couchpilot
go build -o couchpilot .
```

---

## Quick start

```bash
couchpilot
```

Open **http://localhost:7080**. On first run with auth enabled you'll be asked to set a password (or disable auth for a trusted network) — see [Authentication](#authentication).

Override the port:

```bash
couchpilot --port 8080
```

### Run as a macOS service

```bash
couchpilot install
```

This writes a LaunchAgent to `~/Library/LaunchAgents/com.couchpilot.server.plist`, starts it, and keeps it running across reboots. Logs go to `~/.config/couchpilot/stdout.log` and `stderr.log`.

```bash
couchpilot uninstall   # stop and remove the service
```

After rebuilding, restart the running service with:

```bash
launchctl kickstart -k gui/$(id -u)/com.couchpilot.server
```

### CLI reference

| Command | Description |
| --- | --- |
| `couchpilot` | Start the server (foreground). |
| `couchpilot --port <n>` | Start on a specific port. |
| `couchpilot install` | Install & start the macOS LaunchAgent. |
| `couchpilot uninstall` | Stop & remove the LaunchAgent. |
| `couchpilot version` | Print version, commit, and build date. |

---

## Authentication

couchpilot can launch agents that **run arbitrary code on your machine**, so by default it requires a password. The protection is a bcrypt-hashed password plus a signed, HTTP-only, `SameSite=Lax` session cookie that survives restarts (no re-login every time the server bounces). Cross-origin state-changing requests are rejected as CSRF defense-in-depth.

- **First run:** with auth enabled and no password set, the UI shows a one-time setup screen. Set a password, **or** tap *Disable auth (trusted network)* if couchpilot is only reachable on a network you trust.
- **Change / disable later:** **Settings → Security**. Toggle *Require password* off, or set a new password. These changes apply immediately (not on *Save*).
- **Locked out / forgot the password:** delete `~/.config/couchpilot/auth.json` and restart. couchpilot regenerates its signing secret and drops back to first-run setup.

The password hash and signing secret live in `~/.config/couchpilot/auth.json` (mode `0600`), **separate** from `config.json`, and are never exposed through the API.

> ⚠️ couchpilot serves plain HTTP. If you expose it beyond your LAN, put it behind something that terminates TLS, or use a private overlay network like [Tailscale](https://tailscale.com). A password over plain HTTP on the open internet is not enough.

---

## Configuration

Config lives at `~/.config/couchpilot/config.json` and is created with sensible defaults on first run. Most fields are editable from the **Settings** gear in the UI.

```json
{
  "port": 7080,
  "host": "",
  "defaultDir": "~/",
  "favoriteDirs": ["~/", "~/projects/myapp"],
  "projectRoots": ["~/projects/src"],
  "defaultPermissionMode": "bypassPermissions",
  "defaultModel": "",
  "defaultEffort": "",
  "channelsEnabled": false,
  "defaultChannels": "",
  "pluginDirs": [],
  "authEnabled": true
}
```

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `port` | int | `7080` | HTTP listen port. |
| `host` | string | `""` | Bind address. Empty = all interfaces. Set `127.0.0.1` to restrict to localhost. |
| `defaultDir` | string | `"~/"` | Working directory used when a new session doesn't specify one. |
| `favoriteDirs` | string[] | `["~/"]` | Pinned directories shown at the top of the project picker. |
| `projectRoots` | string[] | — | Parent folders; their immediate subdirectories populate the picker with git status. |
| `defaultPermissionMode` | string | `"bypassPermissions"` | Default Claude permission mode (see below). |
| `defaultModel` | string | `""` | Default model ID (empty = Claude Code's default). |
| `defaultEffort` | string | `""` | Default effort level (`max`, `xhigh`, `high`, `medium`, `low`, or empty). |
| `channelsEnabled` | bool | `false` | Auto-start and supervise the iMessage channels session. |
| `defaultChannels` | string | `""` | Channel identifier passed to `--channels`. |
| `pluginDirs` | string[] | — | Local plugin directories loaded via `--plugin-dir` on the channels session. |
| `authEnabled` | bool | `true` | Require a password to use the UI. |

### Permission modes

couchpilot exposes Claude Code's permission modes per session and as a default:

| Mode | Behavior |
| --- | --- |
| `bypassPermissions` | Skips every permission prompt (`--dangerously-skip-permissions`). Maximum vibe, zero friction — and zero guardrails. |
| `acceptEdits` | Auto-accepts file edits, prompts for other actions. |
| `plan` | Plan mode — proposes before acting. |
| `auto` / `dontAsk` / `default` | Standard Claude Code permission behaviors. |

> The UI flags `bypassPermissions` with a warning, because in that mode a launched agent can run any command without asking.

---

## Using couchpilot

### Launch a session

Tap **+ New Session** and fill in the sheet:

- **Title** — optional; auto-generated (e.g. `swift-falcon`) if left blank.
- **Working Directory** — search your favorites and project roots, each shown with branch, ahead/behind arrows, and uncommitted-file count. Or enter a custom path.
- **Branch** — check out an existing branch, or create a new one from a base branch, before the session starts.
- **Permission Mode**, **Model**, **Effort** — default to your configured values; override per session.

Tap **Launch**. couchpilot spawns the `claude` process, scrapes the claude.ai remote-control URL from its output, and the session appears with a live status dot.

### Drive a session

Tap a session card to expand it:

- **Open in Claude** — jumps to the session in the Claude app / claude.ai.
- **Kill** — terminates the session (and its child processes).
- **Dismiss** — clears a dead session from the list.

### Settings

The **gear** opens settings, organized into sections:

- **Session Defaults** — permission mode, model, effort.
- **Projects** — manage project roots and favorite directories.
- **Channels** — enable/configure the iMessage channels session.
- **Security** — password and auth toggle.
- **Claude Account** — run `claude login` from the browser.

Changes are staged and applied on **Save** (except Security, which applies immediately).

---

## iMessage channels

couchpilot can keep a dedicated **channels** session alive that bridges iMessage to Claude Code. With `channelsEnabled: true`, it spawns the session on startup and auto-restarts it if it dies. Enable and configure it under **Settings → Channels**.

### Using a forked iMessage plugin

To use a fork (e.g. `imessage@theoutdoorprogrammer` instead of the official `imessage@claude-plugins-official`), three things are required:

**1. Managed-settings allowlist** (requires sudo):

```jsonc
// /Library/Application Support/ClaudeCode/managed-settings.json
{
  "allowedChannelPlugins": [
    { "marketplace": "theoutdoorprogrammer", "plugin": "imessage" }
  ]
}
```

Claude Code **only** honors `allowedChannelPlugins` from managed settings — putting it in `~/.claude/settings.json` does nothing.

**2. Marketplace registration.** Claude Code resolves `plugin:name@marketplace` by looking up `~/.claude/plugins/marketplaces/<marketplace>/`:

```
~/.claude/plugins/marketplaces/theoutdoorprogrammer/
├── .claude-plugin/marketplace.json    # manifest listing the plugin
└── external_plugins/
    └── imessage -> /path/to/your/fork  # symlink to the fork source
```

Register it in `~/.claude/plugins/known_marketplaces.json`.

**3. couchpilot config:**

```json
{
  "channelsEnabled": true,
  "defaultChannels": "plugin:imessage@theoutdoorprogrammer",
  "pluginDirs": ["/path/to/your/fork"]
}
```

### What doesn't work

- `allowedChannelPlugins` in user settings — silently ignored; managed settings only.
- `--dangerously-load-development-channels` — interactive prompt, unreliable delivery.
- Symlinks in `~/.claude/plugins/cache/` — destroyed on every session start.
- `--plugin-dir` alone without the marketplace — the plugin loads but the channel identifier can't resolve.

---

## How it works

Each session spawns a `claude --remote-control <name>` process under a pseudo-terminal, with the flags for your chosen model, effort, permission mode, and channels. couchpilot:

1. starts the process and scrapes the `claude.ai` remote-control URL from its output,
2. tracks liveness — via the PTY while it owns the process, and via signal-0 polling for sessions re-adopted after a restart,
3. broadcasts state changes to every connected browser over **Server-Sent Events**, so the dashboard is always live,
4. persists session metadata to `~/.config/couchpilot/sessions.json` and re-adopts still-running processes on startup.

Sessions are started in their own session/process group, so terminating one cleanly signals the whole group — Claude's child processes (node, MCP servers, hooks) get reaped instead of orphaned.

Set `COUCHPILOT_DEBUG=1` in the environment to log raw session PTY output for troubleshooting. It's off by default — the Claude TUI redraws constantly and would otherwise flood the logs.

### State & files

| Path | Purpose |
| --- | --- |
| `~/.config/couchpilot/config.json` | Configuration. |
| `~/.config/couchpilot/auth.json` | Password hash + signing secret (`0600`). |
| `~/.config/couchpilot/sessions.json` | Persisted session metadata. |
| `~/.config/couchpilot/stdout.log`, `stderr.log` | LaunchAgent logs. |
| `~/Library/LaunchAgents/com.couchpilot.server.plist` | macOS service definition. |

---

## Accessing from your phone

couchpilot binds to all interfaces by default, so on your home network you can reach it at `http://<your-machine>.local:7080` (or its LAN IP). Keep **auth enabled** if anything other than you can touch that network.

For access from anywhere — or to avoid exposing it on a shared LAN — put your machine on a private [Tailscale](https://tailscale.com) network and hit it over the tailnet. Add a TLS-terminating reverse proxy if you want HTTPS.

---

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| **"account not found" / sessions won't start** | Make sure `claude` is on the `PATH` the service sees, and that you've run `claude login`. From the LaunchAgent, `PATH` is `/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`. |
| **Locked out of the UI** | Delete `~/.config/couchpilot/auth.json` and restart — back to first-run setup. |
| **Session stuck on "Waiting for URL…"** | The process didn't print a claude.ai URL yet. Check `stderr.log`, or run with `COUCHPILOT_DEBUG=1` to see raw output. |
| **Channels session keeps dying** | Verify the forked-plugin setup above; check `stderr.log` for the spawn error. |
| **Can't reach it from your phone** | Confirm the machine's firewall allows the port and you're on the same network (or tailnet). |

---

## Development

```bash
go build -o couchpilot . && ./couchpilot            # build & run
COUCHPILOT_DEBUG=1 ./couchpilot                      # verbose session logging
go vet ./... && gofmt -l .                           # lint
```

The web UI lives in `static/` and is embedded into the binary at build time, so a plain `go build` produces a self-contained executable — no asset bundling step.

### Project layout

| File | Responsibility |
| --- | --- |
| `main.go` | CLI entry point, LaunchAgent install/uninstall, version. |
| `server.go` | HTTP server, routes, SSE hub, auth middleware, config API. |
| `session.go` | Session lifecycle — spawn, PTY scan, liveness, persistence. |
| `auth.go` | Password hashing, signed cookie tokens, auth storage. |
| `config.go` | Config load/save. |
| `login.go` | Interactive `claude login` over a PTY. |
| `projects.go` | Project discovery + cached git status. |
| `models.go` | Live Claude model catalog. |
| `static/` | Embedded SPA (`index.html`, `app.js`, `style.css`). |

---

## Releasing

Releases are automated with [GoReleaser](https://goreleaser.com) and GitHub Actions. The **tag is the version** — it's injected into the binary (`couchpilot version`) and shown in the UI.

```bash
git tag v1.2.3
git push origin v1.2.3
```

Pushing a `v*` tag triggers `.github/workflows/release.yml`, which cross-compiles macOS and Linux binaries (amd64 + arm64), archives them with checksums, and publishes a GitHub Release. Validate the config locally with `goreleaser check` or do a dry run with `goreleaser release --snapshot --clean`.

---

## License

[MIT](LICENSE) © Joey Stout
