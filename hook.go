package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// `couchpilot _hook pre|post` is what claude invokes for PreToolUse and
// PostToolUse on the file tools (wired up via the per-session settings file
// couchpilot generates at spawn). It bridges the hook protocol to the local
// couchpilot API and blocks — potentially for a long time — while a pending
// review waits for a verdict in the UI.
//
// Failure stance: before a review exists we don't know whether this session
// even has review mode on, so transient API errors fail OPEN after a bounded
// retry window — a couchpilot outage must not brick file writes in every
// session. Once a review has been created we know the gate applies, so the
// wait loop retries forever and claude simply stays blocked until couchpilot
// (keepalive) comes back with the verdict.

const (
	hookSubmitRetryWindow = 60 * time.Second
	hookWaitPoll          = 50 * time.Second
)

type hookDecision struct {
	HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
}

func emitPre(decision, reason string) {
	out := map[string]string{
		"hookEventName":      "PreToolUse",
		"permissionDecision": decision,
	}
	if reason != "" {
		out["permissionDecisionReason"] = reason
	}
	json.NewEncoder(os.Stdout).Encode(hookDecision{HookSpecificOutput: out})
}

func cmdHook() {
	log.SetFlags(0)
	log.SetPrefix("couchpilot-hook: ")

	if len(os.Args) < 3 {
		log.Print("usage: couchpilot _hook pre|post")
		os.Exit(0)
	}
	mode := os.Args[2]

	sessionID := os.Getenv("COUCHPILOT_SESSION_ID")
	port := os.Getenv("COUCHPILOT_PORT")
	token := os.Getenv("COUCHPILOT_HOOK_TOKEN")
	if sessionID == "" || port == "" || token == "" {
		// Not a couchpilot-managed session (or an old spawn) — no opinion.
		os.Exit(0)
	}

	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(0)
	}

	c := &hookClient{base: "http://127.0.0.1:" + port, token: token}

	switch mode {
	case "pre":
		hookPre(c, sessionID, stdin)
	case "post":
		hookPost(c, sessionID, stdin)
	}
}

func hookPre(c *hookClient, sessionID string, stdin []byte) {
	var resp struct {
		Action   string `json:"action"`
		ReviewID string `json:"reviewId"`
	}
	deadline := time.Now().Add(hookSubmitRetryWindow)
	for {
		err := c.post("/api/hook/review", map[string]json.RawMessage{
			"sessionId": json.RawMessage(fmt.Sprintf("%q", sessionID)),
			"event":     stdin,
		}, &resp)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("couchpilot unreachable for %s; allowing write unreviewed: %v", hookSubmitRetryWindow, err)
			os.Exit(0) // no opinion — fail open
		}
		time.Sleep(2 * time.Second)
	}

	if resp.Action != "review" || resp.ReviewID == "" {
		os.Exit(0) // review mode off — no opinion, write proceeds
	}

	for {
		var wait struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		err := c.get("/api/hook/review/"+resp.ReviewID+"/wait", &wait)
		if err != nil {
			// Couchpilot is restarting; the review is persisted, keep asking.
			time.Sleep(2 * time.Second)
			continue
		}
		switch ReviewStatus(wait.Status) {
		case ReviewPending:
			continue
		case ReviewApproved:
			emitPre("allow", "")
			return
		case ReviewDenied, ReviewCancelled:
			emitPre("deny", wait.Reason)
			return
		default:
			// Review vanished (session dismissed mid-wait): don't let the write
			// through a gate we know was on.
			emitPre("deny", "Code review was cancelled because the session's review state was removed.")
			return
		}
	}
}

func hookPost(c *hookClient, sessionID string, stdin []byte) {
	var ev hookEvent
	if err := json.Unmarshal(stdin, &ev); err != nil || ev.ToolUseID == "" {
		os.Exit(0)
	}
	var resp struct {
		Context string `json:"context"`
	}
	err := c.post("/api/hook/posttool", map[string]string{
		"sessionId": sessionID,
		"toolUseId": ev.ToolUseID,
	}, &resp)
	if err != nil || resp.Context == "" {
		os.Exit(0) // best-effort: approve-with-comments context is a nice-to-have
	}
	json.NewEncoder(os.Stdout).Encode(hookDecision{HookSpecificOutput: map[string]string{
		"hookEventName":     "PostToolUse",
		"additionalContext": resp.Context,
	}})
}

type hookClient struct {
	base  string
	token string
}

func (c *hookClient) do(method, path string, body, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Couchpilot-Hook", c.token)

	// The wait endpoint holds the request up to hookWaitPoll; give it headroom.
	client := &http.Client{Timeout: hookWaitPoll + 15*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *hookClient) post(path string, body, out interface{}) error {
	return c.do(http.MethodPost, path, body, out)
}

func (c *hookClient) get(path string, out interface{}) error {
	return c.do(http.MethodGet, path, nil, out)
}
