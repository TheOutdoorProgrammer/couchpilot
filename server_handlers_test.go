package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFullServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		Port:                  7080,
		AuthEnabled:           false,
		DefaultDir:            "~/",
		DefaultPermissionMode: "bypassPermissions",
		configPath:            filepath.Join(dir, "config.json"),
	}
	hub := NewSSEHub()
	rm, err := NewReviewManager(dir, hub)
	if err != nil {
		t.Fatal(err)
	}
	sm := &SessionManager{sessions: map[string]*Session{}, hub: hub, dataDir: dir}
	sm.SetHookEnv(cfg.Port, rm.HookToken())
	rm.reviewOn = sm.ReviewModeOn
	am, err := NewAuthManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	pm, err := NewPushManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(cfg, sm, NewLoginManager(hub), hub, am, rm, pm)
}

func doReq(t *testing.T, h func(http.ResponseWriter, *http.Request), method, target, body string, pathVals map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rdr)
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func TestHandleVersion(t *testing.T) {
	s := newFullServer(t)
	w := doReq(t, s.handleVersion, "GET", "/api/version", "", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var v VersionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Version == "" {
		t.Error("version should be populated")
	}
}

func TestHandleGetConfigHidesNoSecrets(t *testing.T) {
	s := newFullServer(t)
	w := doReq(t, s.handleGetConfig, "GET", "/api/config", "", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	// The config payload must never carry auth secrets — those live in auth.json.
	if strings.Contains(w.Body.String(), "passwordHash") || strings.Contains(w.Body.String(), "secret") {
		t.Errorf("config leaked a secret field: %s", w.Body.String())
	}
}

func TestHandleUpdateConfig(t *testing.T) {
	s := newFullServer(t)
	w := doReq(t, s.handleUpdateConfig, "PUT", "/api/config", `{"defaultModel":"claude-opus-4-6","defaultEffort":"high"}`, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.cfgSnapshot(); got.DefaultModel != "claude-opus-4-6" || got.DefaultEffort != "high" {
		t.Errorf("config not updated: %+v", got)
	}
}

func TestHandleListSessions(t *testing.T) {
	s := newFullServer(t)
	s.sm.sessions["s1"] = &Session{ID: "s1", Name: "one", CreatedAt: time.Now()}
	w := doReq(t, s.handleListSessions, "GET", "/api/sessions", "", nil)
	var got []*Session
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 1 || got[0].ID != "s1" {
		t.Errorf("unexpected sessions: %+v", got)
	}
}

func TestHandleSetReviewMode(t *testing.T) {
	s := newFullServer(t)
	s.sm.sessions["s1"] = &Session{ID: "s1", Status: StatusActive}

	w := doReq(t, s.handleSetReviewMode, "POST", "/api/sessions/s1/review-mode", `{"enabled":true}`, map[string]string{"id": "s1"})
	if w.Code != 204 {
		t.Fatalf("status = %d", w.Code)
	}
	if !s.sm.ReviewModeOn("s1") {
		t.Error("review mode should be on")
	}

	w = doReq(t, s.handleSetReviewMode, "POST", "/api/sessions/ghost/review-mode", `{"enabled":true}`, map[string]string{"id": "ghost"})
	if w.Code != http.StatusNotFound {
		t.Errorf("missing session = %d, want 404", w.Code)
	}
}

func TestHandleKillChannelsSessionForbidden(t *testing.T) {
	s := newFullServer(t)
	s.sm.sessions["ch"] = &Session{ID: "ch", Status: StatusActive, IsChannels: true}
	w := doReq(t, s.handleKillSession, "DELETE", "/api/sessions/ch", "", map[string]string{"id": "ch"})
	if w.Code != http.StatusForbidden {
		t.Errorf("killing channels session = %d, want 403", w.Code)
	}
}

// --- review CRUD over HTTP ---

func seedReview(t *testing.T, s *Server, sessionID string) *Review {
	t.Helper()
	s.sm.sessions[sessionID] = &Session{ID: sessionID, Status: StatusActive, ReviewMode: true}
	r, err := s.rm.Submit(sessionID, writeEvent(t, "Write", "tu1", map[string]any{
		"file_path": "/tmp/seed", "content": "x",
	}))
	if err != nil || r == nil {
		t.Fatalf("seed review: r=%v err=%v", r, err)
	}
	return r
}

func TestHandleReviewListAndGet(t *testing.T) {
	s := newFullServer(t)
	r := seedReview(t, s, "s1")

	w := doReq(t, s.handleListReviews, "GET", "/api/reviews", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), r.ID) {
		t.Errorf("list reviews missing %s: %s", r.ID, w.Body.String())
	}

	w = doReq(t, s.handleGetReview, "GET", "/api/reviews/"+r.ID, "", map[string]string{"id": r.ID})
	if w.Code != 200 {
		t.Errorf("get review status = %d", w.Code)
	}

	w = doReq(t, s.handleGetReview, "GET", "/api/reviews/ghost", "", map[string]string{"id": "ghost"})
	if w.Code != http.StatusNotFound {
		t.Errorf("missing review = %d, want 404", w.Code)
	}
}

func TestHandleAddCommentAndDecide(t *testing.T) {
	s := newFullServer(t)
	r := seedReview(t, s, "s1")

	// Deny with no comments is rejected by the manager (409).
	w := doReq(t, s.handleReviewDecision, "POST", "/api/reviews/"+r.ID+"/decision", `{"action":"deny"}`, map[string]string{"id": r.ID})
	if w.Code != http.StatusConflict {
		t.Errorf("deny w/o comment = %d, want 409", w.Code)
	}

	// Add a comment, then deny succeeds.
	w = doReq(t, s.handleAddReviewComment, "POST", "/api/reviews/"+r.ID+"/comments", `{"text":"fix this"}`, map[string]string{"id": r.ID})
	if w.Code != http.StatusCreated {
		t.Fatalf("add comment = %d: %s", w.Code, w.Body.String())
	}
	w = doReq(t, s.handleReviewDecision, "POST", "/api/reviews/"+r.ID+"/decision", `{"action":"deny"}`, map[string]string{"id": r.ID})
	if w.Code != 200 {
		t.Errorf("deny w/ comment = %d, want 200", w.Code)
	}
	if got := s.rm.Get(r.ID, false); got.Status != ReviewDenied {
		t.Errorf("review status = %s, want denied", got.Status)
	}
}

// --- hook endpoints ---

func TestHookAuth(t *testing.T) {
	s := newFullServer(t)
	r := httptest.NewRequest("POST", "/api/hook/review", nil)
	if s.hookAuthed(r) {
		t.Error("request with no token should not be authed")
	}
	r.Header.Set("X-Couchpilot-Hook", s.rm.HookToken())
	if !s.hookAuthed(r) {
		t.Error("request with the hook token should be authed")
	}
}

func TestHandleHookReviewRejectsBadToken(t *testing.T) {
	s := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/hook/review", strings.NewReader(`{}`))
	req.Header.Set("X-Couchpilot-Hook", "wrong")
	w := httptest.NewRecorder()
	s.handleHookReview(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad hook token = %d, want 401", w.Code)
	}
}

func TestHandleHookReviewAllowsWhenReviewOff(t *testing.T) {
	s := newFullServer(t)
	// Session exists but review mode is off → hook should be told to allow.
	s.sm.sessions["s1"] = &Session{ID: "s1", Status: StatusActive}
	event := `{"tool_name":"Write","tool_use_id":"t","tool_input":{"file_path":"/tmp/x","content":"y"}}`
	body := `{"sessionId":"s1","event":` + event + `}`
	req := httptest.NewRequest("POST", "/api/hook/review", strings.NewReader(body))
	req.Header.Set("X-Couchpilot-Hook", s.rm.HookToken())
	w := httptest.NewRecorder()
	s.handleHookReview(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "allow") {
		t.Errorf("review-off hook = %d %s, want allow", w.Code, w.Body.String())
	}
}

// --- push endpoints ---

func TestHandlePushKey(t *testing.T) {
	s := newFullServer(t)
	w := doReq(t, s.handlePushKey, "GET", "/api/push/key", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "key") {
		t.Errorf("push key = %d %s", w.Code, w.Body.String())
	}
}

func TestHandlePushSubscribeValidation(t *testing.T) {
	s := newFullServer(t)
	// Missing endpoint is rejected.
	w := doReq(t, s.handlePushSubscribe, "POST", "/api/push/subscribe", `{}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty subscription = %d, want 400", w.Code)
	}
	// A valid subscription is stored.
	w = doReq(t, s.handlePushSubscribe, "POST", "/api/push/subscribe", `{"endpoint":"https://push.example/a","keys":{"p256dh":"k","auth":"a"}}`, nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("valid subscription = %d, want 204", w.Code)
	}
	if s.pm.Count() != 1 {
		t.Errorf("subscription not stored: count = %d", s.pm.Count())
	}
}

func TestReviewErrCode(t *testing.T) {
	if reviewErrCode(os.ErrNotExist) != http.StatusNotFound {
		t.Error("ErrNotExist should map to 404")
	}
	if reviewErrCode(errors.New("boom")) != http.StatusConflict {
		t.Error("other errors should map to 409")
	}
}
