package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
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
	cfg *Config
	sm  *SessionManager
	lm  *LoginManager
	hub *SSEHub
}

func NewServer(cfg *Config, sm *SessionManager, lm *LoginManager, hub *SSEHub) *Server {
	srv := &Server{cfg: cfg, sm: sm, lm: lm, hub: hub}

	sm.onChannelsDied = func() {
		srv.ensureChannelsSession()
	}

	return srv
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
	mux.HandleFunc("POST /api/channels/restart", s.handleRestartChannels)
	mux.HandleFunc("GET /api/events", s.handleSSE)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handleUpdateConfig)
	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("GET /api/branches", s.handleListBranches)
	mux.HandleFunc("GET /api/login", s.handleLoginState)
	mux.HandleFunc("POST /api/login", s.handleLoginStart)
	mux.HandleFunc("POST /api/login/input", s.handleLoginInput)
	mux.HandleFunc("DELETE /api/login", s.handleLoginStop)

	return http.ListenAndServe(fmt.Sprintf(":%d", s.cfg.Port), mux)
}

func (s *Server) ensureChannelsSession() {
	if !s.cfg.ChannelsEnabled || s.cfg.DefaultChannels == "" {
		return
	}

	for _, sess := range s.sm.GetSessions() {
		if sess.IsChannels && sess.Status != StatusDead {
			return
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
	session, err := s.sm.CreateSession(CreateSessionOpts{
		Name:       "channels",
		Dir:        s.cfg.DefaultDir,
		PermMode:   s.cfg.DefaultPermissionMode,
		Model:      s.cfg.DefaultModel,
		Effort:     s.cfg.DefaultEffort,
		Channels:   s.cfg.DefaultChannels,
		PluginDirs: s.cfg.PluginDirs,
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

	dir := req.Dir
	if dir == "" {
		dir = s.cfg.DefaultDir
	}

	permMode := req.PermissionMode
	if permMode == "" {
		permMode = s.cfg.DefaultPermissionMode
	}

	model := req.Model
	if model == "" {
		model = s.cfg.DefaultModel
	}

	effort := req.Effort
	if effort == "" {
		effort = s.cfg.DefaultEffort
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
	var jobs []job
	for _, p := range s.cfg.FavoriteDirs {
		jobs = append(jobs, job{p, "Favorites"})
	}
	for _, root := range s.cfg.ProjectRoots {
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

func (s *Server) handleRestartChannels(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ChannelsEnabled {
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
	writeJSON(w, s.cfg)
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

	if incoming.DefaultDir != nil {
		s.cfg.DefaultDir = *incoming.DefaultDir
	}
	if incoming.FavoriteDirs != nil {
		s.cfg.FavoriteDirs = incoming.FavoriteDirs
	}
	if incoming.ProjectRoots != nil {
		s.cfg.ProjectRoots = incoming.ProjectRoots
	}
	if incoming.DefaultPermissionMode != nil {
		s.cfg.DefaultPermissionMode = *incoming.DefaultPermissionMode
	}
	if incoming.DefaultModel != nil {
		s.cfg.DefaultModel = *incoming.DefaultModel
	}
	if incoming.DefaultEffort != nil {
		s.cfg.DefaultEffort = *incoming.DefaultEffort
	}
	if incoming.DefaultChannels != nil {
		s.cfg.DefaultChannels = *incoming.DefaultChannels
	}
	if incoming.PluginDirs != nil {
		s.cfg.PluginDirs = incoming.PluginDirs
	}
	if incoming.ChannelsEnabled != nil {
		s.cfg.ChannelsEnabled = *incoming.ChannelsEnabled
	}

	if err := s.cfg.Save(); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.ensureChannelsSession()

	writeJSON(w, s.cfg)
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
