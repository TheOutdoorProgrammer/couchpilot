package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
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

	"github.com/creack/pty"
)

type SessionStatus string

const (
	StatusStarting SessionStatus = "starting"
	StatusActive   SessionStatus = "active"
	StatusDead     SessionStatus = "dead"
)

type Session struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Dir            string        `json:"dir"`
	PID            int           `json:"pid"`
	URL            string        `json:"url"`
	Status         SessionStatus `json:"status"`
	PermissionMode string        `json:"permissionMode"`
	IsChannels     bool          `json:"isChannels"`
	CreatedAt      time.Time     `json:"createdAt"`
	DiedAt         *time.Time    `json:"diedAt,omitempty"`

	cmd *exec.Cmd `json:"-"`
}

type SessionManager struct {
	sessions       map[string]*Session
	mu             sync.RWMutex
	hub            *SSEHub
	dataDir        string
	onChannelsDied func()
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

type CreateSessionOpts struct {
	Name         string
	Dir          string
	PermMode     string
	Model        string
	Effort       string
	Channels     string
	PluginDirs   []string
	IsChannels   bool
	Branch       string
	CreateBranch bool
	BranchFrom   string
}

func (sm *SessionManager) CreateSession(opts CreateSessionOpts) (*Session, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, err
	}

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

	permMode := opts.PermMode
	model := opts.Model
	effort := opts.Effort

	args := []string{"--remote-control", name}
	if permMode == "bypassPermissions" {
		args = append(args, "--dangerously-skip-permissions")
	} else if permMode != "" && permMode != "default" {
		args = append(args, "--permission-mode", permMode)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if opts.Channels != "" {
		args = append(args, "--channels", opts.Channels)
	}
	for _, pd := range opts.PluginDirs {
		args = append(args, "--plugin-dir", pd)
	}

	cmd := exec.Command(claudePath, args...)
	cmd.Dir = dir

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:             generateID(),
		Name:           name,
		Dir:            dir,
		PID:            cmd.Process.Pid,
		Status:         StatusStarting,
		PermissionMode: permMode,
		IsChannels:     opts.IsChannels,
		CreatedAt:      time.Now(),
		cmd:            cmd,
	}

	sm.mu.Lock()
	sm.sessions[s.ID] = s
	snap := *s
	sm.mu.Unlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_created", Data: &snap})

	go sm.scanOutput(s, ptmx)
	go sm.waitProcess(s)

	if permMode == "bypassPermissions" {
		go func() {
			time.Sleep(1 * time.Second)
			ptmx.Write([]byte("\x1b[B"))
			time.Sleep(200 * time.Millisecond)
			ptmx.Write([]byte("\r"))
		}()
	}

	return s, nil
}

func (sm *SessionManager) KillSession(id string) error {
	sm.mu.RLock()
	s, ok := sm.sessions[id]
	sm.mu.RUnlock()
	if !ok {
		return os.ErrNotExist
	}
	if s.Status == StatusDead {
		return nil
	}
	// pty starts each session in its own session/process group (Setsid), so a
	// negative PID signals the whole group and reaps Claude's children (node,
	// MCP servers, hooks) instead of orphaning them. Fall back to the bare PID
	// if the group is already gone.
	if err := syscall.Kill(-s.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return syscall.Kill(s.PID, syscall.SIGTERM)
	}
	return nil
}

func (sm *SessionManager) DismissSession(id string) {
	sm.mu.Lock()
	s, ok := sm.sessions[id]
	if ok && s.Status == StatusDead {
		delete(sm.sessions, id)
	}
	sm.mu.Unlock()
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

var urlPattern = regexp.MustCompile(`https://claude\.ai[^\s'")]*`)

const scanTailMax = 8192

// debugOutput gates per-chunk logging of session PTY output. Off by default —
// the Claude TUI redraws constantly, so logging every chunk fills the disk.
var debugOutput = os.Getenv("COUCHPILOT_DEBUG") != ""

// scanOutput drains the session's PTY, looking for the claude.ai remote-control
// URL. It must keep reading until EOF even after the URL is found, or the child
// blocks on a full PTY buffer. Raw reads (not bufio.Scanner) avoid the 64 KB
// token limit that a newline-less TUI redraw would trip.
func (sm *SessionManager) scanOutput(s *Session, r io.Reader) {
	buf := make([]byte, 4096)
	var tail []byte
	urlFound := false
	for {
		n, err := r.Read(buf)
		if n > 0 {
			clean := stripANSI(string(buf[:n]))
			if debugOutput && strings.TrimSpace(clean) != "" {
				log.Printf("[%s] %s", s.Name, strings.TrimRight(clean, "\r\n"))
			}
			if !urlFound && clean != "" {
				tail = append(tail, clean...)
				if len(tail) > scanTailMax {
					tail = tail[len(tail)-scanTailMax:]
				}
				if url := urlPattern.FindString(string(tail)); url != "" {
					urlFound = true
					tail = nil
					sm.mu.Lock()
					s.URL = url
					if s.Status == StatusStarting {
						s.Status = StatusActive
					}
					snap := *s
					sm.mu.Unlock()
					sm.persist()
					sm.hub.Broadcast(SSEEvent{Type: "session_updated", Data: &snap})
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (sm *SessionManager) waitProcess(s *Session) {
	if s.cmd == nil {
		return
	}
	err := s.cmd.Wait()
	log.Printf("[%s] process exited: %v", s.Name, err)

	sm.mu.Lock()
	if s.Status == StatusStarting {
		s.Status = StatusActive
	}
	sm.mu.Unlock()

	sm.markDead(s.ID)
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

	if isChannels && sm.onChannelsDied != nil {
		go func() {
			time.Sleep(3 * time.Second)
			sm.onChannelsDied()
		}()
	}
}

func (sm *SessionManager) recover() {
	sessions, err := sm.loadFromDisk()
	if err != nil {
		return
	}

	for _, s := range sessions {
		if s.Status == StatusDead {
			if s.DiedAt != nil && time.Since(*s.DiedAt) < time.Hour {
				sm.sessions[s.ID] = s
			}
			continue
		}

		if err := syscall.Kill(s.PID, 0); err != nil {
			now := time.Now()
			s.Status = StatusDead
			s.DiedAt = &now
			sm.sessions[s.ID] = s
			continue
		}

		s.Status = StatusActive
		sm.sessions[s.ID] = s
		go sm.pollProcess(s)
	}

	sm.persist()
}

func (sm *SessionManager) pollProcess(s *Session) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := syscall.Kill(s.PID, 0); err != nil {
			sm.markDead(s.ID)
			return
		}
	}
}

func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sm.mu.Lock()
		for id, s := range sm.sessions {
			if s.Status == StatusDead && s.DiedAt != nil && time.Since(*s.DiedAt) > 24*time.Hour {
				delete(sm.sessions, id)
			}
		}
		sm.mu.Unlock()
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

func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
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
