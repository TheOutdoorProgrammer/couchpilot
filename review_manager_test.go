package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestReviewManager(t *testing.T) *ReviewManager {
	t.Helper()
	rm, err := NewReviewManager(t.TempDir(), NewSSEHub())
	if err != nil {
		t.Fatalf("NewReviewManager: %v", err)
	}
	rm.reviewOn = func(string) bool { return true }
	return rm
}

func writeEvent(t *testing.T, tool, toolUseID string, input map[string]any) hookEvent {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return hookEvent{ToolName: tool, ToolUseID: toolUseID, ToolInput: raw}
}

func submitWrite(t *testing.T, rm *ReviewManager, sessionID, path, content string) *Review {
	t.Helper()
	r, err := rm.Submit(sessionID, writeEvent(t, "Write", "tu-"+path, map[string]any{
		"file_path": path,
		"content":   content,
	}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return r
}

func TestCheckToken(t *testing.T) {
	rm := newTestReviewManager(t)
	if rm.HookToken() == "" {
		t.Fatal("hook token should be generated")
	}
	if !rm.CheckToken(rm.HookToken()) {
		t.Error("correct token rejected")
	}
	if rm.CheckToken("") {
		t.Error("empty token accepted")
	}
	if rm.CheckToken("wrong") {
		t.Error("wrong token accepted")
	}
}

func TestHookTokenPersists(t *testing.T) {
	dir := t.TempDir()
	rm1, err := NewReviewManager(dir, NewSSEHub())
	if err != nil {
		t.Fatal(err)
	}
	rm2, err := NewReviewManager(dir, NewSSEHub())
	if err != nil {
		t.Fatal(err)
	}
	if rm1.HookToken() != rm2.HookToken() {
		t.Error("hook token not stable across reload")
	}
}

func TestSubmitReviewOff(t *testing.T) {
	rm := newTestReviewManager(t)
	rm.reviewOn = func(string) bool { return false }
	r, err := rm.Submit("s1", writeEvent(t, "Write", "tu1", map[string]any{
		"file_path": "/tmp/x", "content": "y",
	}))
	if err != nil || r != nil {
		t.Errorf("review off: got (%v, %v), want (nil, nil)", r, err)
	}
}

func TestSubmitCreatesPendingAndSequences(t *testing.T) {
	rm := newTestReviewManager(t)
	r1 := submitWrite(t, rm, "s1", "/tmp/a", "A")
	if r1.Status != ReviewPending {
		t.Errorf("status = %s, want pending", r1.Status)
	}
	if r1.Seq != 0 {
		t.Errorf("first seq = %d, want 0", r1.Seq)
	}
	r2 := submitWrite(t, rm, "s1", "/tmp/b", "B")
	if r2.Seq != 1 {
		t.Errorf("second seq = %d, want 1", r2.Seq)
	}
	// A different session has its own sequence.
	r3 := submitWrite(t, rm, "s2", "/tmp/c", "C")
	if r3.Seq != 0 {
		t.Errorf("new session seq = %d, want 0", r3.Seq)
	}
}

func TestDecideApprove(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	out, err := rm.Decide(r.ID, "approve")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out.Status != ReviewApproved {
		t.Errorf("status = %s, want approved", out.Status)
	}
	if out.DecidedAt == nil {
		t.Error("DecidedAt should be set")
	}
	if out.PostContext != "" {
		t.Error("approve without comments should leave PostContext empty")
	}
}

func TestDecideDenyRequiresComment(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	if _, err := rm.Decide(r.ID, "deny"); err == nil {
		t.Error("deny with no comments should be rejected")
	}
	// The review must stay pending so the user can add a comment and retry.
	if got := rm.Get(r.ID, false); got.Status != ReviewPending {
		t.Errorf("status = %s, want still pending", got.Status)
	}
}

func TestDecideDenyWithComment(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	if _, err := rm.AddComment(r.ID, ReviewComment{Text: "no thanks"}); err != nil {
		t.Fatal(err)
	}
	out, err := rm.Decide(r.ID, "deny")
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if out.Status != ReviewDenied {
		t.Errorf("status = %s, want denied", out.Status)
	}
}

func TestDecideApproveWithCommentsStashesContext(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	rm.AddComment(r.ID, ReviewComment{Text: "nit: rename this"})
	out, err := rm.Decide(r.ID, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if out.PostContext == "" {
		t.Fatal("approve-with-comments should stash PostContext")
	}
	// TakePostContext delivers it exactly once, matched by tool_use_id.
	ctx := rm.TakePostContext("s1", r.ToolUseID)
	if !strings.Contains(ctx, "rename this") {
		t.Errorf("post context missing comment: %q", ctx)
	}
	if again := rm.TakePostContext("s1", r.ToolUseID); again != "" {
		t.Error("PostContext should only be delivered once")
	}
}

func TestDecideRejectsRepeatAndUnknown(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	if _, err := rm.Decide(r.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := rm.Decide(r.ID, "approve"); err == nil {
		t.Error("deciding an already-decided review should error")
	}
	r2 := submitWrite(t, rm, "s1", "/tmp/b", "B")
	if _, err := rm.Decide(r2.ID, "frobnicate"); err == nil {
		t.Error("unknown action should error")
	}
}

func TestAddCommentValidation(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")

	if _, err := rm.AddComment(r.ID, ReviewComment{Text: "   "}); err == nil {
		t.Error("blank comment should be rejected")
	}
	if _, err := rm.AddComment("nope", ReviewComment{Text: "hi"}); err == nil {
		t.Error("comment on missing review should error")
	}
	c, err := rm.AddComment(r.ID, ReviewComment{Text: "real"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.CreatedAt.IsZero() {
		t.Error("returned comment should have ID and timestamp")
	}
	// No comments allowed once decided.
	rm.Decide(r.ID, "approve")
	if _, err := rm.AddComment(r.ID, ReviewComment{Text: "late"}); err == nil {
		t.Error("comment on decided review should be rejected")
	}
}

func TestDeleteComment(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	c, _ := rm.AddComment(r.ID, ReviewComment{Text: "x"})
	if err := rm.DeleteComment(r.ID, "missing"); err == nil {
		t.Error("deleting unknown comment should error")
	}
	if err := rm.DeleteComment(r.ID, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := rm.Get(r.ID, false); len(got.Comments) != 0 {
		t.Errorf("comment not removed: %+v", got.Comments)
	}
}

func TestWaitAlreadyDecided(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	rm.Decide(r.ID, "approve")
	status, _, ok := rm.Wait(r.ID, time.Second)
	if !ok || status != ReviewApproved {
		t.Errorf("Wait on decided = (%s, %v), want (approved, true)", status, ok)
	}
}

func TestWaitMissingReview(t *testing.T) {
	rm := newTestReviewManager(t)
	if _, _, ok := rm.Wait("ghost", time.Second); ok {
		t.Error("Wait on unknown review should report ok=false")
	}
}

func TestWaitWakesOnDecide(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	rm.AddComment(r.ID, ReviewComment{Text: "fix"})

	type res struct {
		status ReviewStatus
		reason string
	}
	done := make(chan res, 1)
	go func() {
		s, reason, _ := rm.Wait(r.ID, 5*time.Second)
		done <- res{s, reason}
	}()

	// Wait until the waiter is registered, then deny — deterministic, no sleep.
	waitForWaiter(t, rm, r.ID)
	rm.Decide(r.ID, "deny")

	select {
	case got := <-done:
		if got.status != ReviewDenied {
			t.Errorf("woke with status %s, want denied", got.status)
		}
		if !strings.Contains(got.reason, "CHANGES REQUESTED") {
			t.Errorf("deny reason missing feedback: %q", got.reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not wake on decide")
	}
}

func TestWaitTimeoutStaysPending(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	status, _, ok := rm.Wait(r.ID, 30*time.Millisecond)
	if !ok || status != ReviewPending {
		t.Errorf("timed-out Wait = (%s, %v), want (pending, true)", status, ok)
	}
}

func TestCancelForSession(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")

	done := make(chan ReviewStatus, 1)
	go func() {
		s, _, _ := rm.Wait(r.ID, 5*time.Second)
		done <- s
	}()
	waitForWaiter(t, rm, r.ID)

	rm.CancelForSession("s1")
	if got := rm.Get(r.ID, false); got.Status != ReviewCancelled {
		t.Errorf("status = %s, want cancelled", got.Status)
	}
	select {
	case s := <-done:
		if s != ReviewCancelled {
			t.Errorf("waiter woke with %s, want cancelled", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CancelForSession did not wake the waiter")
	}
}

func TestDropSession(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	rm.DropSession("s1")
	if got := rm.Get(r.ID, false); got != nil {
		t.Error("review should be gone after DropSession")
	}
	if len(rm.List("s1", false)) != 0 {
		t.Error("session list should be empty after DropSession")
	}
}

func TestReviewsPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	rm, err := NewReviewManager(dir, NewSSEHub())
	if err != nil {
		t.Fatal(err)
	}
	rm.reviewOn = func(string) bool { return true }
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	rm.AddComment(r.ID, ReviewComment{Text: "keep me"})

	// A fresh manager over the same dir must recover the pending review so a
	// blocked hook can find its verdict after a couchpilot restart.
	rm2, err := NewReviewManager(dir, NewSSEHub())
	if err != nil {
		t.Fatal(err)
	}
	rm2.reviewOn = func(string) bool { return true }
	got := rm2.Get(r.ID, false)
	if got == nil {
		t.Fatal("review did not survive reload")
	}
	if got.Status != ReviewPending || len(got.Comments) != 1 {
		t.Errorf("reloaded review wrong: status=%s comments=%d", got.Status, len(got.Comments))
	}
	// nextSeq must continue past the reloaded review.
	r2 := submitWrite(t, rm2, "s1", "/tmp/b", "B")
	if r2.Seq != 1 {
		t.Errorf("post-reload seq = %d, want 1", r2.Seq)
	}
}

func TestListNewestFirstAndPendingFilter(t *testing.T) {
	rm := newTestReviewManager(t)
	a := submitWrite(t, rm, "s1", "/tmp/a", "A")
	time.Sleep(2 * time.Millisecond)
	b := submitWrite(t, rm, "s1", "/tmp/b", "B")
	rm.Decide(a.ID, "approve")

	all := rm.List("s1", false)
	if len(all) != 2 || all[0].ID != b.ID {
		t.Errorf("List should be newest-first; got %d items", len(all))
	}
	pending := rm.List("s1", true)
	if len(pending) != 1 || pending[0].ID != b.ID {
		t.Errorf("pending filter wrong: %+v", pending)
	}
}

func TestBuildProposalWriteNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	fp, base, proposed, flags, err := buildProposal(writeEvent(t, "Write", "t", map[string]any{
		"file_path": path, "content": "hello\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if fp != path || base != "" || proposed != "hello\n" {
		t.Errorf("got fp=%q base=%q proposed=%q", fp, base, proposed)
	}
	if !flags.newFile {
		t.Error("newFile flag should be set for a non-existent target")
	}
}

func TestBuildProposalEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0644)

	_, base, proposed, flags, err := buildProposal(writeEvent(t, "Edit", "t", map[string]any{
		"file_path": path, "old_string": "two", "new_string": "TWO",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if base != "one\ntwo\nthree\n" {
		t.Errorf("base = %q", base)
	}
	if proposed != "one\nTWO\nthree\n" {
		t.Errorf("proposed = %q", proposed)
	}
	if flags.noMatch {
		t.Error("noMatch should be false for a matching edit")
	}
}

func TestBuildProposalEditNoMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(path, []byte("abc\n"), 0644)
	_, _, _, flags, err := buildProposal(writeEvent(t, "Edit", "t", map[string]any{
		"file_path": path, "old_string": "zzz", "new_string": "q",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !flags.noMatch {
		t.Error("noMatch should be set when old_string is absent")
	}
}

func TestBuildProposalMultiEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(path, []byte("a b c\n"), 0644)
	_, _, proposed, _, err := buildProposal(writeEvent(t, "MultiEdit", "t", map[string]any{
		"file_path": path,
		"edits": []map[string]any{
			{"old_string": "a", "new_string": "A"},
			{"old_string": "c", "new_string": "C"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if proposed != "A b C\n" {
		t.Errorf("proposed = %q, want %q", proposed, "A b C\n")
	}
}

func TestBuildProposalNotebook(t *testing.T) {
	_, _, proposed, flags, err := buildProposal(writeEvent(t, "NotebookEdit", "t", map[string]any{
		"notebook_path": "/tmp/nb.ipynb", "new_source": "print(1)",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !flags.notebook || proposed != "print(1)" {
		t.Errorf("notebook handling wrong: flags=%+v proposed=%q", flags, proposed)
	}
}

func TestBuildProposalErrors(t *testing.T) {
	if _, _, _, _, err := buildProposal(writeEvent(t, "Write", "t", map[string]any{"content": "x"})); err == nil {
		t.Error("missing file path should error")
	}
	path := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(path, []byte("x"), 0644)
	if _, _, _, _, err := buildProposal(writeEvent(t, "Bash", "t", map[string]any{"file_path": path})); err == nil {
		t.Error("unsupported tool should error")
	}
}

func TestBuildProposalTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	big := strings.Repeat("x", reviewMaxBytes+1)
	_, base, proposed, flags, err := buildProposal(writeEvent(t, "Write", "t", map[string]any{
		"file_path": path, "content": big,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !flags.tooLarge {
		t.Error("tooLarge flag should be set")
	}
	if base != "" || proposed != "" {
		t.Error("oversized content should be dropped from the review")
	}
}

func TestDenyReasonFormatting(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/file.go", "A")
	rm.AddComment(r.ID, ReviewComment{Text: "overall: bad idea"})
	rm.AddComment(r.ID, ReviewComment{Line: 5, Side: "new", LineText: "x := 1", Text: "use a const"})
	rm.Decide(r.ID, "deny")

	reason := rm.denyReasonLocked(rm.byID[r.ID])
	for _, want := range []string{"CHANGES REQUESTED", "/tmp/file.go", "Overall: overall: bad idea", "Line 5 (new)", "use a const"} {
		if !strings.Contains(reason, want) {
			t.Errorf("deny reason missing %q:\n%s", want, reason)
		}
	}
}

func TestCancelledReasonTellsClaudeToStop(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	rm.CancelForSession("s1")
	reason := rm.denyReasonLocked(rm.byID[r.ID])
	if !strings.Contains(reason, "cancelled") || !strings.Contains(reason, "Do not retry") {
		t.Errorf("cancelled reason wrong: %q", reason)
	}
}

func TestTrimRetention(t *testing.T) {
	rm := newTestReviewManager(t)
	// One pending review must always survive the trim.
	pending := submitWrite(t, rm, "s1", "/tmp/keep", "K")
	// Push well past the retention cap of decided reviews.
	for i := 0; i < reviewRetention+5; i++ {
		r := submitWrite(t, rm, "s1", "/tmp/x", "X")
		rm.Decide(r.ID, "approve")
	}
	// trimLocked runs on Submit, before the just-submitted review is decided,
	// so the last decided review isn't trimmed until the next submit. Trigger
	// one more submit to force the trim to settle at the cap.
	submitWrite(t, rm, "s1", "/tmp/trigger", "T")

	list := rm.List("s1", false)
	decided := 0
	pendingSeen := false
	for _, v := range list {
		if v.Status == ReviewPending {
			pendingSeen = true
		} else {
			decided++
		}
	}
	if decided > reviewRetention {
		t.Errorf("decided count = %d, want <= %d", decided, reviewRetention)
	}
	if !pendingSeen || rm.Get(pending.ID, false) == nil {
		t.Error("pending review must never be trimmed")
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 2}, // no trailing newline
		{"\n", 1},   // single blank line
	}
	for _, c := range cases {
		if got := len(splitLines(c.in)); got != c.want {
			t.Errorf("splitLines(%q) = %d lines, want %d", c.in, got, c.want)
		}
	}
}

// waitForWaiter blocks until a Wait() call has registered a waiter channel for
// the review, making decide-then-wake tests deterministic without sleeps.
func waitForWaiter(t *testing.T, rm *ReviewManager, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rm.mu.Lock()
		n := len(rm.waiters[id])
		rm.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no waiter registered in time")
}

func TestUpdateCommentRange(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	c, err := rm.AddComment(r.ID, ReviewComment{Line: 8, Side: "new", LineText: "return foo", Text: "note"})
	if err != nil {
		t.Fatal(err)
	}

	span := ReviewComment{
		Line: 8, Side: "new", LineText: "return foo",
		StartLine: 5, StartSide: "new", StartText: "foo := bar()",
	}
	got, err := rm.UpdateCommentRange(r.ID, c.ID, span)
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	if !got.isRange() || got.StartLine != 5 || got.Line != 8 {
		t.Errorf("range not applied: %+v", got)
	}
	if got.Text != "note" {
		t.Errorf("body must be preserved, got %q", got.Text)
	}

	// Zero start collapses it back to a single line.
	got, err = rm.UpdateCommentRange(r.ID, c.ID, ReviewComment{Line: 8, Side: "new", LineText: "return foo"})
	if err != nil {
		t.Fatal(err)
	}
	if got.isRange() {
		t.Errorf("should collapse to single line: %+v", got)
	}

	if _, err := rm.UpdateCommentRange("nope", c.ID, span); err == nil {
		t.Error("update on missing review should error")
	}
	if _, err := rm.UpdateCommentRange(r.ID, "missing", span); err == nil {
		t.Error("update on missing comment should error")
	}
	if _, err := rm.UpdateCommentRange(r.ID, c.ID, ReviewComment{Line: 0}); err == nil {
		t.Error("range without an anchor should error")
	}
	g, _ := rm.AddComment(r.ID, ReviewComment{Text: "overall"})
	if _, err := rm.UpdateCommentRange(r.ID, g.ID, span); err == nil {
		t.Error("range on an overall comment should error")
	}

	rm.Decide(r.ID, "approve")
	if _, err := rm.UpdateCommentRange(r.ID, c.ID, span); err == nil {
		t.Error("update on decided review should error")
	}
}

func TestUpdateCommentRangePersists(t *testing.T) {
	dir := t.TempDir()
	rm, err := NewReviewManager(dir, NewSSEHub())
	if err != nil {
		t.Fatal(err)
	}
	rm.reviewOn = func(string) bool { return true }
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")
	c, _ := rm.AddComment(r.ID, ReviewComment{Line: 8, Side: "new", LineText: "return foo", Text: "x"})
	if _, err := rm.UpdateCommentRange(r.ID, c.ID, ReviewComment{
		Line: 8, Side: "new", LineText: "return foo",
		StartLine: 5, StartSide: "new", StartText: "foo := bar()",
	}); err != nil {
		t.Fatal(err)
	}

	rm2, err := NewReviewManager(dir, NewSSEHub())
	if err != nil {
		t.Fatal(err)
	}
	rm2.reviewOn = func(string) bool { return true }
	got := rm2.Get(r.ID, false)
	if got == nil || len(got.Comments) != 1 {
		t.Fatalf("reload wrong: %+v", got)
	}
	if rc := got.Comments[0]; !rc.isRange() || rc.StartLine != 5 || rc.StartText != "foo := bar()" {
		t.Errorf("range didn't persist: %+v", rc)
	}
}

func TestUpdateCommentText(t *testing.T) {
	rm := newTestReviewManager(t)
	r := submitWrite(t, rm, "s1", "/tmp/a", "A")

	// Editing text must leave a range comment's span intact.
	c, _ := rm.AddComment(r.ID, ReviewComment{
		Line: 8, Side: "new", LineText: "return foo",
		StartLine: 5, StartSide: "new", StartText: "foo := bar()", Text: "old",
	})
	got, err := rm.UpdateCommentText(r.ID, c.ID, "new words")
	if err != nil {
		t.Fatalf("update text: %v", err)
	}
	if got.Text != "new words" {
		t.Errorf("text = %q, want %q", got.Text, "new words")
	}
	if !got.isRange() || got.StartLine != 5 || got.Line != 8 {
		t.Errorf("range must survive a text edit: %+v", got)
	}

	// Overall (global) comment text is editable too and stays global.
	g, _ := rm.AddComment(r.ID, ReviewComment{Text: "overall old"})
	gg, err := rm.UpdateCommentText(r.ID, g.ID, "overall new")
	if err != nil {
		t.Fatal(err)
	}
	if gg.Text != "overall new" || gg.Line != 0 {
		t.Errorf("global text edit wrong: %+v", gg)
	}

	if _, err := rm.UpdateCommentText(r.ID, c.ID, "  "); err == nil {
		t.Error("blank text should be rejected")
	}
	if _, err := rm.UpdateCommentText("nope", c.ID, "x"); err == nil {
		t.Error("missing review should error")
	}
	if _, err := rm.UpdateCommentText(r.ID, "missing", "x"); err == nil {
		t.Error("missing comment should error")
	}
	rm.Decide(r.ID, "approve")
	if _, err := rm.UpdateCommentText(r.ID, c.ID, "x"); err == nil {
		t.Error("update on decided review should error")
	}
}
