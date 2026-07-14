package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SessionStatus string

const (
	StatusStarting SessionStatus = "starting"
	StatusActive   SessionStatus = "active"
	StatusDead     SessionStatus = "dead"
)

// deadRetention is how long dead sessions stay visible (and resumable) in the
// UI, both across restarts and in the periodic cleanup.
const deadRetention = 24 * time.Hour

type Session struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Dir            string        `json:"dir"`
	PID            int           `json:"pid"` // claude's pid, reported by the shim
	ShimPID        int           `json:"shimPid,omitempty"`
	SessionUUID    string        `json:"sessionUuid,omitempty"` // claude conversation id; the --resume handle
	URL            string        `json:"url"`
	Status         SessionStatus `json:"status"`
	PermissionMode string        `json:"permissionMode"`
	Model          string        `json:"model,omitempty"`
	Effort         string        `json:"effort,omitempty"`
	Channels       string        `json:"channels,omitempty"`
	PluginDirs     []string      `json:"pluginDirs,omitempty"`
	IsChannels     bool          `json:"isChannels"`
	ReviewMode     bool          `json:"reviewMode"`        // gate file writes behind code review
	Discard        bool          `json:"discard,omitempty"` // deliberately restarted: don't auto-resume
	CreatedAt      time.Time     `json:"createdAt"`
	DiedAt         *time.Time    `json:"diedAt,omitempty"`

	// gen increments on every (re)spawn; a watcher goroutine exits when the
	// session moves to a generation it doesn't own (e.g. a resume raced it).
	gen int
}

type SessionManager struct {
	sessions       map[string]*Session
	mu             sync.RWMutex
	hub            *SSEHub
	dataDir        string
	onChannelsDied func()

	// onSessionDied/onSessionDismissed let the review manager resolve pending
	// reviews for sessions that go away; hookPort/hookToken flow to spawned
	// claude processes so their hooks can call back into this couchpilot.
	onSessionDied      func(id string)
	onSessionDismissed func(id string)
	hookPort           int
	hookToken          string
	// cfgSnapshot returns the current couchpilot config; wired during server
	// setup so a spawn can read live settings (e.g. session-prompt injection).
	// Nil in tests that construct the manager without a server.
	cfgSnapshot func() Config
}

// SetHookEnv provides the local API coordinates the review hook needs; it
// must be called before any session spawns.
func (sm *SessionManager) SetHookEnv(port int, token string) {
	sm.hookPort = port
	sm.hookToken = token
}

func NewSessionManager(dataDir string, hub *SSEHub) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*Session),
		hub:      hub,
		dataDir:  dataDir,
	}
	sm.recover()
	go sm.cleanupLoop()
	return sm
}

func (sm *SessionManager) stateDir(id string) string {
	return filepath.Join(sm.dataDir, "sessions", id)
}

type CreateSessionOpts struct {
	Name         string
	Dir          string
	PermMode     string
	Model        string
	Effort       string
	Channels     string
	PluginDirs   []string
	IsChannels   bool
	ReviewMode   bool
	Branch       string
	CreateBranch bool
	BranchFrom   string
}

func (sm *SessionManager) CreateSession(opts CreateSessionOpts) (*Session, error) {
	dir := opts.Dir
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	} else if dir == "~" {
		dir, _ = os.UserHomeDir()
	}

	name := opts.Name
	if name == "" {
		name = generateName()
	}

	branch := strings.TrimSpace(opts.Branch)
	if branch != "" {
		if !isGitRepo(dir) {
			log.Printf("skipping branch %q: %s is not a git repository", branch, dir)
		} else if err := CheckoutBranch(dir, branch, opts.CreateBranch, opts.BranchFrom); err != nil {
			verb := "checkout"
			if opts.CreateBranch {
				verb = "create"
			}
			return nil, fmt.Errorf("%s branch %q: %w", verb, branch, err)
		}
		InvalidateProjectCache(opts.Dir)
	}

	s := &Session{
		ID:             generateID(),
		Name:           name,
		Dir:            dir,
		SessionUUID:    generateUUID(),
		Status:         StatusStarting,
		PermissionMode: opts.PermMode,
		Model:          opts.Model,
		Effort:         opts.Effort,
		Channels:       opts.Channels,
		PluginDirs:     opts.PluginDirs,
		IsChannels:     opts.IsChannels,
		ReviewMode:     opts.ReviewMode,
		CreatedAt:      time.Now(),
	}
	// The RC TUI no longer prints its URL, but it is deterministic from the
	// conversation id we just picked. The shim overrides this if an older
	// claude does print one.
	s.URL = "https://claude.ai/code/" + s.SessionUUID

	if err := sm.spawnShim(s, false); err != nil {
		return nil, err
	}

	sm.mu.Lock()
	sm.sessions[s.ID] = s
	snap := *s
	gen := s.gen
	sm.mu.Unlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_created", Data: &snap})

	go sm.watchSession(s, gen)

	return s, nil
}

// ResumeSession relaunches a dead session's claude process with --resume, so
// the conversation continues where it left off (and re-registers with Remote
// Control under the same name).
func (sm *SessionManager) ResumeSession(id string) (*Session, error) {
	sm.mu.Lock()
	s, ok := sm.sessions[id]
	if !ok {
		sm.mu.Unlock()
		return nil, os.ErrNotExist
	}
	if s.Status != StatusDead {
		sm.mu.Unlock()
		return nil, errors.New("session is still running")
	}
	if s.SessionUUID == "" {
		sm.mu.Unlock()
		return nil, errors.New("session predates resume support — no conversation id recorded")
	}
	if !sessionFileExists(s.SessionUUID) {
		sm.mu.Unlock()
		return nil, errors.New("no saved conversation to resume — the session never exchanged a message")
	}
	s.Status = StatusStarting
	s.DiedAt = nil
	s.Discard = false
	s.PID = 0
	s.ShimPID = 0
	s.gen++
	sm.mu.Unlock()

	if err := sm.spawnShim(s, true); err != nil {
		sm.markDead(s.ID)
		return nil, err
	}

	sm.mu.RLock()
	snap := *s
	gen := s.gen
	sm.mu.RUnlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_updated", Data: &snap})

	go sm.watchSession(s, gen)

	return &snap, nil
}

// spawnShim launches the detached shim that owns the session's PTY. The shim
// is setsid'd into its own session, so couchpilot restarts don't touch it —
// that's the whole point: claude survives couchpilot.
func (sm *SessionManager) spawnShim(s *Session, resume bool) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	stateDir := sm.stateDir(s.ID)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return err
	}
	// Stale state from a previous run of this session would race the watcher.
	os.Remove(filepath.Join(stateDir, shimStateFile))
	os.Remove(filepath.Join(stateDir, shimTailFile))

	// The review hook is wired into every session via a generated settings
	// file; whether it actually gates anything is decided server-side per
	// session, so review mode can be toggled mid-session without a respawn.
	settingsPath := filepath.Join(stateDir, "hooks.json")
	if err := writeHookSettings(settingsPath, self); err != nil {
		return err
	}

	args := []string{"_shim", "-state", stateDir}
	if s.PermissionMode == "bypassPermissions" {
		args = append(args, "-accept-bypass")
	}
	args = append(args, "--", claudePath)
	args = append(args, buildClaudeArgs(s, resume)...)
	// Inject the couchpilot session-prompt (appended, never replacing the default
	// system prompt or CLAUDE.md files) when enabled for this new/resume spawn.
	if sm.cfgSnapshot != nil {
		cfg := sm.cfgSnapshot()
		if pf := cfg.sessionPromptFile(resume); pf != "" {
			args = append(args, "--append-system-prompt-file", pf)
		}
	}
	args = append(args, "--settings", settingsPath)

	cmd := exec.Command(self, args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(),
		"COUCHPILOT_SESSION_ID="+s.ID,
		fmt.Sprintf("COUCHPILOT_PORT=%d", sm.hookPort),
		"COUCHPILOT_HOOK_TOKEN="+sm.hookToken,
	)
	logf, err := os.OpenFile(filepath.Join(stateDir, shimLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		defer logf.Close()
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap only; lifecycle is tracked via state file + pid polls

	sm.mu.Lock()
	s.ShimPID = cmd.Process.Pid
	sm.mu.Unlock()
	return nil
}

// writeHookSettings generates the per-session claude settings file that wires
// the file tools through `couchpilot _hook`. The PreToolUse timeout is a day:
// the hook legitimately blocks for as long as a human takes to review.
func writeHookSettings(path, exe string) error {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type hookMatcher struct {
		Matcher string    `json:"matcher"`
		Hooks   []hookCmd `json:"hooks"`
	}
	settings := map[string]map[string][]hookMatcher{
		"hooks": {
			"PreToolUse": {{
				Matcher: "Write|Edit|MultiEdit|NotebookEdit",
				Hooks:   []hookCmd{{Type: "command", Command: fmt.Sprintf("%q _hook pre", exe), Timeout: 86400}},
			}},
			"PostToolUse": {{
				Matcher: "Write|Edit|MultiEdit|NotebookEdit",
				Hooks:   []hookCmd{{Type: "command", Command: fmt.Sprintf("%q _hook post", exe), Timeout: 30}},
			}},
		},
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// SetReviewMode flips the code-review gate for a running session. The hook is
// always installed, so this takes effect on the session's next file write.
func (sm *SessionManager) SetReviewMode(id string, on bool) error {
	sm.mu.Lock()
	s, ok := sm.sessions[id]
	if !ok {
		sm.mu.Unlock()
		return os.ErrNotExist
	}
	s.ReviewMode = on
	snap := *s
	sm.mu.Unlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_updated", Data: &snap})
	return nil
}

// SendInput writes data to a running session's PTY via the shim's Unix socket.
func (sm *SessionManager) SendInput(id, input string) error {
	sockPath := filepath.Join(sm.stateDir(id), shimInputSocket)
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to session input: %w", err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write([]byte(input))
	return err
}

// ChangeModel restarts a running session with a different model. The session is
// killed and resumed with --model <modelID>, preserving conversation history.
// modelID is a Claude model identifier (e.g. "claude-opus-4-6", "sonnet").
func (sm *SessionManager) ChangeModel(id, modelID string) error {
	sm.mu.Lock()
	s, ok := sm.sessions[id]
	if !ok {
		sm.mu.Unlock()
		return os.ErrNotExist
	}
	if s.Status == StatusDead {
		sm.mu.Unlock()
		return errors.New("session is dead")
	}
	if s.SessionUUID == "" {
		sm.mu.Unlock()
		return errors.New("session has no conversation id")
	}

	oldShimPID := s.ShimPID
	s.Model = modelID
	s.Status = StatusStarting
	s.PID = 0
	s.ShimPID = 0
	s.gen++
	sm.mu.Unlock()

	if oldShimPID > 0 && pidAlive(oldShimPID) {
		syscall.Kill(oldShimPID, syscall.SIGTERM)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && pidAlive(oldShimPID) {
			time.Sleep(100 * time.Millisecond)
		}
		if pidAlive(oldShimPID) {
			syscall.Kill(oldShimPID, syscall.SIGKILL)
		}
	}

	if err := sm.spawnShim(s, true); err != nil {
		sm.markDead(id)
		return fmt.Errorf("respawn with new model: %w", err)
	}

	sm.mu.RLock()
	snap := *s
	gen := s.gen
	sm.mu.RUnlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_updated", Data: &snap})

	go sm.watchSession(s, gen)

	return nil
}

// ReviewModeOn reports whether a session currently has the review gate on —
// the hook asks this (via the server) on every file write.
func (sm *SessionManager) ReviewModeOn(id string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.sessions[id]
	return ok && s.ReviewMode
}

func buildClaudeArgs(s *Session, resume bool) []string {
	args := []string{"--remote-control", s.Name}
	if resume {
		args = append(args, "--resume", s.SessionUUID)
	} else {
		args = append(args, "--session-id", s.SessionUUID)
	}
	if s.PermissionMode == "bypassPermissions" {
		args = append(args, "--dangerously-skip-permissions")
	} else if s.PermissionMode != "" && s.PermissionMode != "default" {
		args = append(args, "--permission-mode", s.PermissionMode)
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
	}
	if s.Channels != "" {
		args = append(args, "--channels", s.Channels)
	}
	for _, pd := range s.PluginDirs {
		args = append(args, "--plugin-dir", pd)
	}
	return args
}

// watchSession follows a session through its shim's state file and pid. It is
// the only status-transition driver for shim-backed sessions, both freshly
// spawned and re-adopted after a couchpilot restart.
func (sm *SessionManager) watchSession(s *Session, gen int) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	bridgeDone := false
	tick := 0
	for range ticker.C {
		tick++
		sm.mu.RLock()
		id, shimPID, status, curGen := s.ID, s.ShimPID, s.Status, s.gen
		sm.mu.RUnlock()
		if curGen != gen || status == StatusDead {
			return // a resume superseded this watcher, or the session was dismissed
		}

		// Once active, swap the synthesized URL for the canonical one from
		// claude's own session file. The file (or its bridge line) can appear
		// well after activation — first message for an idle session — so keep
		// trying on a slow cadence until it lands.
		if !bridgeDone && status == StatusActive && tick%5 == 0 {
			sm.mu.RLock()
			uuid := s.SessionUUID
			sm.mu.RUnlock()
			if uuid == "" {
				bridgeDone = true
			} else if u := bridgeURL(uuid); u != "" {
				bridgeDone = true
				sm.mu.Lock()
				changed := s.URL != u
				s.URL = u
				snap := *s
				sm.mu.Unlock()
				if changed {
					sm.persist()
					sm.hub.Broadcast(SSEEvent{Type: "session_updated", Data: &snap})
				}
			}
		}

		st, err := readShimState(sm.stateDir(id))
		if err == nil {
			if st.Phase == ShimExited {
				sm.markDead(id)
				return
			}
			changed := false
			sm.mu.Lock()
			if st.ClaudePID != 0 && s.PID != st.ClaudePID {
				s.PID = st.ClaudePID
				changed = true
			}
			if st.URL != "" && s.URL != st.URL {
				s.URL = st.URL
				changed = true
			}
			if st.Phase == ShimActive && s.Status == StatusStarting {
				s.Status = StatusActive
				changed = true
			}
			snap := *s
			sm.mu.Unlock()
			if changed {
				sm.persist()
				sm.hub.Broadcast(SSEEvent{Type: "session_updated", Data: &snap})
			}
		}

		if !pidAlive(shimPID) {
			sm.markDead(id)
			return
		}
	}
}

func (sm *SessionManager) KillSession(id string) error {
	sm.mu.RLock()
	s, ok := sm.sessions[id]
	var shimPID, claudePID int
	var status SessionStatus
	if ok {
		shimPID, claudePID, status = s.ShimPID, s.PID, s.Status
	}
	sm.mu.RUnlock()
	if !ok {
		return os.ErrNotExist
	}
	if status == StatusDead {
		return nil
	}
	// Normal path: ask the shim, which forwards SIGTERM to claude's process
	// group and reaps it.
	if shimPID > 0 && pidAlive(shimPID) {
		return syscall.Kill(shimPID, syscall.SIGTERM)
	}
	// Fallback (legacy pre-shim session, or the shim crashed): signal claude's
	// process group directly so its children (node, MCP servers) die too.
	if claudePID > 0 {
		if err := syscall.Kill(-claudePID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return syscall.Kill(claudePID, syscall.SIGTERM)
		}
	}
	return nil
}

// SetDiscard marks a session as deliberately retired: when it dies, automatic
// recovery (channels auto-resume) must not bring its conversation back.
func (sm *SessionManager) SetDiscard(id string) {
	sm.mu.Lock()
	if s, ok := sm.sessions[id]; ok {
		s.Discard = true
	}
	sm.mu.Unlock()
	sm.persist()
}

func (sm *SessionManager) DismissSession(id string) {
	sm.mu.Lock()
	s, ok := sm.sessions[id]
	if ok && s.Status == StatusDead {
		delete(sm.sessions, id)
	} else {
		ok = false
	}
	sm.mu.Unlock()
	if ok {
		os.RemoveAll(sm.stateDir(id))
		if sm.onSessionDismissed != nil {
			sm.onSessionDismissed(id)
		}
	}
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_dismissed", Data: map[string]string{"id": id}})
}

func (sm *SessionManager) GetSessions() []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make([]*Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (sm *SessionManager) markDead(id string) {
	sm.mu.Lock()
	s, ok := sm.sessions[id]
	if !ok || s.Status == StatusDead {
		sm.mu.Unlock()
		return
	}
	now := time.Now()
	s.Status = StatusDead
	s.DiedAt = &now
	isChannels := s.IsChannels
	snap := *s
	sm.mu.Unlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_died", Data: &snap})

	if sm.onSessionDied != nil {
		sm.onSessionDied(id)
	}
	if isChannels && sm.onChannelsDied != nil {
		go func() {
			time.Sleep(3 * time.Second)
			sm.onChannelsDied()
		}()
	}
}

// recover re-adopts sessions after a couchpilot restart. Shim-backed sessions
// whose shim is still running are picked up live — this is what makes
// couchpilot restarts invisible to sessions. Everything else is marked dead
// (and stays resumable when it has a conversation id).
func (sm *SessionManager) recover() {
	sessions, err := sm.loadFromDisk()
	if err != nil {
		return
	}

	now := time.Now()
	for _, s := range sessions {
		if s.Status == StatusDead {
			if s.DiedAt != nil && time.Since(*s.DiedAt) < deadRetention {
				sm.sessions[s.ID] = s
			}
			continue
		}

		sm.sessions[s.ID] = s

		if s.ShimPID > 0 {
			st, stErr := readShimState(sm.stateDir(s.ID))
			shimRunning := pidAlive(s.ShimPID) && pidCommandContains(s.ShimPID, "couchpilot")
			if shimRunning && (stErr != nil || st.Phase != ShimExited) {
				if stErr == nil {
					if st.ClaudePID != 0 {
						s.PID = st.ClaudePID
					}
					if st.URL != "" {
						s.URL = st.URL
					}
					if st.Phase == ShimActive && s.Status == StatusStarting {
						s.Status = StatusActive
					}
				}
				go sm.watchSession(s, s.gen)
				continue
			}
			s.Status = StatusDead
			s.DiedAt = &now
			continue
		}

		// Legacy session from before the shim existed: couchpilot held its PTY
		// directly, so all we can do is poll the claude pid for liveness.
		if s.PID > 0 && pidAlive(s.PID) && pidCommandContains(s.PID, "claude") {
			s.Status = StatusActive
			go sm.pollLegacy(s)
		} else {
			s.Status = StatusDead
			s.DiedAt = &now
		}
	}

	sm.persist()
}

func (sm *SessionManager) pollLegacy(s *Session) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !pidAlive(s.PID) {
			sm.markDead(s.ID)
			return
		}
	}
}

func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		var removed []string
		sm.mu.Lock()
		for id, s := range sm.sessions {
			if s.Status == StatusDead && s.DiedAt != nil && time.Since(*s.DiedAt) > deadRetention {
				delete(sm.sessions, id)
				removed = append(removed, id)
			}
		}
		sm.mu.Unlock()
		for _, id := range removed {
			os.RemoveAll(sm.stateDir(id))
		}
		sm.persist()
	}
}

func (sm *SessionManager) persist() {
	sm.mu.RLock()
	sessions := make([]*Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		sessions = append(sessions, s)
	}
	sm.mu.RUnlock()

	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		log.Printf("persist: %v", err)
		return
	}
	os.MkdirAll(sm.dataDir, 0755)
	os.WriteFile(filepath.Join(sm.dataDir, "sessions.json"), data, 0644)
}

func (sm *SessionManager) loadFromDisk() ([]*Session, error) {
	data, err := os.ReadFile(filepath.Join(sm.dataDir, "sessions.json"))
	if err != nil {
		return nil, err
	}
	var sessions []*Session
	return sessions, json.Unmarshal(data, &sessions)
}

// pidAlive reports whether a process exists. Guard against pid<=0: kill(0)
// and kill(-n) address process groups, not processes.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// pidCommandContains sanity-checks that a recovered pid still belongs to the
// process we think it does — after a reboot, pids get reshuffled and a stale
// sessions.json could otherwise adopt some unrelated process.
func pidCommandContains(pid int, substr string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), substr)
}

// sessionFile locates claude's persisted conversation for this id, anywhere
// under ~/.claude/projects — the uuid is globally unique, so no need to
// reproduce claude's path-munging scheme.
func sessionFile(uuid string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", uuid+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// sessionFileExists reports whether claude persisted a conversation for this
// id. Sessions that never exchanged a message have nothing on disk and
// `claude --resume` would hang on an interactive picker.
func sessionFileExists(uuid string) bool {
	return sessionFile(uuid) != ""
}

// bridgePattern extracts the Remote Control registration id that claude
// records in its session file. The claude.ai app addresses RC sessions by
// this id (cse_X maps to /code/session_X), not by the conversation uuid, so
// a URL built from it is the canonical deep link.
var bridgePattern = regexp.MustCompile(`"bridgeSessionId":"cse_([A-Za-z0-9_-]+)"`)

// bridgeURL returns the canonical claude.ai URL for the session's current RC
// registration, or "" if claude hasn't written one (yet). The last match wins:
// every (re)launch registers a fresh bridge id.
func bridgeURL(uuid string) string {
	path := sessionFile(uuid)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	ms := bridgePattern.FindAllSubmatch(data, -1)
	if len(ms) == 0 {
		return ""
	}
	return "https://claude.ai/code/session_" + string(ms[len(ms)-1][1])
}

func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// generateUUID returns a random v4 UUID, used as the claude --session-id so
// couchpilot always knows the resume handle for every session it spawns.
func generateUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ansiPattern strips CSI sequences (including `?`-prefixed private modes like
// the cursor show/hide \x1b[?25h that the TUI emits constantly), OSC strings,
// charset selectors, and stray C0 control bytes. Kept in sync with the
// browser-side regex in app.js.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[=>]|[\x00-\x08\x0b\x0c\x0e-\x1f]`)

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

var adjectives = []string{
	"swift", "calm", "bold", "warm", "cool", "keen", "bright", "quiet",
	"quick", "sharp", "smooth", "steady", "fresh", "gentle", "vivid",
	"crisp", "clear", "nimble", "proud", "brave",
}

var nouns = []string{
	"falcon", "cedar", "river", "summit", "breeze", "ember", "compass",
	"harbor", "meadow", "canyon", "lantern", "tide", "ridge", "aurora",
	"grove", "drift", "spark", "cove", "peak", "trail",
}

func generateName() string {
	ai, err1 := rand.Int(rand.Reader, big.NewInt(int64(len(adjectives))))
	ni, err2 := rand.Int(rand.Reader, big.NewInt(int64(len(nouns))))
	if err1 != nil || err2 != nil {
		return fmt.Sprintf("session-%d", time.Now().Unix())
	}
	return fmt.Sprintf("%s-%s", adjectives[ai.Int64()], nouns[ni.Int64()])
}
