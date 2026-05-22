package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
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
	hub *SSEHub
}

func NewServer(cfg *Config, sm *SessionManager, hub *SSEHub) *Server {
	return &Server{cfg: cfg, sm: sm, hub: hub}
}

func (s *Server) Start() error {
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
	mux.HandleFunc("GET /api/events", s.handleSSE)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handleUpdateConfig)

	return http.ListenAndServe(fmt.Sprintf(":%d", s.cfg.Port), mux)
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

	session, err := s.sm.CreateSession(req.Name, dir, permMode, model, effort)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, session)
}

func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.cfg)
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var incoming struct {
		DefaultDir            *string  `json:"defaultDir"`
		FavoriteDirs          []string `json:"favoriteDirs"`
		DefaultPermissionMode *string  `json:"defaultPermissionMode"`
		DefaultModel          *string  `json:"defaultModel"`
		DefaultEffort         *string  `json:"defaultEffort"`
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
	if incoming.DefaultPermissionMode != nil {
		s.cfg.DefaultPermissionMode = *incoming.DefaultPermissionMode
	}
	if incoming.DefaultModel != nil {
		s.cfg.DefaultModel = *incoming.DefaultModel
	}
	if incoming.DefaultEffort != nil {
		s.cfg.DefaultEffort = *incoming.DefaultEffort
	}

	if err := s.cfg.Save(); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
