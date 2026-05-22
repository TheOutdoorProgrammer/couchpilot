package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Dir       string        `json:"dir"`
	PID       int           `json:"pid"`
	URL            string        `json:"url"`
	Status         SessionStatus `json:"status"`
	PermissionMode string        `json:"permissionMode"`
	CreatedAt time.Time     `json:"createdAt"`
	DiedAt    *time.Time    `json:"diedAt,omitempty"`

	cmd *exec.Cmd `json:"-"`
}

type SessionManager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	hub      *SSEHub
	dataDir  string
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

func (sm *SessionManager) CreateSession(name, dir, permMode, model, effort string) (*Session, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	} else if dir == "~" {
		dir, _ = os.UserHomeDir()
	}

	if name == "" {
		name = generateName()
	}

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
		CreatedAt:      time.Now(),
		cmd:            cmd,
	}

	sm.mu.Lock()
	sm.sessions[s.ID] = s
	sm.mu.Unlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_created", Data: s})

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
	return syscall.Kill(s.PID, syscall.SIGTERM)
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

var urlPattern = regexp.MustCompile(`https://claude\.ai\S*`)

func (sm *SessionManager) scanOutput(s *Session, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := stripANSI(scanner.Text())
		if line == "" {
			continue
		}

		log.Printf("[%s] %s", s.Name, line)
		if url := urlPattern.FindString(line); url != "" {
			sm.mu.Lock()
			s.URL = url
			if s.Status == StatusStarting {
				s.Status = StatusActive
			}
			sm.mu.Unlock()
			sm.persist()
			sm.hub.Broadcast(SSEEvent{Type: "session_updated", Data: s})
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
	sm.mu.Unlock()
	sm.persist()
	sm.hub.Broadcast(SSEEvent{Type: "session_died", Data: s})
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
	rand.Read(b)
	return hex.EncodeToString(b)
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07`)

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
	ai, _ := rand.Int(rand.Reader, big.NewInt(int64(len(adjectives))))
	ni, _ := rand.Int(rand.Reader, big.NewInt(int64(len(nouns))))
	return fmt.Sprintf("%s-%s", adjectives[ai.Int64()], nouns[ni.Int64()])
}
