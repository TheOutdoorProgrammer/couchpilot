package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func cookieValue(w *httptest.ResponseRecorder, name string) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func cookieMaxAge(w *httptest.ResponseRecorder, name string) int {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c.MaxAge
		}
	}
	return 0
}

func TestHandleAuthStatus(t *testing.T) {
	s := newFullServer(t)
	s.cfg.AuthEnabled = true
	if err := s.am.SetPassword("secret1"); err != nil {
		t.Fatal(err)
	}
	w := doReq(t, s.handleAuthStatus, "GET", "/api/auth/status", "", nil)
	body := w.Body.String()
	if !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"hasPassword":true`) {
		t.Errorf("status payload wrong: %s", body)
	}
}

func TestHandleAuthLogin(t *testing.T) {
	s := newFullServer(t)
	s.cfg.AuthEnabled = true
	s.am.SetPassword("hunter2!")

	t.Run("wrong password", func(t *testing.T) {
		w := doReq(t, s.handleAuthLogin, "POST", "/api/auth/login", `{"password":"nope"}`, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})
	t.Run("correct password sets cookie", func(t *testing.T) {
		w := doReq(t, s.handleAuthLogin, "POST", "/api/auth/login", `{"password":"hunter2!"}`, nil)
		if w.Code != 200 {
			t.Fatalf("status = %d", w.Code)
		}
		tok := cookieValue(w, sessionCookie)
		if tok == "" || !s.am.validateToken(tok) {
			t.Error("login should set a valid session cookie")
		}
	})
}

func TestHandleAuthLoginNoPasswordSet(t *testing.T) {
	s := newFullServer(t)
	s.cfg.AuthEnabled = true
	w := doReq(t, s.handleAuthLogin, "POST", "/api/auth/login", `{"password":"x"}`, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("login with no password set = %d, want 409", w.Code)
	}
}

func TestHandleAuthLoginDisabledShortCircuits(t *testing.T) {
	s := newFullServer(t) // auth disabled
	w := doReq(t, s.handleAuthLogin, "POST", "/api/auth/login", `{"password":"whatever"}`, nil)
	if w.Code != 200 {
		t.Errorf("auth-disabled login = %d, want 200 ok", w.Code)
	}
}

func TestHandleAuthSetup(t *testing.T) {
	s := newFullServer(t)
	s.cfg.AuthEnabled = true // enabled, no password yet → first-run setup window

	w := doReq(t, s.handleAuthSetup, "POST", "/api/auth/setup", `{"password":"brandnew1"}`, nil)
	if w.Code != 200 {
		t.Fatalf("setup = %d: %s", w.Code, w.Body.String())
	}
	if !s.am.CheckPassword("brandnew1") {
		t.Error("password not set by setup")
	}
	if cookieValue(w, sessionCookie) == "" {
		t.Error("setup should log the user in")
	}

	// Once a password exists, setup is forbidden.
	w = doReq(t, s.handleAuthSetup, "POST", "/api/auth/setup", `{"password":"another1"}`, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("second setup = %d, want 403", w.Code)
	}
}

func TestHandleAuthSetupDisable(t *testing.T) {
	s := newFullServer(t)
	s.cfg.AuthEnabled = true
	w := doReq(t, s.handleAuthSetup, "POST", "/api/auth/setup", `{"disable":true}`, nil)
	if w.Code != 200 {
		t.Fatalf("disable setup = %d", w.Code)
	}
	if s.cfgSnapshot().AuthEnabled {
		t.Error("auth should be disabled after opt-out")
	}
}

func TestHandleAuthConfigSetPasswordEnables(t *testing.T) {
	s := newFullServer(t) // auth off, no password
	w := doReq(t, s.handleAuthConfig, "PUT", "/api/auth/config", `{"newPassword":"freshpw1","enabled":true}`, nil)
	if w.Code != 200 {
		t.Fatalf("auth config = %d: %s", w.Code, w.Body.String())
	}
	if !s.am.CheckPassword("freshpw1") {
		t.Error("password not changed")
	}
	if !s.cfgSnapshot().AuthEnabled {
		t.Error("auth should be enabled")
	}
}

func TestHandleAuthConfigEnableWithoutPasswordRejected(t *testing.T) {
	s := newFullServer(t) // no password
	w := doReq(t, s.handleAuthConfig, "PUT", "/api/auth/config", `{"enabled":true}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("enabling auth with no password = %d, want 400", w.Code)
	}
}

func TestHandleAuthLogoutClearsCookie(t *testing.T) {
	s := newFullServer(t)
	w := doReq(t, s.handleAuthLogout, "POST", "/api/auth/logout", "", nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("logout = %d, want 204", w.Code)
	}
	// A negative MaxAge tells the browser to delete the cookie.
	if cookieMaxAge(w, sessionCookie) >= 0 {
		t.Error("logout should expire the session cookie")
	}
}

// --- remaining small handlers ---

func TestHandleDismissSession(t *testing.T) {
	s := newFullServer(t)
	s.sm.sessions["d"] = &Session{ID: "d", Status: StatusDead}
	w := doReq(t, s.handleDismissSession, "POST", "/api/sessions/d/dismiss", "", map[string]string{"id": "d"})
	if w.Code != http.StatusNoContent {
		t.Errorf("dismiss = %d, want 204", w.Code)
	}
	if _, ok := s.sm.sessions["d"]; ok {
		t.Error("dead session should be gone after dismiss")
	}
}

func TestHandleDeleteReviewComment(t *testing.T) {
	s := newFullServer(t)
	r := seedReview(t, s, "s1")
	c, _ := s.rm.AddComment(r.ID, ReviewComment{Text: "x"})

	w := doReq(t, s.handleDeleteReviewComment, "DELETE", "/api/reviews/"+r.ID+"/comments/"+c.ID, "",
		map[string]string{"id": r.ID, "cid": c.ID})
	if w.Code != http.StatusNoContent {
		t.Errorf("delete comment = %d, want 204", w.Code)
	}

	w = doReq(t, s.handleDeleteReviewComment, "DELETE", "/api/reviews/"+r.ID+"/comments/missing", "",
		map[string]string{"id": r.ID, "cid": "missing"})
	if w.Code != http.StatusNotFound {
		t.Errorf("delete missing comment = %d, want 404", w.Code)
	}
}

func TestHandlePushUnsubscribe(t *testing.T) {
	s := newFullServer(t)
	w := doReq(t, s.handlePushUnsubscribe, "POST", "/api/push/unsubscribe", `{"endpoint":"https://x/a"}`, nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("unsubscribe = %d, want 204", w.Code)
	}
}

func TestHandleListModels(t *testing.T) {
	s := newFullServer(t)
	// Seed the global cache so the handler doesn't reach out to models.dev.
	models.mu.Lock()
	models.models = []ModelInfo{{ID: "seed-model", Name: "Seed"}}
	models.fetchedAt = time.Now()
	models.mu.Unlock()

	w := doReq(t, s.handleListModels, "GET", "/api/models", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "seed-model") {
		t.Errorf("list models = %d %s", w.Code, w.Body.String())
	}
}
