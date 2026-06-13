package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// The shim is a tiny detached process that sits between couchpilot and each
// claude session. It — not couchpilot — owns the PTY master, so restarting
// couchpilot no longer closes the PTY and SIGHUPs every running session.
// Couchpilot observes the session through the state file the shim maintains
// and signals the shim to kill it. The shim exits when claude exits.

type ShimPhase string

const (
	ShimStarting ShimPhase = "starting"
	ShimActive   ShimPhase = "active"
	ShimExited   ShimPhase = "exited"
)

type ShimState struct {
	Phase     ShimPhase `json:"phase"`
	ClaudePID int       `json:"claudePid"`
	URL       string    `json:"url,omitempty"`
	ExitCode  *int      `json:"exitCode,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

const (
	shimStateFile   = "state.json"
	shimTailFile    = "tail.log"
	shimLogFile     = "shim.log"
	shimInputSocket = "input.sock"
	shimTailMax     = 64 * 1024
)

// activeMarker matches the Remote Control status line in the TUI footer.
// Claude Code stopped printing a claude.ai URL (≥ ~2.1), so this text is the
// only reliable readiness signal. The TUI positions status text with absolute
// column moves rather than spaces, so after ANSI stripping the words can run
// together ("RemoteControl active") — match against a whitespace-squashed
// copy of the tail.
const activeMarker = "RemoteControlactive"

var whitespacePattern = regexp.MustCompile(`\s+`)

// legacyURLPattern keeps older Claude Code versions working: when a session
// URL is printed, scrape it and treat the session as active. Deliberately
// claude.ai-only — claude.com links show up in the TUI tips box.
var legacyURLPattern = regexp.MustCompile(`https://claude\.ai[^\s'")]*`)

func readShimState(stateDir string) (*ShimState, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, shimStateFile))
	if err != nil {
		return nil, err
	}
	var st ShimState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func cmdShim() {
	fs := flag.NewFlagSet("_shim", flag.ExitOnError)
	stateDir := fs.String("state", "", "directory for state.json and tail.log")
	acceptBypass := fs.Bool("accept-bypass", false, "auto-accept the bypass-permissions dialog")
	fs.Parse(os.Args[2:])
	argv := fs.Args()
	if *stateDir == "" || len(argv) == 0 {
		log.Fatal("usage: couchpilot _shim -state <dir> [-accept-bypass] -- <claude> [args...]")
	}

	w := &shimWriter{stateDir: *stateDir}

	cmd := exec.Command(argv[0], argv[1:]...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		w.writeExited(0, -1)
		log.Fatalf("start %s: %v", argv[0], err)
	}
	defer ptmx.Close()
	claudePID := cmd.Process.Pid

	// A 0x0 winsize makes the TUI guess; give it a wide canvas so the status
	// line ("Remote Control active") renders without wrapping.
	pty.Setsize(ptmx, &pty.Winsize{Rows: 50, Cols: 200})

	w.write(ShimState{Phase: ShimStarting, ClaudePID: claudePID})
	log.Printf("shim: started %s (pid %d)", argv[0], claudePID)

	// Forward termination to claude's process group; claude's exit (not the
	// signal itself) unwinds the shim. Repeated signals escalate to SIGKILL
	// in case the session is wedged.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := syscall.SIGTERM
		for range sigCh {
			if err := syscall.Kill(-claudePID, sig); err != nil {
				syscall.Kill(claudePID, sig)
			}
			sig = syscall.SIGKILL
		}
	}()

	if *acceptBypass {
		// The --dangerously-skip-permissions dialog needs "down, enter" shortly
		// after startup. Blind-firing at 1s has been reliable in production;
		// the same keys are harmless on an idle prompt if the dialog is absent.
		go func() {
			time.Sleep(1 * time.Second)
			ptmx.Write([]byte("\x1b[B"))
			time.Sleep(200 * time.Millisecond)
			ptmx.Write([]byte("\r"))
		}()
	}

	go w.tailFlusher()
	go serveInputSocket(ptmx, *stateDir)

	// Drain the PTY until EOF — even after going active — or claude blocks on
	// a full buffer. Raw reads, not bufio: TUI redraws have no newlines.
	buf := make([]byte, 4096)
	active := false
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			clean := stripANSI(string(buf[:n]))
			tail := w.appendTail(clean)
			if !active {
				if url := legacyURLPattern.FindString(tail); url != "" {
					active = true
					w.write(ShimState{Phase: ShimActive, ClaudePID: claudePID, URL: url})
					log.Printf("shim: session active (url %s)", url)
				} else if strings.Contains(whitespacePattern.ReplaceAllString(tail, ""), activeMarker) {
					active = true
					w.write(ShimState{Phase: ShimActive, ClaudePID: claudePID})
					log.Print("shim: session active")
				}
			}
		}
		if rerr != nil {
			break
		}
	}

	err = cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	w.flushTail()
	w.writeExited(claudePID, code)
	log.Printf("shim: claude exited: code %d (%v)", code, err)
}

// shimWriter serializes state/tail writes. The tail is a rolling window of
// ANSI-stripped output kept for debugging; it is rewritten in place (never
// appended) so a chatty TUI can't grow it unbounded.
type shimWriter struct {
	stateDir string

	mu        sync.Mutex
	tail      []byte
	tailDirty bool
}

func (w *shimWriter) write(st ShimState) {
	st.UpdatedAt = time.Now()
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := filepath.Join(w.stateDir, shimStateFile+".tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("shim: write state: %v", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(w.stateDir, shimStateFile)); err != nil {
		log.Printf("shim: rename state: %v", err)
	}
}

func (w *shimWriter) writeExited(pid, code int) {
	w.write(ShimState{Phase: ShimExited, ClaudePID: pid, ExitCode: &code})
}

func (w *shimWriter) appendTail(s string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail = append(w.tail, s...)
	if len(w.tail) > shimTailMax {
		w.tail = w.tail[len(w.tail)-shimTailMax:]
	}
	w.tailDirty = true
	return string(w.tail)
}

func (w *shimWriter) tailFlusher() {
	for range time.Tick(2 * time.Second) {
		w.flushTail()
	}
}

func (w *shimWriter) flushTail() {
	w.mu.Lock()
	if !w.tailDirty {
		w.mu.Unlock()
		return
	}
	data := make([]byte, len(w.tail))
	copy(data, w.tail)
	w.tailDirty = false
	w.mu.Unlock()
	os.WriteFile(filepath.Join(w.stateDir, shimTailFile), data, 0644)
}

// serveInputSocket accepts connections on a Unix socket in the state directory
// and forwards any data received to the PTY master. This lets couchpilot send
// input (like /model commands) to the session without holding the PTY itself.
func serveInputSocket(ptmx *os.File, stateDir string) {
	sockPath := filepath.Join(stateDir, shimInputSocket)
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Printf("shim: input socket: %v", err)
		return
	}
	defer ln.Close()
	defer os.Remove(sockPath)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			io.Copy(ptmx, conn)
		}()
	}
}
