package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://localhost:7080", "localhost:7080", true},
		{"https://couchpilot.stout.zone", "couchpilot.stout.zone", true},
		{"http://evil.com", "localhost:7080", false},
		{"http://localhost:7080", "localhost:9999", false},
		{"::not a url", "localhost", false},
		{"", "localhost", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.origin, c.host); got != c.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

func TestAuthRequired(t *testing.T) {
	cases := map[string]bool{
		"/":                     false, // static shell must load
		"/static/app.js":        false,
		"/sw.js":                false,
		"/api/hook/review":      false, // hooks present their own token
		"/api/auth/status":      false,
		"/api/auth/login":       false,
		"/api/version":          false,
		"/api/sessions":         true,
		"/api/config":           true,
		"/api/sessions/x/model": true,
	}
	for path, want := range cases {
		if got := authRequired(path); got != want {
			t.Errorf("authRequired(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAssetVersionComputed(t *testing.T) {
	// init() hashes the embedded SPA assets into an 8-char tag for cache-busting.
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(assetVersion) {
		t.Errorf("assetVersion = %q, want 8 hex chars", assetVersion)
	}
}

func newModelTestServer(t *testing.T) *Server {
	t.Helper()
	sm := &SessionManager{sessions: map[string]*Session{}, hub: NewSSEHub(), dataDir: t.TempDir()}
	return &Server{sm: sm}
}

func TestHandleChangeModelValidation(t *testing.T) {
	s := newModelTestServer(t)

	tests := []struct {
		name, id, body string
		want           int
	}{
		{"bad json", "x", "{not json", http.StatusBadRequest},
		{"empty model", "x", `{"model":""}`, http.StatusBadRequest},
		{"missing session", "ghost", `{"model":"sonnet"}`, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("PUT", "/api/sessions/"+tt.id+"/model", strings.NewReader(tt.body))
			req.SetPathValue("id", tt.id)
			w := httptest.NewRecorder()
			s.handleChangeModel(w, req)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

func newAuthTestServer(t *testing.T, enabled bool) (*Server, *AuthManager) {
	t.Helper()
	am := newTestAuth(t)
	srv := &Server{cfg: &Config{AuthEnabled: enabled}, am: am}
	return srv, am
}

func TestAuthMiddlewareBlocksUnauthed(t *testing.T) {
	srv, _ := newAuthTestServer(t, true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.authMiddleware(next)

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthed protected request = %d, want 401", w.Code)
	}
}

func TestAuthMiddlewareAllowsAuthed(t *testing.T) {
	srv, am := newAuthTestServer(t, true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.authMiddleware(next)

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: am.issueToken(time.Hour)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("authed request = %d, want passthrough (418)", w.Code)
	}
}

func TestAuthMiddlewareOpenEndpointsBypass(t *testing.T) {
	srv, _ := newAuthTestServer(t, true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.authMiddleware(next)

	// /api/version is unauthenticated even when auth is enabled.
	req := httptest.NewRequest("GET", "/api/version", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("open endpoint = %d, want passthrough (418)", w.Code)
	}
}

func TestAuthMiddlewareDisabledPasses(t *testing.T) {
	srv, _ := newAuthTestServer(t, false)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.authMiddleware(next)

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("auth-disabled request = %d, want passthrough (418)", w.Code)
	}
}

func TestAuthMiddlewareBlocksCrossOrigin(t *testing.T) {
	srv, _ := newAuthTestServer(t, false) // even with auth off, CSRF guard applies
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.authMiddleware(next)

	req := httptest.NewRequest("POST", "/api/sessions", nil)
	req.Host = "localhost:7080"
	req.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", w.Code)
	}
}

func TestAuthMiddlewareAllowsSameOriginPost(t *testing.T) {
	srv, _ := newAuthTestServer(t, false)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.authMiddleware(next)

	req := httptest.NewRequest("POST", "/api/sessions", nil)
	req.Host = "localhost:7080"
	req.Header.Set("Origin", "http://localhost:7080")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("same-origin POST = %d, want passthrough (418)", w.Code)
	}
}
