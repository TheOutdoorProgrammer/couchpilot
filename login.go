package main

import (
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const loginBufferMax = 64 * 1024

type LoginState struct {
	Running   bool       `json:"running"`
	Output    string     `json:"output"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
}

type LoginManager struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	pty       *os.File
	hub       *SSEHub
	running   bool
	startedAt *time.Time
	exitCode  *int
	buffer    []byte
	readDone  chan struct{}
}

func NewLoginManager(hub *SSEHub) *LoginManager {
	return &LoginManager{hub: hub}
}

type LoginOptions struct {
	Method string `json:"method"` // "claudeai" (default), "console", "sso"
	Email  string `json:"email"`
}

func (lm *LoginManager) Start(opts LoginOptions) error {
	lm.mu.Lock()
	if lm.running {
		lm.mu.Unlock()
		return errors.New("login already running")
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		lm.mu.Unlock()
		return err
	}

	args := []string{"auth", "login"}
	switch opts.Method {
	case "console":
		args = append(args, "--console")
	case "sso":
		args = append(args, "--sso")
	case "", "claudeai":
		args = append(args, "--claudeai")
	default:
		lm.mu.Unlock()
		return errors.New("unknown method: " + opts.Method)
	}
	if opts.Email != "" {
		args = append(args, "--email", opts.Email)
	}

	cmd := exec.Command(claudePath, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		lm.mu.Unlock()
		return err
	}

	now := time.Now()
	lm.cmd = cmd
	lm.pty = ptmx
	lm.running = true
	lm.startedAt = &now
	lm.exitCode = nil
	lm.buffer = nil
	lm.readDone = make(chan struct{})
	state := lm.snapshotLocked()
	readDone := lm.readDone
	lm.mu.Unlock()

	lm.hub.Broadcast(SSEEvent{Type: "login_started", Data: state})

	go lm.readLoop(ptmx, readDone)
	go lm.waitProc(cmd, ptmx, readDone)

	return nil
}

func (lm *LoginManager) readLoop(ptmx *os.File, done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 2048)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			lm.mu.Lock()
			lm.buffer = append(lm.buffer, chunk...)
			if len(lm.buffer) > loginBufferMax {
				lm.buffer = lm.buffer[len(lm.buffer)-loginBufferMax:]
			}
			lm.mu.Unlock()

			lm.hub.Broadcast(SSEEvent{
				Type: "login_output",
				Data: map[string]string{"data": string(chunk)},
			})
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("login read: %v", err)
			}
			return
		}
	}
}

func (lm *LoginManager) waitProc(cmd *exec.Cmd, ptmx *os.File, readDone <-chan struct{}) {
	err := cmd.Wait()
	ptmx.Close()
	<-readDone

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	log.Printf("claude login exited (code=%d, err=%v)", code, err)

	lm.mu.Lock()
	lm.running = false
	lm.exitCode = &code
	state := lm.snapshotLocked()
	lm.mu.Unlock()

	lm.hub.Broadcast(SSEEvent{Type: "login_ended", Data: state})
}

func (lm *LoginManager) SendInput(data string) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if !lm.running || lm.pty == nil {
		return errors.New("login not running")
	}
	_, err := lm.pty.Write([]byte(data))
	return err
}

func (lm *LoginManager) Stop() error {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if !lm.running || lm.cmd == nil || lm.cmd.Process == nil {
		return nil
	}
	return syscall.Kill(lm.cmd.Process.Pid, syscall.SIGTERM)
}

func (lm *LoginManager) State() LoginState {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.snapshotLocked()
}

func (lm *LoginManager) snapshotLocked() LoginState {
	return LoginState{
		Running:   lm.running,
		Output:    string(lm.buffer),
		StartedAt: lm.startedAt,
		ExitCode:  lm.exitCode,
	}
}
