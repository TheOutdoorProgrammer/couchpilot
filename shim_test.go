package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReadShimStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := &shimWriter{stateDir: dir}
	w.write(ShimState{Phase: ShimActive, ClaudePID: 4242, URL: "https://claude.ai/code/x"})

	st, err := readShimState(dir)
	if err != nil {
		t.Fatalf("readShimState: %v", err)
	}
	if st.Phase != ShimActive {
		t.Errorf("phase = %q, want active", st.Phase)
	}
	if st.ClaudePID != 4242 {
		t.Errorf("claudePid = %d, want 4242", st.ClaudePID)
	}
	if st.URL != "https://claude.ai/code/x" {
		t.Errorf("url = %q", st.URL)
	}
}

func TestWriteExited(t *testing.T) {
	dir := t.TempDir()
	w := &shimWriter{stateDir: dir}
	w.writeExited(99, 1)

	st, err := readShimState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != ShimExited {
		t.Errorf("phase = %q, want exited", st.Phase)
	}
	if st.ExitCode == nil || *st.ExitCode != 1 {
		t.Errorf("exitCode = %v, want 1", st.ExitCode)
	}
}

func TestReadShimStateErrors(t *testing.T) {
	if _, err := readShimState(t.TempDir()); err == nil {
		t.Error("missing state.json should error")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, shimStateFile), []byte("{bad"), 0644)
	if _, err := readShimState(dir); err == nil {
		t.Error("corrupt state.json should error")
	}
}

func TestLegacyURLPattern(t *testing.T) {
	got := legacyURLPattern.FindString("connecting... https://claude.ai/code/abc123 ready")
	if got != "https://claude.ai/code/abc123" {
		t.Errorf("matched %q", got)
	}
	// claude.com tips links must not be mistaken for a session URL.
	if legacyURLPattern.MatchString("see https://claude.com/docs for help") {
		t.Error("claude.com should not match the session URL pattern")
	}
}

func TestActiveMarkerSquash(t *testing.T) {
	// The TUI positions the status line with absolute column moves, so after
	// ANSI stripping the words run together. The readiness check squashes
	// whitespace before matching activeMarker.
	squash := regexp.MustCompile(`\s+`)
	tail := "footer    Remote Control active   tokens"
	if !strings.Contains(squash.ReplaceAllString(tail, ""), activeMarker) {
		t.Errorf("squashed tail should contain %q", activeMarker)
	}
}
