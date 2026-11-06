// Package server is the HTTP front end: embedded web UI, health check,
// and the chat API the UI calls.
package server

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/utopiafallen/alcyona-agent/internal/agent"
	"github.com/utopiafallen/alcyona-agent/internal/store"
	"github.com/utopiafallen/alcyona-agent/web"
)

// Server is the alcyona HTTP handler.
type Server struct {
	ag    *agent.Agent
	store *store.Store
}

// New builds the root HTTP handler.
func New(ag *agent.Agent, s *store.Store) http.Handler {
	srv := &Server{ag: ag, store: s}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", srv.health)
	mux.HandleFunc("POST /api/chat", srv.chat)
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}
	mux.Handle("GET /", http.FileServerFS(static))
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

type chatRequest struct {
	User    string `json:"user"`
	Message string `json:"message"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.User == "" || req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user and message are required"})
		return
	}

	out, err := s.ag.Run(r.Context(), agent.TurnInput{UserHandle: req.User, Message: req.Message})
	if err != nil {
		log.Printf("turn: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// TODO(streaming): replace with SSE or a WebSocket so tokens stream
	// to the UI as they arrive.
	writeJSON(w, http.StatusOK, map[string]string{"reply": out.Reply})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
