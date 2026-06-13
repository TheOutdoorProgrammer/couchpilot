package main

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// Code review gate: when a session has review mode on, every Write/Edit the
// session attempts is intercepted by a PreToolUse hook (`couchpilot _hook`),
// turned into a pending Review here, and blocked until Joey approves or denies
// it from the UI. A denial carries the review comments back to claude as the
// tool-call failure reason, so it can iterate; an approval lets the original
// write proceed untouched. This reviews the file tools only — writes made via
// Bash (heredocs, sed) never hit the hook. It's a review workflow, not a
// security boundary.

type ReviewStatus string

const (
	ReviewPending   ReviewStatus = "pending"
	ReviewApproved  ReviewStatus = "approved"
	ReviewDenied    ReviewStatus = "denied"
	ReviewCancelled ReviewStatus = "cancelled"
)

// reviewRetention bounds per-session history; decided reviews beyond the cap
// are dropped oldest-first. Pending reviews are never trimmed.
const reviewRetention = 30

// reviewMaxBytes guards the diff engine against huge files: past this size we
// keep the review (it can still be approved/denied) but skip diff rendering.
const reviewMaxBytes = 2 << 20

type ReviewComment struct {
	ID        string    `json:"id"`
	Line      int       `json:"line,omitempty"` // 0 = global comment
	Side      string    `json:"side,omitempty"` // "new" or "old"; "" for global
	LineText  string    `json:"lineText,omitempty"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

type Review struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	Seq       int             `json:"seq"`
	ToolName  string          `json:"toolName"`
	ToolUseID string          `json:"toolUseId"`
	FilePath  string          `json:"filePath"`
	Status    ReviewStatus    `json:"status"`
	Comments  []ReviewComment `json:"comments"`
	CreatedAt time.Time       `json:"createdAt"`
	DecidedAt *time.Time      `json:"decidedAt,omitempty"`

	NewFile  bool `json:"newFile,omitempty"`  // file does not exist on disk yet
	NoMatch  bool `json:"noMatch,omitempty"`  // Edit old_string not found — claude's edit will fail
	TooLarge bool `json:"tooLarge,omitempty"` // diff suppressed past reviewMaxBytes
	Notebook bool `json:"notebook,omitempty"` // NotebookEdit: proposed shows the new cell source only

	// Base is the on-disk content at hook time; Proposed is the content the
	// tool call would produce. Persisted so pending reviews survive restarts;
	// never serialized to the API (the UI gets structured hunks instead).
	Base     string `json:"base"`
	Proposed string `json:"proposed"`

	// PostContext carries approve-with-comments feedback until the PostToolUse
	// hook for the same tool_use_id collects it.
	PostContext string `json:"postContext,omitempty"`
}

// DiffRow is one rendered line of the review diff. T is "ctx", "add" or "del";
// O and N are 1-based old/new line numbers (0 when the side doesn't exist).
type DiffRow struct {
	T    string `json:"t"`
	O    int    `json:"o,omitempty"`
	N    int    `json:"n,omitempty"`
	Text string `json:"text"`
}

type DiffHunk struct {
	Rows []DiffRow `json:"rows"`
}

// reviewView is the API shape: review metadata without file contents, plus
// hunks when requested.
type reviewView struct {
	*Review
	Base     omitted    `json:"base,omitempty"`
	Proposed omitted    `json:"proposed,omitempty"`
	Hunks    []DiffHunk `json:"hunks,omitempty"`
	Adds     int        `json:"adds"`
	Dels     int        `json:"dels"`
}

// omitted shadows a field out of the embedded struct's JSON.
type omitted *struct{}

type ReviewManager struct {
	mu       sync.Mutex
	dataDir  string
	hub      *SSEHub
	token    string
	byID     map[string]*Review
	order    map[string][]*Review // per session, oldest first
	nextSeq  map[string]int
	waiters  map[string][]chan struct{}
	reviewOn func(sessionID string) bool // SessionManager lookup, set by main
}

func NewReviewManager(dataDir string, hub *SSEHub) (*ReviewManager, error) {
	rm := &ReviewManager{
		dataDir: dataDir,
		hub:     hub,
		byID:    make(map[string]*Review),
		order:   make(map[string][]*Review),
		nextSeq: make(map[string]int),
		waiters: make(map[string][]chan struct{}),
	}
	if err := rm.loadToken(); err != nil {
		return nil, err
	}
	rm.loadAll()
	return rm, nil
}

// loadToken loads or creates the shared secret that authenticates hook
// processes to the local API. Hook endpoints bypass password auth (the hook
// has no cookie), so they get their own credential instead, passed to claude
// via the environment at spawn.
func (rm *ReviewManager) loadToken() error {
	path := filepath.Join(rm.dataDir, "hook-secret")
	data, err := os.ReadFile(path)
	if err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		rm.token = strings.TrimSpace(string(data))
		return nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	rm.token = hex.EncodeToString(b)
	os.MkdirAll(rm.dataDir, 0755)
	return os.WriteFile(path, []byte(rm.token), 0600)
}

func (rm *ReviewManager) HookToken() string { return rm.token }

func (rm *ReviewManager) CheckToken(tok string) bool {
	return tok != "" && hmac.Equal([]byte(tok), []byte(rm.token))
}

func (rm *ReviewManager) reviewsFile(sessionID string) string {
	return filepath.Join(rm.dataDir, "sessions", sessionID, "reviews.json")
}

// loadAll picks persisted reviews back up after a restart. Hooks that were
// blocked on a pending review reconnect on their own and find it via Status.
func (rm *ReviewManager) loadAll() {
	dirs, err := os.ReadDir(filepath.Join(rm.dataDir, "sessions"))
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		data, err := os.ReadFile(rm.reviewsFile(d.Name()))
		if err != nil {
			continue
		}
		var reviews []*Review
		if err := json.Unmarshal(data, &reviews); err != nil {
			log.Printf("reviews: %s corrupt (%v); skipping", d.Name(), err)
			continue
		}
		for _, r := range reviews {
			rm.byID[r.ID] = r
			rm.order[r.SessionID] = append(rm.order[r.SessionID], r)
			if r.Seq >= rm.nextSeq[r.SessionID] {
				rm.nextSeq[r.SessionID] = r.Seq + 1
			}
		}
	}
}

// persistLocked writes one session's reviews. Caller holds rm.mu.
func (rm *ReviewManager) persistLocked(sessionID string) {
	list := rm.order[sessionID]
	if len(list) == 0 {
		os.Remove(rm.reviewsFile(sessionID))
		return
	}
	data, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		log.Printf("reviews: persist %s: %v", sessionID, err)
		return
	}
	dir := filepath.Dir(rm.reviewsFile(sessionID))
	os.MkdirAll(dir, 0755)
	tmp := rm.reviewsFile(sessionID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("reviews: persist %s: %v", sessionID, err)
		return
	}
	os.Rename(tmp, rm.reviewsFile(sessionID))
}

// trimLocked drops the oldest decided reviews past the retention cap.
func (rm *ReviewManager) trimLocked(sessionID string) {
	list := rm.order[sessionID]
	decided := 0
	for _, r := range list {
		if r.Status != ReviewPending {
			decided++
		}
	}
	if decided <= reviewRetention {
		return
	}
	keep := list[:0]
	for _, r := range list {
		if r.Status != ReviewPending && decided > reviewRetention {
			decided--
			delete(rm.byID, r.ID)
			continue
		}
		keep = append(keep, r)
	}
	rm.order[sessionID] = keep
}

// hookEvent is the subset of the PreToolUse/PostToolUse stdin payload the
// review gate needs.
type hookEvent struct {
	SessionUUID string          `json:"session_id"`
	ToolName    string          `json:"tool_name"`
	ToolUseID   string          `json:"tool_use_id"`
	ToolInput   json.RawMessage `json:"tool_input"`
}

// Submit turns a PreToolUse event into a pending review, or short-circuits
// when the session isn't in review mode. Returns the pending review (nil when
// the write should just proceed).
func (rm *ReviewManager) Submit(sessionID string, ev hookEvent) (*Review, error) {
	if rm.reviewOn == nil || !rm.reviewOn(sessionID) {
		return nil, nil
	}

	filePath, base, proposed, flags, err := buildProposal(ev)
	if err != nil {
		return nil, err
	}

	rm.mu.Lock()
	seq := rm.nextSeq[sessionID]
	rm.nextSeq[sessionID] = seq + 1
	r := &Review{
		ID:        generateID(),
		SessionID: sessionID,
		Seq:       seq,
		ToolName:  ev.ToolName,
		ToolUseID: ev.ToolUseID,
		FilePath:  filePath,
		Status:    ReviewPending,
		CreatedAt: time.Now(),
		NewFile:   flags.newFile,
		NoMatch:   flags.noMatch,
		TooLarge:  flags.tooLarge,
		Notebook:  flags.notebook,
		Base:      base,
		Proposed:  proposed,
	}
	rm.byID[r.ID] = r
	rm.order[sessionID] = append(rm.order[sessionID], r)
	rm.trimLocked(sessionID)
	rm.persistLocked(sessionID)
	view := rm.viewLocked(r, false)
	rm.mu.Unlock()

	rm.hub.Broadcast(SSEEvent{Type: "review_created", Data: view})
	return r, nil
}

type proposalFlags struct {
	newFile, noMatch, tooLarge, notebook bool
}

// buildProposal reads the target file and computes the content the tool call
// would leave behind. The write is blocked while the review is pending, so
// disk at hook time is the authoritative "before".
func buildProposal(ev hookEvent) (filePath, base, proposed string, flags proposalFlags, err error) {
	var input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Content      string `json:"content"`
		OldString    string `json:"old_string"`
		NewString    string `json:"new_string"`
		ReplaceAll   bool   `json:"replace_all"`
		NewSource    string `json:"new_source"`
		Edits        []struct {
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		} `json:"edits"`
	}
	if err = json.Unmarshal(ev.ToolInput, &input); err != nil {
		return "", "", "", flags, fmt.Errorf("tool_input: %w", err)
	}

	filePath = input.FilePath
	if filePath == "" {
		filePath = input.NotebookPath
	}
	if filePath == "" {
		return "", "", "", flags, fmt.Errorf("tool_input has no file path")
	}

	if ev.ToolName == "NotebookEdit" {
		// Notebook cells don't map to a plain-text before/after; review the new
		// cell source on its own.
		flags.notebook = true
		return filePath, "", input.NewSource, flags, nil
	}

	raw, rerr := os.ReadFile(filePath)
	if rerr != nil {
		flags.newFile = true
	} else {
		base = string(raw)
	}

	switch ev.ToolName {
	case "Write":
		proposed = input.Content
	case "Edit":
		proposed, flags.noMatch = applyEdit(base, input.OldString, input.NewString, input.ReplaceAll)
	case "MultiEdit":
		proposed = base
		for _, e := range input.Edits {
			next, miss := applyEdit(proposed, e.OldString, e.NewString, e.ReplaceAll)
			if miss {
				flags.noMatch = true
			}
			proposed = next
		}
	default:
		return "", "", "", flags, fmt.Errorf("unsupported tool %q", ev.ToolName)
	}

	if len(base) > reviewMaxBytes || len(proposed) > reviewMaxBytes {
		flags.tooLarge = true
		base, proposed = "", ""
	}
	return filePath, base, proposed, flags, nil
}

// applyEdit mirrors the Edit tool's replacement semantics closely enough for
// review purposes. A missing old_string is reported, not fatal: the review
// still renders (empty diff) with a warning that claude's edit will fail.
func applyEdit(base, oldStr, newStr string, replaceAll bool) (string, bool) {
	if oldStr == "" || !strings.Contains(base, oldStr) {
		return base, true
	}
	if replaceAll {
		return strings.ReplaceAll(base, oldStr, newStr), false
	}
	return strings.Replace(base, oldStr, newStr, 1), false
}

// Wait blocks until the review is decided or the timeout passes, and returns
// its current status. Hooks poll this in a loop, so couchpilot restarts (which
// drop the in-memory waiter) just look like a timed-out poll.
func (rm *ReviewManager) Wait(id string, timeout time.Duration) (ReviewStatus, string, bool) {
	rm.mu.Lock()
	r, ok := rm.byID[id]
	if !ok {
		rm.mu.Unlock()
		return "", "", false
	}
	if r.Status != ReviewPending {
		status, reason := r.Status, rm.denyReasonLocked(r)
		rm.mu.Unlock()
		return status, reason, true
	}
	ch := make(chan struct{})
	rm.waiters[id] = append(rm.waiters[id], ch)
	rm.mu.Unlock()

	select {
	case <-ch:
	case <-time.After(timeout):
	}

	rm.mu.Lock()
	defer rm.mu.Unlock()
	if r, ok := rm.byID[id]; ok {
		return r.Status, rm.denyReasonLocked(r), true
	}
	return ReviewCancelled, "", true
}

func (rm *ReviewManager) wakeLocked(id string) {
	for _, ch := range rm.waiters[id] {
		close(ch)
	}
	delete(rm.waiters, id)
}

// AddComment attaches a line or global comment to a pending review.
func (rm *ReviewManager) AddComment(id string, c ReviewComment) (*ReviewComment, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	r, ok := rm.byID[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	if r.Status != ReviewPending {
		return nil, fmt.Errorf("review already %s", r.Status)
	}
	if strings.TrimSpace(c.Text) == "" {
		return nil, fmt.Errorf("comment text is empty")
	}
	c.ID = generateID()
	c.CreatedAt = time.Now()
	r.Comments = append(r.Comments, c)
	rm.persistLocked(r.SessionID)
	rm.broadcastLocked(r)
	return &c, nil
}

func (rm *ReviewManager) DeleteComment(id, commentID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	r, ok := rm.byID[id]
	if !ok {
		return os.ErrNotExist
	}
	for i, c := range r.Comments {
		if c.ID == commentID {
			r.Comments = append(r.Comments[:i], r.Comments[i+1:]...)
			rm.persistLocked(r.SessionID)
			rm.broadcastLocked(r)
			return nil
		}
	}
	return os.ErrNotExist
}

// Decide resolves a pending review. Denials require at least one comment —
// otherwise claude has nothing to iterate on. Approvals with comments stash
// the feedback for the PostToolUse hook to deliver after the write lands.
func (rm *ReviewManager) Decide(id, action string) (*Review, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	r, ok := rm.byID[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	if r.Status != ReviewPending {
		return nil, fmt.Errorf("review already %s", r.Status)
	}
	switch action {
	case "approve":
		r.Status = ReviewApproved
		if len(r.Comments) > 0 {
			r.PostContext = approveContext(r)
		}
	case "deny":
		if len(r.Comments) == 0 {
			return nil, fmt.Errorf("add at least one comment so claude knows what to change")
		}
		r.Status = ReviewDenied
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
	now := time.Now()
	r.DecidedAt = &now
	rm.persistLocked(r.SessionID)
	rm.wakeLocked(id)
	rm.broadcastLocked(r)
	return r, nil
}

// TakePostContext hands approve-with-comments feedback to the PostToolUse
// hook, exactly once, matched by the tool_use_id of the approved call.
func (rm *ReviewManager) TakePostContext(sessionID, toolUseID string) string {
	if toolUseID == "" {
		return ""
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for _, r := range rm.order[sessionID] {
		if r.ToolUseID == toolUseID && r.PostContext != "" {
			ctx := r.PostContext
			r.PostContext = ""
			rm.persistLocked(sessionID)
			return ctx
		}
	}
	return ""
}

// CancelForSession resolves all pending reviews when their session dies. The
// blocked hook (if it somehow outlives claude) gets a denial, and the UI
// stops showing actionable reviews for a dead session.
func (rm *ReviewManager) CancelForSession(sessionID string) {
	rm.mu.Lock()
	var cancelled []*Review
	for _, r := range rm.order[sessionID] {
		if r.Status == ReviewPending {
			r.Status = ReviewCancelled
			now := time.Now()
			r.DecidedAt = &now
			rm.wakeLocked(r.ID)
			cancelled = append(cancelled, r)
		}
	}
	if len(cancelled) > 0 {
		rm.persistLocked(sessionID)
	}
	views := make([]*reviewView, len(cancelled))
	for i, r := range cancelled {
		views[i] = rm.viewLocked(r, false)
	}
	rm.mu.Unlock()
	for _, v := range views {
		rm.hub.Broadcast(SSEEvent{Type: "review_decided", Data: v})
	}
}

// DropSession removes a dismissed session's reviews from memory; the on-disk
// file goes away with the session state dir.
func (rm *ReviewManager) DropSession(sessionID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for _, r := range rm.order[sessionID] {
		rm.wakeLocked(r.ID)
		delete(rm.byID, r.ID)
	}
	delete(rm.order, sessionID)
	delete(rm.nextSeq, sessionID)
}

func (rm *ReviewManager) Get(id string, withDiff bool) *reviewView {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	r, ok := rm.byID[id]
	if !ok {
		return nil
	}
	return rm.viewLocked(r, withDiff)
}

// List returns one session's reviews (newest first), or all sessions' when
// sessionID is empty. Optionally filtered to pending.
func (rm *ReviewManager) List(sessionID string, pendingOnly bool) []*reviewView {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	var out []*reviewView
	collect := func(list []*Review) {
		for _, r := range list {
			if pendingOnly && r.Status != ReviewPending {
				continue
			}
			out = append(out, rm.viewLocked(r, false))
		}
	}
	if sessionID != "" {
		collect(rm.order[sessionID])
	} else {
		for _, list := range rm.order {
			collect(list)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (rm *ReviewManager) broadcastLocked(r *Review) {
	event := "review_updated"
	if r.Status != ReviewPending {
		event = "review_decided"
	}
	view := rm.viewLocked(r, false)
	go rm.hub.Broadcast(SSEEvent{Type: event, Data: view})
}

func (rm *ReviewManager) viewLocked(r *Review, withDiff bool) *reviewView {
	v := &reviewView{Review: r}
	v.Adds, v.Dels = diffStats(r.Base, r.Proposed)
	if withDiff {
		v.Hunks = diffHunks(r.Base, r.Proposed)
	}
	return v
}

// denyReasonLocked formats the feedback claude receives when a review is
// denied. It quotes each commented line so claude can locate the complaint
// even after its own line numbering shifts.
func (rm *ReviewManager) denyReasonLocked(r *Review) string {
	if r.Status == ReviewCancelled {
		return "Code review was cancelled because the session ended. Do not retry the change."
	}
	if r.Status != ReviewDenied {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CODE REVIEW — CHANGES REQUESTED on %s\n", r.FilePath)
	b.WriteString("The reviewer rejected this change. Address every comment, then make the corrected change.\n")
	writeComments(&b, r)
	b.WriteString("\nDo not apply the change as proposed. Revise it per the comments above; the corrected attempt will be reviewed again.")
	return b.String()
}

// approveContext formats approve-with-comments feedback: the write went
// through, but the reviewer had notes for the follow-up work.
func approveContext(r *Review) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CODE REVIEW — APPROVED WITH COMMENTS on %s\n", r.FilePath)
	b.WriteString("The change was applied as-is, but the reviewer left feedback to incorporate going forward:\n")
	writeComments(&b, r)
	return b.String()
}

func writeComments(b *strings.Builder, r *Review) {
	for _, c := range r.Comments {
		if c.Line == 0 {
			fmt.Fprintf(b, "\nOverall: %s\n", c.Text)
		}
	}
	for _, c := range r.Comments {
		if c.Line == 0 {
			continue
		}
		side := "new"
		if c.Side == "old" {
			side = "old/removed"
		}
		if c.LineText != "" {
			fmt.Fprintf(b, "\nLine %d (%s): `%s`\n  -> %s\n", c.Line, side, strings.TrimSpace(c.LineText), c.Text)
		} else {
			fmt.Fprintf(b, "\nLine %d (%s):\n  -> %s\n", c.Line, side, c.Text)
		}
	}
}

// --- diff engine ---

// diffLines runs go-diff's line-mode recipe: lines are mapped to runes, diffed
// as a flat sequence, then mapped back, giving a clean per-line diff without
// intra-line noise.
func diffLines(base, proposed string) []diffmatchpatch.Diff {
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(base, proposed)
	diffs := dmp.DiffMain(a, b, false)
	return dmp.DiffCharsToLines(diffs, lines)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// A trailing newline produces a phantom empty last element.
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func diffStats(base, proposed string) (adds, dels int) {
	if base == proposed {
		return 0, 0
	}
	for _, d := range diffLines(base, proposed) {
		n := len(splitLines(d.Text))
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			adds += n
		case diffmatchpatch.DiffDelete:
			dels += n
		}
	}
	return adds, dels
}

const diffContext = 3

// diffHunks renders the full row stream (del/add/ctx with line numbers), then
// keeps only rows within diffContext lines of a change, grouped into hunks.
func diffHunks(base, proposed string) []DiffHunk {
	if base == proposed {
		return nil
	}

	var rows []DiffRow
	oldN, newN := 0, 0
	for _, d := range diffLines(base, proposed) {
		for _, line := range splitLines(d.Text) {
			switch d.Type {
			case diffmatchpatch.DiffDelete:
				oldN++
				rows = append(rows, DiffRow{T: "del", O: oldN, Text: line})
			case diffmatchpatch.DiffInsert:
				newN++
				rows = append(rows, DiffRow{T: "add", N: newN, Text: line})
			default:
				oldN++
				newN++
				rows = append(rows, DiffRow{T: "ctx", O: oldN, N: newN, Text: line})
			}
		}
	}

	keep := make([]bool, len(rows))
	for i, r := range rows {
		if r.T == "ctx" {
			continue
		}
		lo := max(0, i-diffContext)
		hi := min(len(rows)-1, i+diffContext)
		for j := lo; j <= hi; j++ {
			keep[j] = true
		}
	}

	var hunks []DiffHunk
	var cur []DiffRow
	for i, r := range rows {
		if keep[i] {
			cur = append(cur, r)
		} else if len(cur) > 0 {
			hunks = append(hunks, DiffHunk{Rows: cur})
			cur = nil
		}
	}
	if len(cur) > 0 {
		hunks = append(hunks, DiffHunk{Rows: cur})
	}
	return hunks
}
