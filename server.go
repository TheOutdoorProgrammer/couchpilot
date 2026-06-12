package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

type SSEEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type SSEHub struct {
	clients map[chan SSEEvent]struct{}
	mu      sync.RWMutex
}

func NewSSEHub() *SSEHub {
	return &SSEHub{clients: make(map[chan SSEEvent]struct{})}
}

func (h *SSEHub) Register() chan SSEEvent {
	ch := make(chan SSEEvent, 32)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *SSEHub) Unregister(ch chan SSEEvent) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *SSEHub) Broadcast(event SSEEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

type Server struct {
	cfg   *Config
	cfgMu sync.RWMutex
	sm    *SessionManager
	lm    *LoginManager
	hub   *SSEHub
	am    *AuthManager
}

func NewServer(cfg *Config, sm *SessionManager, lm *LoginManager, hub *SSEHub, am *AuthManager) *Server {
	srv := &Server{cfg: cfg, sm: sm, lm: lm, hub: hub, am: am}

	sm.onChannelsDied = func() {
		srv.ensureChannelsSession()
	}

	return srv
}

// cfgSnapshot returns a consistent copy of the config for concurrent readers.
// Config carries no embedded lock, so the value copy is vet-clean; writers
// replace slices wholesale (never mutate in place), so sharing the slice
// backing arrays in the snapshot is safe.
func (s *Server) cfgSnapshot() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return *s.cfg
}

// updateConfig mutates the config under the write lock and persists it. The
// mutate callback runs while the lock is held, so it must not do anything slow
// (no spawning sessions) — do that after updateConfig returns.
func (s *Server) updateConfig(mutate func(*Config)) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	mutate(s.cfg)
	return s.cfg.Save()
}

func (s *Server) Start() error {
	s.ensureChannelsSession()

	mux := http.NewServeMux()

	sub, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(sub))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, _ := staticFS.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))

	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleKillSession)
	mux.HandleFunc("POST /api/sessions/{id}/dismiss", s.handleDismissSession)
	mux.HandleFunc("POST /api/sessions/{id}/resume", s.handleResumeSession)
	mux.HandleFunc("POST /api/channels/restart", s.handleRestartChannels)
	mux.HandleFunc("GET /api/events", s.handleSSE)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handleUpdateConfig)
	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("GET /api/branches", s.handleListBranches)
	mux.HandleFunc("GET /api/models", s.handleListModels)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /api/login", s.handleLoginState)
	mux.HandleFunc("POST /api/login", s.handleLoginStart)
	mux.HandleFunc("POST /api/login/input", s.handleLoginInput)
	mux.HandleFunc("DELETE /api/login", s.handleLoginStop)

	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("PUT /api/auth/config", s.handleAuthConfig)

	cfg := s.cfgSnapshot()
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return http.ListenAndServe(addr, s.authMiddleware(mux))
}

// authMiddleware enforces password auth (when enabled) and blocks cross-origin
// state-changing requests as CSRF defense-in-depth. Static assets and the
// unauthenticated auth endpoints (status/login/setup) always pass through.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
				httpError(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
		}
		if s.cfgSnapshot().AuthEnabled && authRequired(r.URL.Path) && !s.am.requestAuthed(r) {
			httpError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authRequired(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false // static assets must load so the login page can render
	}
	switch path {
	case "/api/auth/status", "/api/auth/login", "/api/auth/setup", "/api/version":
		return false
	}
	return true
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == host
}

func (s *Server) authStatus(r *http.Request) map[string]bool {
	enabled := s.cfgSnapshot().AuthEnabled
	hasPassword := s.am.HasPassword()
	return map[string]bool{
		"enabled":     enabled,
		"hasPassword": hasPassword,
		"needsSetup":  enabled && !hasPassword,
		"authed":      !enabled || s.am.requestAuthed(r),
	}
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.authStatus(r))
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfgSnapshot().AuthEnabled {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !s.am.HasPassword() {
		httpError(w, "no password set", http.StatusConflict)
		return
	}
	if !s.am.CheckPassword(req.Password) {
		httpError(w, "incorrect password", http.StatusUnauthorized)
		return
	}
	s.setSessionCookie(w)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if !s.cfgSnapshot().AuthEnabled {
		httpError(w, "auth is disabled", http.StatusBadRequest)
		return
	}
	if s.am.HasPassword() {
		httpError(w, "password already set", http.StatusForbidden)
		return
	}
	var req struct {
		Password string `json:"password"`
		Disable  bool   `json:"disable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// This endpoint is only reachable in the first-run window (auth enabled, no
	// password). A local user could set their own password here, so letting them
	// instead opt out for a trusted network carries no extra risk.
	if req.Disable {
		if err := s.updateConfig(func(c *Config) { c.AuthEnabled = false }); err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if err := s.am.SetPassword(req.Password); err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.setSessionCookie(w)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.am.tokenCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// handleAuthConfig changes security settings. It sits behind the middleware, so
// when auth is enabled only an authenticated user can reach it.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled     *bool   `json:"enabled"`
		NewPassword *string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.NewPassword != nil {
		if err := s.am.SetPassword(*req.NewPassword); err != nil {
			httpError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.setSessionCookie(w) // keep the caller logged in after a password change
	}

	if req.Enabled != nil {
		// Refuse to enable auth with no password — it would lock the UI into
		// first-run setup. The client must send newPassword in the same call.
		if *req.Enabled && !s.am.HasPassword() {
			httpError(w, "set a password before enabling auth", http.StatusBadRequest)
			return
		}
		if err := s.updateConfig(func(c *Config) { c.AuthEnabled = *req.Enabled }); err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, s.authStatus(r))
}

func (s *Server) setSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, s.am.tokenCookie(s.am.issueToken(tokenTTL), int(tokenTTL/time.Second)))
}

func (s *Server) ensureChannelsSession() {
	cfg := s.cfgSnapshot()
	if !cfg.ChannelsEnabled || cfg.DefaultChannels == "" {
		return
	}

	// GetSessions is newest-first, so the first dead channels session we see
	// is the most recent one.
	var deadChannels *Session
	for _, sess := range s.sm.GetSessions() {
		if !sess.IsChannels {
			continue
		}
		if sess.Status != StatusDead {
			return
		}
		if deadChannels == nil {
			deadChannels = sess
		}
	}

	// A channels session that died on its own (crash, reboot) resumes so the
	// conversation context survives. Deliberate restarts set Discard and fall
	// through to a fresh session. ResumeSession also refuses sessions with no
	// saved conversation — fresh start covers those too.
	if deadChannels != nil && !deadChannels.Discard && deadChannels.SessionUUID != "" {
		if _, err := s.sm.ResumeSession(deadChannels.ID); err == nil {
			log.Printf("channels session: resumed %s", deadChannels.ID)
			s.dismissDeadChannelsSessions()
			return
		} else {
			log.Printf("channels session: resume failed (%v); starting fresh", err)
		}
	}

	s.dismissDeadChannelsSessions()
	s.startChannelsSession()
}

func (s *Server) dismissDeadChannelsSessions() {
	for _, sess := range s.sm.GetSessions() {
		if sess.IsChannels && sess.Status == StatusDead {
			s.sm.DismissSession(sess.ID)
		}
	}
}

func (s *Server) startChannelsSession() {
	cfg := s.cfgSnapshot()
	session, err := s.sm.CreateSession(CreateSessionOpts{
		Name:       "channels",
		Dir:        cfg.DefaultDir,
		PermMode:   cfg.DefaultPermissionMode,
		Model:      cfg.DefaultModel,
		Effort:     cfg.DefaultEffort,
		Channels:   cfg.DefaultChannels,
		PluginDirs: cfg.PluginDirs,
		IsChannels: true,
	})
	if err != nil {
		log.Printf("channels session: failed to start: %v", err)
		return
	}
	log.Printf("channels session started: %s (pid %d)", session.ID, session.PID)
}

func (s *Server) restartChannelsSession() {
	for _, sess := range s.sm.GetSessions() {
		if sess.IsChannels && sess.Status != StatusDead {
			// Deliberate restart: flag it so ensureChannelsSession starts fresh
			// instead of resuming the old conversation.
			s.sm.SetDiscard(sess.ID)
			s.sm.KillSession(sess.ID)
		}
	}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.sm.GetSessions())
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string `json:"name"`
		Dir            string `json:"dir"`
		PermissionMode string `json:"permissionMode"`
		Model          string `json:"model"`
		Effort         string `json:"effort"`
		Branch         string `json:"branch"`
		CreateBranch   bool   `json:"createBranch"`
		BranchFrom     string `json:"branchFrom"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	cfg := s.cfgSnapshot()

	dir := req.Dir
	if dir == "" {
		dir = cfg.DefaultDir
	}

	permMode := req.PermissionMode
	if permMode == "" {
		permMode = cfg.DefaultPermissionMode
	}

	model := req.Model
	if model == "" {
		model = cfg.DefaultModel
	}

	effort := req.Effort
	if effort == "" {
		effort = cfg.DefaultEffort
	}

	session, err := s.sm.CreateSession(CreateSessionOpts{
		Name:         req.Name,
		Dir:          dir,
		PermMode:     permMode,
		Model:        model,
		Effort:       effort,
		Branch:       req.Branch,
		CreateBranch: req.CreateBranch,
		BranchFrom:   req.BranchFrom,
	})
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, session)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	type job struct {
		path, group string
	}
	cfg := s.cfgSnapshot()
	var jobs []job
	for _, p := range cfg.FavoriteDirs {
		jobs = append(jobs, job{p, "Favorites"})
	}
	for _, root := range cfg.ProjectRoots {
		for _, child := range ListSubdirs(root) {
			jobs = append(jobs, job{child, root})
		}
	}

	out := make([]ProjectStatus, len(jobs))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, path, group string) {
			defer wg.Done()
			defer func() { <-sem }()
			ps := GetProjectStatusCached(path)
			ps.Group = group
			out[i] = ps
		}(i, j.path, j.group)
	}
	wg.Wait()

	writeJSON(w, out)
}

func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, []string{})
		return
	}
	writeJSON(w, GetBranches(path))
}

func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.sm.mu.RLock()
	sess, ok := s.sm.sessions[id]
	s.sm.mu.RUnlock()
	if ok && sess.IsChannels {
		httpError(w, "channels session cannot be killed — use restart", http.StatusForbidden)
		return
	}

	if err := s.sm.KillSession(id); err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDismissSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.sm.DismissSession(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, err := s.sm.ResumeSession(id)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		httpError(w, err.Error(), code)
		return
	}
	writeJSON(w, session)
}

func (s *Server) handleRestartChannels(w http.ResponseWriter, r *http.Request) {
	if !s.cfgSnapshot().ChannelsEnabled {
		httpError(w, "channels not enabled", http.StatusBadRequest)
		return
	}
	s.restartChannelsSession()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.hub.Register()
	defer s.hub.Unregister(ch)

	data, _ := json.Marshal(s.sm.GetSessions())
	fmt.Fprintf(w, "event: init\ndata: %s\n\n", data)
	flusher.Flush()

	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(event.Data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleLoginState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.lm.State())
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var opts LoginOptions
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
			httpError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	if err := s.lm.Start(opts); err != nil {
		httpError(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, s.lm.State())
}

func (s *Server) handleLoginInput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.lm.SendInput(req.Data); err != nil {
		httpError(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLoginStop(w http.ResponseWriter, r *http.Request) {
	if err := s.lm.Stop(); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgSnapshot()
	writeJSON(w, &cfg)
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var incoming struct {
		DefaultDir            *string  `json:"defaultDir"`
		FavoriteDirs          []string `json:"favoriteDirs"`
		ProjectRoots          []string `json:"projectRoots"`
		DefaultPermissionMode *string  `json:"defaultPermissionMode"`
		DefaultModel          *string  `json:"defaultModel"`
		DefaultEffort         *string  `json:"defaultEffort"`
		DefaultChannels       *string  `json:"defaultChannels"`
		PluginDirs            []string `json:"pluginDirs"`
		ChannelsEnabled       *bool    `json:"channelsEnabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := s.updateConfig(func(c *Config) {
		if incoming.DefaultDir != nil {
			c.DefaultDir = *incoming.DefaultDir
		}
		if incoming.FavoriteDirs != nil {
			c.FavoriteDirs = incoming.FavoriteDirs
		}
		if incoming.ProjectRoots != nil {
			c.ProjectRoots = incoming.ProjectRoots
		}
		if incoming.DefaultPermissionMode != nil {
			c.DefaultPermissionMode = *incoming.DefaultPermissionMode
		}
		if incoming.DefaultModel != nil {
			c.DefaultModel = *incoming.DefaultModel
		}
		if incoming.DefaultEffort != nil {
			c.DefaultEffort = *incoming.DefaultEffort
		}
		if incoming.DefaultChannels != nil {
			c.DefaultChannels = *incoming.DefaultChannels
		}
		if incoming.PluginDirs != nil {
			c.PluginDirs = incoming.PluginDirs
		}
		if incoming.ChannelsEnabled != nil {
			c.ChannelsEnabled = *incoming.ChannelsEnabled
		}
	})
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.ensureChannelsSession()

	cfg := s.cfgSnapshot()
	writeJSON(w, &cfg)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, versionInfo())
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, models.Get())
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
