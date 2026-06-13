package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// containsSeq reports whether want appears as a contiguous subsequence of got.
func containsSeq(got, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(got); i++ {
		if equalSlice(got[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildClaudeArgsFresh(t *testing.T) {
	s := &Session{Name: "swift-falcon", SessionUUID: "uuid-123"}
	args := buildClaudeArgs(s, false)

	want := []string{"--remote-control", "swift-falcon", "--session-id", "uuid-123"}
	if !equalSlice(args, want) {
		t.Errorf("got %v, want %v", args, want)
	}
}

func TestBuildClaudeArgsResume(t *testing.T) {
	s := &Session{Name: "n", SessionUUID: "uuid-123"}
	args := buildClaudeArgs(s, true)

	if !containsSeq(args, []string{"--resume", "uuid-123"}) {
		t.Errorf("resume should pass --resume <uuid>: %v", args)
	}
	if containsSeq(args, []string{"--session-id"}) {
		t.Errorf("resume must not pass --session-id: %v", args)
	}
}

func TestBuildClaudeArgsModelAndEffort(t *testing.T) {
	s := &Session{Name: "n", SessionUUID: "u", Model: "claude-opus-4-6", Effort: "high"}
	args := buildClaudeArgs(s, false)

	if !containsSeq(args, []string{"--model", "claude-opus-4-6"}) {
		t.Errorf("missing --model: %v", args)
	}
	if !containsSeq(args, []string{"--effort", "high"}) {
		t.Errorf("missing --effort: %v", args)
	}
}

func TestBuildClaudeArgsOmitsEmptyOptionals(t *testing.T) {
	s := &Session{Name: "n", SessionUUID: "u"}
	args := buildClaudeArgs(s, false)
	for _, flag := range []string{"--model", "--effort", "--channels", "--plugin-dir", "--permission-mode"} {
		if containsSeq(args, []string{flag}) {
			t.Errorf("unset optional %s should be absent: %v", flag, args)
		}
	}
}

func TestBuildClaudeArgsPermissionModes(t *testing.T) {
	t.Run("bypass", func(t *testing.T) {
		s := &Session{Name: "n", SessionUUID: "u", PermissionMode: "bypassPermissions"}
		args := buildClaudeArgs(s, false)
		if !containsSeq(args, []string{"--dangerously-skip-permissions"}) {
			t.Errorf("bypass should pass --dangerously-skip-permissions: %v", args)
		}
		if containsSeq(args, []string{"--permission-mode"}) {
			t.Errorf("bypass must not also pass --permission-mode: %v", args)
		}
	})
	t.Run("plan", func(t *testing.T) {
		s := &Session{Name: "n", SessionUUID: "u", PermissionMode: "plan"}
		args := buildClaudeArgs(s, false)
		if !containsSeq(args, []string{"--permission-mode", "plan"}) {
			t.Errorf("plan should pass --permission-mode plan: %v", args)
		}
	})
	t.Run("default is implicit", func(t *testing.T) {
		s := &Session{Name: "n", SessionUUID: "u", PermissionMode: "default"}
		args := buildClaudeArgs(s, false)
		if containsSeq(args, []string{"--permission-mode"}) {
			t.Errorf("default mode should not pass --permission-mode: %v", args)
		}
	})
}

func TestBuildClaudeArgsChannelsAndPlugins(t *testing.T) {
	s := &Session{
		Name:        "n",
		SessionUUID: "u",
		Channels:    "plugin:imessage@fork",
		PluginDirs:  []string{"/a", "/b"},
	}
	args := buildClaudeArgs(s, false)
	if !containsSeq(args, []string{"--channels", "plugin:imessage@fork"}) {
		t.Errorf("missing --channels: %v", args)
	}
	if !containsSeq(args, []string{"--plugin-dir", "/a"}) || !containsSeq(args, []string{"--plugin-dir", "/b"}) {
		t.Errorf("missing --plugin-dir entries: %v", args)
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[2J\x1b[Hclear", "clear"},
		{"a\x1b[?25hb", "ab"},                // private cursor mode
		{"\x1b]0;title\x07body", "body"},     // OSC string
		{"tab\tnewline\n", "tab\tnewline\n"}, // tab/newline are kept
		{"\x00\x01ctrl", "ctrl"},             // stray control bytes stripped
	}
	for _, c := range cases {
		if got := stripANSI(c.in); got != c.want {
			t.Errorf("stripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGenerateUUIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		u := generateUUID()
		if !re.MatchString(u) {
			t.Fatalf("not a v4 uuid: %q", u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid: %q", u)
		}
		seen[u] = true
	}
}

func TestGenerateIDUnique(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := generateID()
		if !re.MatchString(id) {
			t.Fatalf("unexpected id shape: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id: %q", id)
		}
		seen[id] = true
	}
}

func TestGenerateName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+-[a-z]+$`)
	for i := 0; i < 50; i++ {
		if n := generateName(); !re.MatchString(n) {
			t.Fatalf("unexpected name shape: %q", n)
		}
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("own pid should be alive")
	}
	if pidAlive(0) {
		t.Error("pid 0 must be treated as not-a-process")
	}
	if pidAlive(-5) {
		t.Error("negative pid must be treated as not-a-process")
	}
}

func TestBridgeURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	uuid := "abc-123"
	projDir := filepath.Join(home, ".claude", "projects", "some-proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Two bridge registrations; the last must win.
	content := `{"type":"bridge-session","bridgeSessionId":"cse_OLD"}
{"type":"message"}
{"type":"bridge-session","bridgeSessionId":"cse_NEW123"}
`
	if err := os.WriteFile(filepath.Join(projDir, uuid+".jsonl"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if got := bridgeURL(uuid); got != "https://claude.ai/code/session_NEW123" {
		t.Errorf("bridgeURL = %q, want last bridge id", got)
	}
}

func TestBridgeURLNoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := bridgeURL("does-not-exist"); got != "" {
		t.Errorf("bridgeURL for missing session = %q, want empty", got)
	}
}

func TestChangeModelGuards(t *testing.T) {
	sm := &SessionManager{sessions: map[string]*Session{}, hub: NewSSEHub(), dataDir: t.TempDir()}
	sm.sessions["dead"] = &Session{ID: "dead", Status: StatusDead, SessionUUID: "u"}
	sm.sessions["nouuid"] = &Session{ID: "nouuid", Status: StatusActive}

	if err := sm.ChangeModel("missing", "sonnet"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing session: got %v, want ErrNotExist", err)
	}
	if err := sm.ChangeModel("dead", "sonnet"); err == nil || !strings.Contains(err.Error(), "dead") {
		t.Errorf("dead session: got %v, want dead error", err)
	}
	if err := sm.ChangeModel("nouuid", "sonnet"); err == nil || !strings.Contains(err.Error(), "conversation id") {
		t.Errorf("no-uuid session: got %v, want conversation id error", err)
	}
}

func TestSendInputRoundTrip(t *testing.T) {
	// Unix socket paths are capped at ~104 bytes on macOS, so the deep
	// t.TempDir() path under /var/folders overflows sun_path. Use a short
	// /tmp dir instead — production paths under ~/.config are short too.
	dir, err := os.MkdirTemp("/tmp", "cpsi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sm := &SessionManager{sessions: map[string]*Session{}, hub: NewSSEHub(), dataDir: dir}

	id := "sess1"
	stateDir := sm.stateDir(id)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(stateDir, shimInputSocket)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		got <- string(buf[:n])
	}()

	if err := sm.SendInput(id, "/model sonnet\r"); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if received := <-got; received != "/model sonnet\r" {
		t.Errorf("shim received %q, want %q", received, "/model sonnet\r")
	}
}

func TestSendInputNoSocket(t *testing.T) {
	sm := &SessionManager{sessions: map[string]*Session{}, hub: NewSSEHub(), dataDir: t.TempDir()}
	if err := sm.SendInput("ghost", "hi"); err == nil {
		t.Error("SendInput to a session with no socket should error, not hang")
	}
}
