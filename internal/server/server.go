// Package server is the HTTP front end. A swappable Router hosts either the
// setup-mode mux or the normal-mode App, so the in-process setup→normal
// transition swaps the router without a restart. The App is the normal-mode
// handler: health, login/logout, the (auth-gated) chat API, and the embedded UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/utopiafallen/bevin/internal/agent"
	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/email"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/setup"
	"github.com/utopiafallen/bevin/internal/store"
	"github.com/utopiafallen/bevin/web"
)

// Router is a swappable http.Handler for the in-process setup→normal transition.
type Router struct {
	mu      sync.RWMutex
	current http.Handler
}

// ServeHTTP delegates to the current handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	h := r.current
	r.mu.RUnlock()
	if h == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, req)
}

// Swap atomically replaces the current handler.
func (r *Router) Swap(h http.Handler) {
	r.mu.Lock()
	r.current = h
	r.mu.Unlock()
}

// App is the normal-mode HTTP app.
type App struct {
	cfg     *config.ServerConfig
	prov    auth.Provider
	ag      *agent.Agent
	mux     *http.ServeMux
	ttl     time.Duration
	dataDir string
	edit    *setup.Manager
	srv     *store.ServerStore
	// mail is the SMTP sender (nil when email is not configured; notify is a
	// no-op in that case).
	mail *email.Sender
	// cfgMu guards cfg + mail: a config save (the admin config pages) swaps
	// both in under the lock so readers never see a torn state.
	cfgMu sync.RWMutex
	// oauthMu guards oauthStates: the single-use OAuth flow states.
	oauthMu     sync.Mutex
	oauthStates map[string]time.Time
	// supCancel stops the sidecar supervisor (started by NewNormal).
	supCancel context.CancelFunc
}

// NewNormal builds the normal-mode app from a loaded config.
func NewNormal(dataDir string, srv *store.ServerStore, prov auth.Provider) (*App, error) {
	cfg, err := config.Load(dataDir)
	if err != nil {
		return nil, err
	}
	ag, err := agent.New(cfg, dataDir)
	if err != nil {
		return nil, err
	}
	edit := setup.New(dataDir, cfg.Bind, 0, srv, prov, nil)
	// Build the SMTP sender (nil when email is not configured; notify is a
	// no-op in that case). A config save (adminConfigSave) swaps cfg + mail +
	// hot-reloads the agent via applyConfig.
	mail := email.New(cfg.Email)
	a := &App{cfg: cfg, prov: prov, ag: ag, ttl: time.Duration(cfg.Auth.SessionTTLSeconds) * time.Second, dataDir: dataDir, edit: edit, srv: srv, mail: mail}
	a.routes()
	// Start the sidecar supervisor: it ensures the loopback sidecars (Qdrant,
	// the llama-server embedder, ffmpeg) are up at boot (re-launching any that
	// are down after a bevin restart or machine reboot) and respawns any that
	// die mid-session. It runs in a background goroutine + is stopped by
	// App.Close. It never blocks the app from coming up (a slow model load
	// happens in the background; the agent degrades until the sidecar is up).
	sm := setup.NewSidecarManager(nil, nil)
	supCtx, supCancel := context.WithCancel(context.Background())
	a.supCancel = supCancel
	go sm.Supervise(supCtx, cfg)
	return a, nil
}

// ServeHTTP implements http.Handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// Close releases per-user stores.
func (a *App) Close() {
	if a.supCancel != nil {
		a.supCancel()
	}
	a.ag.Close()
}

func (a *App) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /api/me", a.requireAuth(a.me))
	mux.HandleFunc("GET /api/models", a.requireAuth(a.models))
	// The chat API is the user interaction surface: the admin is not a
	// regular user, so it rejects admin sessions (GET / sends them to the
	// dashboard instead; this also stops a stale user tab from chatting as
	// the admin).
	mux.HandleFunc("POST /api/conversations", a.chatGate(a.createConversation))
	mux.HandleFunc("GET /api/conversations", a.chatGate(a.listConversations))
	mux.HandleFunc("GET /api/conversations/{id}", a.chatGate(a.getConversation))
	mux.HandleFunc("PATCH /api/conversations/{id}", a.chatGate(a.updateConversation))
	mux.HandleFunc("DELETE /api/conversations/{id}", a.chatGate(a.deleteConversation))
	mux.HandleFunc("GET /api/conversations/search", a.chatGate(a.searchConversations))
	mux.HandleFunc("GET /api/conversations/{id}/messages", a.chatGate(a.getMessages))
	mux.HandleFunc("POST /api/conversations/{id}/messages", a.chatGate(a.sendMessage))
	mux.HandleFunc("POST /api/conversations/{id}/messages/stream", a.chatGate(a.sendMessageStream))
	mux.HandleFunc("GET /api/email", a.requireAuth(a.emailAddress))
	mux.HandleFunc("PUT /api/email", a.requireAuth(a.setEmail))
	// First-time onboarding (regular users only — the admin has no user UI).
	mux.HandleFunc("GET /api/onboarding", a.chatGate(a.onboardingStatus))
	mux.HandleFunc("POST /api/onboarding", a.chatGate(a.onboardingComplete))
	// External-IdP (OAuth) login: public routes (they run BEFORE a bevin
	// session exists) — the status probe for the login page + the
	// authorization-code flow (login redirect + provider callback).
	mux.HandleFunc("GET /api/oauth/status", a.apiOAuthStatus)
	mux.HandleFunc("GET /auth/oauth/login", a.oauthLogin)
	mux.HandleFunc("GET /auth/oauth/callback", a.oauthCallback)
	// Admin dashboard (admin role only): the page entry + the live-state APIs
	// it polls (slot pools + queue, sidecar health, email config + test send).
	mux.HandleFunc("GET /admin", a.adminPage)
	mux.HandleFunc("GET /admin/api/status", a.requireAdmin(a.adminStatus))
	mux.HandleFunc("GET /admin/api/sidecars", a.requireAdmin(a.adminSidecars))
	mux.HandleFunc("GET /admin/api/email", a.requireAdmin(a.adminEmail))
	mux.HandleFunc("POST /admin/api/email/test", a.requireAdmin(a.adminEmailTest))
	// Admin config pages (the setup wizard is first-time only; in normal mode
	// the admin edits the live config in dedicated pages that reuse the
	// wizard's form components): the gated entry points + the config APIs
	// (read / save + the model Detect probe).
	mux.HandleFunc("GET /admin/models", a.adminModelsPage)
	mux.HandleFunc("GET /admin/email", a.adminEmailPage)
	mux.HandleFunc("GET /admin/users", a.adminUsersPage)
	mux.HandleFunc("GET /admin/api/config", a.requireAdmin(a.adminConfig))
	mux.HandleFunc("POST /admin/api/config", a.requireAdmin(a.adminConfigSave))
	mux.HandleFunc("POST /admin/api/probe-model", a.requireAdmin(a.adminProbeModel))
	// Users panel: the full registry (both IdPs) + admin user management
	// (add an internal account; enable/disable any account).
	mux.HandleFunc("GET /admin/api/users", a.requireAdmin(a.adminUsersList))
	mux.HandleFunc("POST /admin/api/users", a.requireAdmin(a.adminUsersAdd))
	mux.HandleFunc("POST /admin/api/users/status", a.requireAdmin(a.adminUsersStatus))
	if sub, err := fs.Sub(web.FS, "static"); err == nil {
		mux.Handle("GET /", a.rootHandler(sub))
	}
	a.mux = mux
}

// rootHandler serves the web root. The index document is the user chat UI —
// the admin is not a regular user (their entry is the dashboard), so an
// authenticated admin requesting the index is redirected to /admin. Asset
// requests pass through untouched (the dashboard pages load their shared
// assets under the same admin session).
func (a *App) rootHandler(sub fs.FS) http.Handler {
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			if u, ok := a.sessionUser(r); ok && u.Role == "admin" {
				http.Redirect(w, r, "/admin", http.StatusSeeOther)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// liveCfg / liveMail read the app's live config + SMTP sender under the lock
// (a config save swaps them in via applyConfig).
func (a *App) liveCfg() *config.ServerConfig {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *App) liveMail() *email.Sender {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.mail
}

// applyConfig applies a freshly saved config without a restart: hot-reload the
// running agent (models, slot pools, provider endpoints) + swap the app's live
// config + SMTP sender. It reports whether the agent reload succeeded — when
// it fails the config is still saved on disk (the agent keeps the old one
// until the next restart).
func (a *App) applyConfig(cfg *config.ServerConfig) (reloaded bool) {
	err := a.ag.Reload(cfg)
	a.cfgMu.Lock()
	a.cfg = cfg
	a.mail = email.New(cfg.Email)
	a.cfgMu.Unlock()
	if err != nil {
		log.Printf("bevin: agent reload after config save: %v", err)
		return false
	}
	return true
}

// sessionUser resolves the request's session to the user's account row
// (empty on failure).
func (a *App) sessionUser(r *http.Request) (*store.User, bool) {
	tok, err := auth.SessionToken(r)
	if err != nil {
		return nil, false
	}
	u, err := a.prov.UserFromToken(r.Context(), tok)
	if err != nil {
		return nil, false
	}
	return u, true
}

// adminGate authenticates the request and returns the user's role (empty on
// failure).
func (a *App) adminGate(r *http.Request) (string, bool) {
	u, ok := a.sessionUser(r)
	if !ok {
		return "", false
	}
	return u.Role, true
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle   string `json:"handle"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	u, token, err := a.prov.Authenticate(r.Context(), req.Handle, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	log.Printf("login: %s (%s)", u.Handle, clientIP(r))
	auth.SetSessionCookie(w, token, a.ttl)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "handle": u.Handle, "role": u.Role})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if tok, err := auth.SessionToken(r); err == nil {
		_ = a.prov.Logout(r.Context(), tok)
	}
	auth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, err := auth.SessionToken(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		u, err := a.prov.UserFromToken(r.Context(), tok)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		next(w, r.WithContext(auth.ContextWithUser(r.Context(), u)))
	}
}

// chatGate is requireAuth + a non-admin check: the chat endpoints are the
// user interaction surface and the admin does not participate as a regular
// user (GET / redirects them to the dashboard).
func (a *App) chatGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, err := auth.SessionToken(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		u, err := a.prov.UserFromToken(r.Context(), tok)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		if u.Role == "admin" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin does not participate as a regular user — use the dashboard"})
			return
		}
		next(w, r.WithContext(auth.ContextWithUser(r.Context(), u)))
	}
}

func (a *App) me(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"handle": u.Handle, "display_name": u.DisplayName, "role": u.Role})
}

func (a *App) models(w http.ResponseWriter, _ *http.Request) {
	type pubModel struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		ModelID     string `json:"model_id"`
		MaxContext  int    `json:"max_context"`
		Slots       int    `json:"slots"`
		Reasoning   int    `json:"reasoning"`
		Knowledge   int    `json:"knowledge"`
		Speed       int    `json:"speed"`
	}
	out := []pubModel{}
	for _, m := range a.ag.Models() {
		out = append(out, pubModel{m.ID, m.DisplayName, m.ModelID, m.MaxContext, m.Slots, m.Metadata.Reasoning, m.Metadata.Knowledge, m.Metadata.Speed})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "default": a.ag.DefaultModelID()})
}

func (a *App) createConversation(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	var req struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	id, err := a.ag.EnsureConversation(r.Context(), u.Handle, "", req.Title)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// listConversations returns the user's conversations, most recently active
// first (the slim list form — the UI's history panel).
func (a *App) listConversations(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	convos, err := a.ag.Conversations(u.Handle)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": convos})
}

// getConversation returns one conversation's slim row (the UI reads it to
// show the conversation's active model + switch it).
func (a *App) getConversation(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	summary, err := a.ag.Conversation(u.Handle, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": summary})
}

// updateConversation changes a conversation's model and/or title
// (at least one required). The model must be available right now; the
// switch takes effect on the next turn (an in-flight turn keeps its model).
func (a *App) updateConversation(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	var req struct {
		ModelID *string `json:"model_id"`
		Title   *string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.ModelID == nil && req.Title == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide model_id and/or title"})
		return
	}
	if req.ModelID != nil && !a.ag.ModelAvailable(*req.ModelID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model not available"})
		return
	}
	if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title must not be blank"})
		return
	}
	summary, err := a.ag.UpdateConversation(u.Handle, r.PathValue("id"), req.ModelID, req.Title)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": summary})
}

// deleteConversation removes the user's conversation (its message rows, the
// FTS index via triggers, and its indexed vectors best-effort).
func (a *App) deleteConversation(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	err := a.ag.DeleteConversation(r.Context(), u.Handle, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// getMessages returns the conversation's displayable history (user +
// assistant messages, oldest first) so a reloaded page can restore it.
func (a *App) getMessages(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	msgs, err := a.ag.ConversationMessages(u.Handle, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// searchConversations semantically searches the user's conversation history
// (their per-user Qdrant collection — the message texts are embedded at
// append time; see agent.vectorIndex). q is required; limit defaults to 10
// (max 25). Results are the matched messages (not whole conversations),
// newest-scored; the UI jumps to the hit's conversation.
func (a *App) searchConversations(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q is required"})
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 25 {
		limit = 25
	}
	hits, err := a.ag.SearchConversations(r.Context(), u.Handle, q, limit)
	if err != nil {
		log.Printf("conversation search: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}

func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	convID, reply, err := a.ag.Turn(r.Context(), u.Handle, id, req.Message)
	if err != nil {
		log.Printf("turn: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation_id": convID, "reply": reply})
}

// sendMessageStream streams a chat turn as SSE (text/event-stream). Each text
// delta is a "message" event (JSON {delta, reasoning}); when the stream
// completes a "done" event carries the conversation id, the full reply, and
// the API-reported usage (from the final SSE chunk); a failure sends an
// "error" event. A client disconnect cancels the request context, aborting
// the upstream stream. The non-streaming sendMessage remains for clients that
// don't want incremental updates.
func (a *App) sendMessageStream(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable reverse-proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeEvent := func(event string, payload any) error {
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// The emit callback is called synchronously (same goroutine as TurnStream),
	// so capturing the final chunk's usage here is race-free.
	var lastUsage *provider.Usage
	convID, reply, err := a.ag.TurnStream(r.Context(), u.Handle, id, req.Message, func(c provider.Chunk) error {
		if c.Usage != nil {
			lastUsage = c.Usage
		}
		if c.TextDelta != "" || c.ReasoningDelta != "" {
			return writeEvent("message", map[string]any{"delta": c.TextDelta, "reasoning": c.ReasoningDelta})
		}
		return nil
	})
	if err != nil {
		log.Printf("turn (stream %s): %v", u.Handle, err)
		_ = writeEvent("error", map[string]string{"error": err.Error()})
		return
	}
	var usage map[string]any
	if lastUsage != nil {
		usage = map[string]any{
			"prompt_tokens":     lastUsage.PromptTokens,
			"completion_tokens": lastUsage.CompletionTokens,
			"total_tokens":      lastUsage.TotalTokens,
		}
	}
	_ = writeEvent("done", map[string]any{"conversation_id": convID, "reply": reply, "usage": usage})
}

// notify sends an email notification to a user (by handle). It is fire-and-
// forget: a notification must never block or break the calling path. It is a
// no-op when email is not configured, the user has no email address, or the
// send fails (the failure is logged, not returned).
func (a *App) notify(handle, subject, body string) {
	if a.liveMail() == nil {
		return
	}
	u, err := a.srv.GetUser(handle)
	if err != nil || u.Email == "" {
		return
	}
	to := u.Email
	m := a.liveMail()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Send(ctx, to, subject, body); err != nil {
			log.Printf("notify %s: %v", u.Handle, err)
		}
	}()
}

// emailAddress reports whether email is configured on the server + the
// current user's email address (so the UI can prompt for it when missing).
func (a *App) emailAddress(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": a.liveMail() != nil,
		"email":      u.Email,
	})
}

// setEmail sets the current user's email address (self-service). An admin may
// set another user's address by passing "handle" in the body.
func (a *App) setEmail(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	var req struct {
		Handle string `json:"handle"`
		Email  string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// A non-admin may only change their own address; an admin may set any.
	target := u.Handle
	if req.Handle != "" && req.Handle != u.Handle {
		if u.Role != "admin" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		target = req.Handle
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email != "" && !strings.Contains(req.Email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is not valid"})
		return
	}
	if err := a.srv.SetUserEmail(target, req.Email); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	log.Printf("email: %s address set by %s", target, u.Handle)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": req.Email})
}

// clientIP reads the client IP from X-Forwarded-For (fallback X-Real-IP), else
// the connection IP. Never r.RemoteAddr alone (a reverse proxy is in front).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
