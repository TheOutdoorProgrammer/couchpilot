package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ClaudeSession struct {
	ID             string    `json:"id"`
	Title          string    `json:"title,omitempty"`
	FirstPrompt    string    `json:"firstPrompt,omitempty"`
	Cwd            string    `json:"cwd"`
	GitBranch      string    `json:"gitBranch,omitempty"`
	PermissionMode string    `json:"permissionMode,omitempty"`
	ModifiedAt     time.Time `json:"modifiedAt"`
	SizeBytes      int64     `json:"sizeBytes"`
	MessageCount   int       `json:"messageCount"`
}

func ClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ListClaudeSessions walks ~/.claude/projects/* and returns recent sessions
// sorted by last-modified, most recent first. Limit <= 0 means no limit.
func ListClaudeSessions(limit int) []ClaudeSession {
	root := ClaudeProjectsDir()
	if root == "" {
		return nil
	}

	projects, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	type pendingFile struct {
		path string
		info os.FileInfo
	}
	var files []pendingFile
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, p.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			full := filepath.Join(root, p.Name(), e.Name())
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			files = append(files, pendingFile{path: full, info: info})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].info.ModTime().After(files[j].info.ModTime())
	})
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}

	out := make([]ClaudeSession, 0, len(files))
	for _, f := range files {
		s, ok := parseClaudeSession(f.path, f.info)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
}

// parseClaudeSession scans a JSONL session file and pulls out metadata.
// It stops as soon as it has filled the interesting fields.
func parseClaudeSession(path string, info os.FileInfo) (ClaudeSession, bool) {
	base := filepath.Base(path)
	id := strings.TrimSuffix(base, ".jsonl")

	s := ClaudeSession{
		ID:         id,
		ModifiedAt: info.ModTime(),
		SizeBytes:  info.Size(),
	}

	f, err := os.Open(path)
	if err != nil {
		return s, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Some sessions have very long lines (large tool results); give the scanner room.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var firstPromptText string

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Type           string          `json:"type"`
			SessionID      string          `json:"sessionId"`
			Cwd            string          `json:"cwd"`
			GitBranch      string          `json:"gitBranch"`
			AiTitle        string          `json:"aiTitle"`
			PermissionMode string          `json:"permissionMode"`
			Message        json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		s.MessageCount++

		if s.ID == "" && rec.SessionID != "" {
			s.ID = rec.SessionID
		}
		if s.Cwd == "" && rec.Cwd != "" {
			s.Cwd = rec.Cwd
		}
		if s.GitBranch == "" && rec.GitBranch != "" {
			s.GitBranch = rec.GitBranch
		}
		if s.Title == "" && rec.AiTitle != "" {
			s.Title = rec.AiTitle
		}
		if s.PermissionMode == "" && rec.PermissionMode != "" && rec.Type == "permission-mode" {
			s.PermissionMode = rec.PermissionMode
		}
		if firstPromptText == "" && rec.Type == "user" && len(rec.Message) > 0 {
			firstPromptText = extractUserPrompt(rec.Message)
		}
	}

	if s.Cwd == "" {
		// Fall back to decoded directory name (lossy but better than nothing).
		s.Cwd = decodeProjectDir(filepath.Base(filepath.Dir(path)))
	}
	if firstPromptText != "" {
		s.FirstPrompt = truncate(firstPromptText, 200)
	}
	return s, s.ID != ""
}

// extractUserPrompt pulls the first chunk of plain text out of a user message
// payload. Skips meta-content blobs Claude Code wraps around real prompts.
func extractUserPrompt(raw json.RawMessage) string {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	// Content can be a string or an array of blocks.
	var asString string
	if err := json.Unmarshal(msg.Content, &asString); err == nil {
		return cleanPrompt(asString)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				if t := cleanPrompt(b.Text); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func cleanPrompt(s string) string {
	s = strings.TrimSpace(s)
	// Drop bracketed tool result echoes like "[Tool Result (tool_use_id=...)]"
	if strings.HasPrefix(s, "<") || strings.HasPrefix(s, "[Tool ") || strings.HasPrefix(s, "Caveat:") {
		return ""
	}
	// Replace newlines with spaces for a compact preview.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(s[:n], " ") + "…"
}

// decodeProjectDir reverses the convention Claude Code uses to name project
// directories (path with "/" replaced by "-"). This is lossy when real
// directory names contain "-", so we only use it as a last-resort fallback.
func decodeProjectDir(name string) string {
	if !strings.HasPrefix(name, "-") {
		return name
	}
	return strings.ReplaceAll(name, "-", "/")
}
