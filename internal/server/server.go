// Package server is the HTTP front end. A swappable Router hosts either the
// setup-mode mux or the normal-mode App, so the in-process setup→normal
// transition swaps the router without a restart. The App is the normal-mode
// handler: health, login/logout, the (auth-gated) chat API, and the embedded UI.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/utopiafallen/bevin/internal/agent"
	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/email"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/sandbox"
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
	// sandboxBk is the per-user container backend (nil = sandboxing
	// disabled in the config, or unsupported on this platform). Guarded
	// by cfgMu (rebuilt on a config save when the [sandbox] area changes).
	sandboxBk sandbox.Backend
	// oauthMu guards oauthStates: the single-use OAuth flow states.
	oauthMu     sync.Mutex
	oauthStates map[string]time.Time
	// supCancel stops the sidecar supervisor (started by NewNormal).
	supCancel context.CancelFunc
	// diskMon is the per-user disk-usage monitor (background walker +
	// frozen state with hysteresis); the gate + scheduler read its cached
	// frozen state. Nil-safe (a nil monitor never freezes anyone).
	diskMon *diskMonitor
	// diskCancel stops the disk monitor's periodic walk.
	diskCancel context.CancelFunc
	// events is the per-user nudge hub behind GET /api/me/events
	// (multi-device sync); built in routes().
	events *eventHub
	// loginLimiter rate-limits /login (per IP + per handle); lazily built
	// on first use (apps built by hand in tests get the same defaults).
	loginLimitOnce sync.Once
	loginLimiter   *loginLimiter
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
	a.buildSandbox(cfg)
	a.routes()
	// Multi-device sync: every conversation row the agent persists nudges
	// the user's open tabs (the hub's per-user fan-out) so a second tab —
	// desktop + phone — re-pulls the conversation and stays current. The
	// hook must stay non-blocking (broadcast never blocks) and survives
	// Reload (applyConfig reloads this same Agent, not a new one).
	a.ag.NotifyMessage = func(handle, convID string, seq int) {
		a.events.broadcast(handle, event{Type: "message", ConversationID: convID, Seq: seq})
	}
	// A conversation-row change the list must reflect (the rolling purpose,
	// via set_purpose — the purpose renders in place of the title as the
	// list label): the same "conversation" nudge the CRUD handlers fire.
	a.ag.NotifyConversation = func(handle, convID string) {
		a.events.broadcast(handle, event{Type: "conversation", ConversationID: convID})
	}
	// Web push (the PWA stage 2): a task settled into a terminal state
	// (done/blocked — fired from the task runner's settle point,
	// alongside the completion-summary post). Same non-blocking
	// contract as the nudges above; the framing + reason first-line
	// compose in the push fan-out (pushSend).
	a.ag.NotifyTaskTerminal = func(handle, taskTitle, convID, state, summary string) {
		a.pushSend(handle, "Task \""+taskTitle+"\" is "+state, firstLineOf(summary, 120), convID, "task")
	}
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
	// Start the disk-usage monitor: a background walker that tracks each
	// user's storage against the per-user cap (a frozen state with
	// hysteresis). Prime does a synchronous initial scan so the gate has
	// data on the first request; Start runs the periodic walk. The cap is
	// read LIVE from config (applyConfig swaps cfg), so an admin edit
	// applies from the next scan without a restart.
	a.diskMon = newDiskMonitor(dataDir,
		func() int { return a.liveCfg().Sandbox.DiskMB },
		a.userHandles,
	)
	// Wire the two-way link between the disk monitor + the scheduler:
	// (1) the scheduler defers a user's queued work while they're over the
	// cap (a disk-monitored frozen state), and (2) when a user gets back
	// under the cap (unfreezes) the monitor re-dispatches the scheduler so
	// their deferred work runs. The cap is read LIVE from config (an admin
	// edit applies from the next scan without a restart).
	dm := a.diskMon
	a.ag.SetDiskFrozen(func(h string) bool { return dm.Frozen(h) })
	a.diskMon.SetOnUnfreeze(func(string) { a.ag.PokeAll() })
	// Wire the container backend into the file tools (edit_file operates
	// INSIDE the user's container — the shared medium between the user's
	// parallel conversations). The resolver tracks the LIVE backend across
	// config-save rebuilds.
	a.ag.SetContainerBackend(func() sandbox.Backend { return a.sandboxBackend() })
	a.diskMon.Prime()
	diskCtx, diskCancel := context.WithCancel(context.Background())
	a.diskCancel = diskCancel
	a.diskMon.Start(diskCtx)
	return a, nil
}

// ServeHTTP implements http.Handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// Close releases per-user stores.
func (a *App) Close() {
	if a.supCancel != nil {
		a.supCancel()
	}
	if a.diskCancel != nil {
		a.diskCancel()
	}
	a.ag.Close()
}

func (a *App) routes() {
	a.events = newEventHub()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /api/me", a.requireAuth(a.me))
	// The per-user nudge channel (multi-device sync): one long-lived SSE
	// per signed-in tab. A second tab of the same user (desktop + phone)
	// learns a conversation changed and re-pulls it — the channel carries
	// events only, never message content.
	mux.HandleFunc("GET /api/me/events", a.requireAuth(a.userEvents))
	// Web push (the PWA stage 2): per-user subscription management.
	// The subscription IS the delivery gate (there is no "notifications
	// on" flag) — a user is automatically enabled for notifications
	// once subscribed; disabling means unsubscribing this device.
	mux.HandleFunc("GET /api/push/status", a.requireAuth(a.pushStatus))
	mux.HandleFunc("POST /api/push/subscribe", a.requireAuth(a.pushSubscribe))
	mux.HandleFunc("DELETE /api/push/subscribe", a.requireAuth(a.pushSubscribe))
	mux.HandleFunc("GET /api/models", a.requireAuth(a.models))
	// The chat API is the user interaction surface: the admin is not a
	// regular user, so it rejects admin sessions (GET / sends them to the
	// dashboard instead; this also stops a stale user tab from chatting as
	// the admin).
	mux.HandleFunc("POST /api/conversations", a.chatGate(a.diskGate(a.createConversation)))
	mux.HandleFunc("GET /api/conversations", a.chatGate(a.listConversations))
	mux.HandleFunc("GET /api/conversations/{id}", a.chatGate(a.getConversation))
	mux.HandleFunc("PATCH /api/conversations/{id}", a.chatGate(a.updateConversation))
	mux.HandleFunc("DELETE /api/conversations/{id}", a.chatGate(a.deleteConversation))
	mux.HandleFunc("GET /api/conversations/search", a.chatGate(a.searchConversations))
	mux.HandleFunc("GET /api/search", a.chatGate(a.searchAll))
	mux.HandleFunc("GET /api/conversations/{id}/messages", a.chatGate(a.getMessages))
	mux.HandleFunc("POST /api/conversations/{id}/messages", a.chatGate(a.diskGate(a.sendMessage)))
	mux.HandleFunc("POST /api/conversations/{id}/messages/stream", a.chatGate(a.diskGate(a.sendMessageStream)))
	mux.HandleFunc("GET /api/email", a.requireAuth(a.emailAddress))
	mux.HandleFunc("PUT /api/email", a.requireAuth(a.setEmail))
	// The user's profile page: what bevin currently knows about them + their
	// user-level settings (the chat UI's user-name button lands here).
	mux.HandleFunc("GET /api/profile", a.requireAuth(a.userProfile))

	// Per-user sandbox (environment) API: the user's OWN environment
	// (the admin may also use it — the only role that sees the runtime
	// detail, needed to diagnose a lost runtime). envGate: any
	// authenticated role (the handle comes from the session).
	mux.HandleFunc("GET /api/sandbox", a.envGate(a.sandboxStatus))
	mux.HandleFunc("POST /api/sandbox/ensure", a.envGate(a.sandboxEnsure))
	mux.HandleFunc("POST /api/sandbox/exec", a.envGate(a.sandboxExec))
	mux.HandleFunc("POST /api/sandbox/stop", a.envGate(a.sandboxStop))
	mux.HandleFunc("DELETE /api/sandbox", a.envGate(a.sandboxRemove))

	// Workspace management: the user's working folders (created under
	// their data dir, mapped into their environment). NO user-facing run
	// endpoint — the agent manages execution (see workspace.go).
	// chatGate: regular users only (same as everything above).
	mux.HandleFunc("GET /api/workspaces", a.chatGate(a.listWorkspaces))
	mux.HandleFunc("POST /api/workspaces", a.chatGate(a.diskGate(a.createWorkspace)))
	mux.HandleFunc("DELETE /api/workspaces/{id}", a.chatGate(a.deleteWorkspace))
	mux.HandleFunc("GET /api/workspaces/{id}/files", a.chatGate(a.workspaceFiles))
	mux.HandleFunc("POST /api/workspaces/{id}/files", a.chatGate(a.diskGate(a.uploadWorkspaceFile)))
	mux.HandleFunc("GET /api/workspaces/{id}/files/raw", a.chatGate(a.workspaceFileRaw))
	mux.HandleFunc("DELETE /api/workspaces/{id}/files", a.chatGate(a.deleteWorkspaceFile))
	// Disk usage: the user's storage against their cap + the cleanup page.
	// NOT behind diskGate (a frozen user must reach the disk page + the
	// deletes to get back under the cap). chatGate: regular users only.
	mux.HandleFunc("GET /disk", a.diskPage)
	mux.HandleFunc("GET /api/disk", a.chatGate(a.diskStatus))
	mux.HandleFunc("POST /api/disk/done", a.chatGate(a.diskDone))
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
	// Web-push VAPID key generation (the PWA stage 2): the server
	// generates the ES256 pair and stores it in [push]; the response
	// carries the public key only (the private key never leaves the
	// server — the masked-secret pattern).
	mux.HandleFunc("POST /admin/api/push/keys", a.requireAdmin(a.adminPushKeys))
	// Admin config pages (the setup wizard is first-time only; in normal mode
	// the admin edits the live config in dedicated pages that reuse the
	// wizard's form components): the gated entry points + the config APIs
	// (read / save + the model Detect probe).
	mux.HandleFunc("GET /admin/models", a.adminModelsPage)
	mux.HandleFunc("GET /admin/email", a.adminEmailPage)
	mux.HandleFunc("GET /admin/users", a.adminUsersPage)
	mux.HandleFunc("GET /admin/api/config", a.requireAdmin(a.adminConfig))
	mux.HandleFunc("POST /admin/api/config", a.requireAdmin(a.adminConfigSave))
	mux.HandleFunc("GET /admin/api/profiles", a.requireAdmin(a.adminProfiles))
	mux.HandleFunc("POST /admin/api/probe-model", a.requireAdmin(a.adminProbeModel))
	// Users panel: the full registry (both IdPs) + admin user management
	// (add an internal account; enable/disable any account).
	mux.HandleFunc("GET /admin/api/users", a.requireAdmin(a.adminUsersList))
	mux.HandleFunc("POST /admin/api/users", a.requireAdmin(a.adminUsersAdd))
	mux.HandleFunc("POST /admin/api/users/status", a.requireAdmin(a.adminUsersStatus))
	// Containers page: the live per-environment health (state + CPU/RAM/disk
	// usage) + the restart-to-apply action. The per-container resource caps
	// live on this page and are saved via the shared config endpoint.
	mux.HandleFunc("GET /admin/api/containers", a.requireAdmin(a.adminContainers))
	mux.HandleFunc("POST /admin/api/containers/restart", a.requireAdmin(a.adminContainersRestart))
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
	var err error
	if a.ag != nil { // a bare test fixture has no agent — there is nothing to reload
		err = a.ag.Reload(cfg)
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.mail = email.New(cfg.Email)
	a.cfgMu.Unlock()
	// Rebuild the sandbox backend with the new config (the sandbox is
	// always enabled — its values, if changed, apply here).
	a.buildSandbox(cfg)
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

// loginLimiterFor lazily builds the app's login rate limiter (defaults) so
// apps built by hand (tests) are throttled too.
func (a *App) loginLimiterFor() *loginLimiter {
	a.loginLimitOnce.Do(func() {
		a.loginLimiter = newLoginLimiter(defaultLoginLimit, defaultLoginWindow)
	})
	return a.loginLimiter
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
	// Rate limit BEFORE the credential check: the caps bound both an
	// attacker (per IP) and a target account (per handle).
	limit := a.loginLimiterFor()
	if ok, retry := limit.attempt(clientIP(r), req.Handle); !ok {
		log.Printf("login: throttled handle %s from %s", req.Handle, clientIP(r))
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login attempts — try again later"})
		return
	}
	u, token, err := a.prov.Authenticate(r.Context(), req.Handle, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	// A success clears the handle's failure counter (legitimate typos must
	// not accumulate into a lockout).
	limit.reset(req.Handle)
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

// me is the client's boot identity check: is this session valid, and who is
// it. The client renders the app from this (handle, display name, role —
// the role gates the admin rail item) and drops to the login card on 401.
// default_model_id is the user's per-user default (agent.toml, set in
// onboarding): the chat's new-conversation preview preselects it exactly the
// way the create endpoint resolves it ("" = none set — the preview falls
// back to the /api/models "default" field).
func (a *App) me(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	defaultModel := ""
	if ac, err := config.LoadAgent(filepath.Join(a.dataDir, "users", u.Handle)); err == nil {
		defaultModel = ac.DefaultModelID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"handle":           u.Handle,
		"display_name":     u.DisplayName,
		"role":             u.Role,
		"default_model_id": defaultModel,
	})
}

// models is the user's model picker data: one row per DISPLAY NAME
// (deduped — a display name maps one-to-many to model providers, and the
// picker surfaces each display name once, with the POOLED slot count: the
// sum of the slots of every available provider serving it). The "default"
// field is the server default's display name — the client's new-conversation
// preview matches it (and the per-user default) against the rows' ids.
func (a *App) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"models":  a.ag.DisplayModels(),
		"default": a.ag.DefaultModelDisplayName(),
		// vision: the live config has a vision-capable aux model (the chat
		// UI's image-attachment controls render from it — attachments are
		// only useful while image_analyze is on the wire).
		"vision": a.ag.HasVisionAux(),
	})
}

// chatImage is one image attachment in a chat message body (base64 inline —
// the client contract, no upload endpoint: the bytes ride the message).
type chatImage struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64 (the client strips any data: prefix)
}

// The chat message body's attachment limits (the client contract): up to
// maxImagesPerMsg images of up to maxImageBytes each AFTER base64 decoding
// (4 × 8 MiB ≈ 43 MiB of base64 + JSON), so the request-body cap sits a
// little above that. Text-only messages are unaffected by the bigger cap.
const (
	maxChatBodyBytes = 48 << 20
	maxImagesPerMsg  = 4
	maxImageBytes    = 8 << 20
)

// errImageTooLarge marks an over-limit attachment (a 413 — payload too
// large — rather than a 400 malformed).
var errImageTooLarge = errors.New("image over the size limit")

// decodeChatImages validates + decodes the request's image attachments into
// the agent's in-memory TurnImages (the agent's turn-scoped form — nothing
// is persisted). It enforces the client contract: images only (image/* mime
// type), at most maxImagesPerMsg, at most maxImageBytes each after
// decoding. The error is a 413 (errImageTooLarge) when an attachment is
// over the per-image size, a 400 otherwise.
func decodeChatImages(raw []chatImage) ([]agent.TurnImage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxImagesPerMsg {
		return nil, fmt.Errorf("at most %d images per message (got %d)", maxImagesPerMsg, len(raw))
	}
	out := make([]agent.TurnImage, 0, len(raw))
	for i, im := range raw {
		if !strings.HasPrefix(im.MimeType, "image/") {
			return nil, fmt.Errorf("attachment %d is not an image (mime type %q)", i+1, im.MimeType)
		}
		data, err := base64.StdEncoding.DecodeString(im.Data)
		if err != nil {
			return nil, fmt.Errorf("attachment %d is not valid base64: %v", i+1, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("attachment %d is empty", i+1)
		}
		if len(data) > maxImageBytes {
			return nil, fmt.Errorf("%w: attachment %d is %d bytes, the limit is %d MiB", errImageTooLarge, i+1, len(data), maxImageBytes>>20)
		}
		out = append(out, agent.TurnImage{Name: im.Name, MimeType: im.MimeType, Data: data})
	}
	return out, nil
}

// createConversation is the chat's "new conversation" + first message: the
// conversation row is created HERE (title = the first message's opening
// words — the user has not seen a conversation before this moment), and the
// send streams into it. The optional model_id + reasoning_effort pin the
// choice the UI's new-conversation preview showed before the first send:
// model_id "" = the user's preferred / server default; reasoning_effort ""
// = the model's profile default. A pinned model must be available; a pinned
// effort must be one of the (possibly resolved) model's profile levels —
// both are validated BEFORE anything is written, so a rejection creates
// nothing.
func (a *App) createConversation(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	var req struct {
		Title           string `json:"title"`
		ModelID         string `json:"model_id"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ModelID != "" && !a.ag.ModelAvailable(req.ModelID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model not available"})
		return
	}
	if req.ReasoningEffort != "" {
		// The effort is validated against the model the conversation will
		// actually take: the pinned one, else the same resolution
		// newUserConversation applies (user's preferred, then server
		// default).
		target := req.ModelID
		if target == "" {
			target = a.ag.NewConversationModel(u.Handle)
		}
		efforts := a.ag.ReasoningEffortsFor(target)
		if len(efforts) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this model takes no reasoning effort"})
			return
		}
		supported := false
		for _, e := range efforts {
			if e == req.ReasoningEffort {
				supported = true
				break
			}
		}
		if !supported {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unsupported reasoning effort %q (supported: %s)", req.ReasoningEffort, strings.Join(efforts, ", "))})
			return
		}
	}
	id, err := a.ag.CreateConversation(r.Context(), u.Handle, req.Title, req.ModelID, req.ReasoningEffort)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Nudge the user's other open tabs: the conversation list changed.
	a.events.broadcast(u.Handle, event{Type: "conversation", ConversationID: id, Action: "created"})
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

// getConversation returns one conversation's slim row + its context-window
// usage (the UI reads it to show the conversation's active model + switch
// it, and to seed the context usage visualization above the input).
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
	ctxUsage, ctxErr := a.ag.ContextUsage(u.Handle, r.PathValue("id"))
	if ctxErr != nil {
		ctxUsage = map[string]any{"tokens": 0, "max_tokens": 0}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": summary, "context": ctxUsage})
}

// updateConversation changes a conversation's model, title, and/or
// reasoning-effort choice (at least one required). The model must be available
// right now; the effort must be one of the (possibly new) model's profile
// levels. Both take effect on the next turn (an in-flight turn keeps its
// model + effort).
func (a *App) updateConversation(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	var req struct {
		ModelID         *string `json:"model_id"`
		Title           *string `json:"title"`
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.ModelID == nil && req.Title == nil && req.ReasoningEffort == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide model_id and/or title and/or reasoning_effort"})
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
	// The effort is validated against the model it will run under: the NEW
	// model when this call also switches models, else the conversation's
	// current one. A non-empty effort the model's profile doesn't support is
	// rejected (the request builder would drop it anyway — failing here
	// names the valid levels instead of silently ignoring the user's choice).
	if req.ReasoningEffort != nil && *req.ReasoningEffort != "" {
		target := ""
		if req.ModelID != nil && *req.ModelID != "" {
			target = *req.ModelID
		} else {
			if cur, err := a.ag.Conversation(u.Handle, r.PathValue("id")); err == nil {
				target = cur.ActiveModelID
			}
		}
		if target == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no model to validate the reasoning effort against"})
			return
		}
		efforts := a.ag.ReasoningEffortsFor(target)
		ok := false
		for _, e := range efforts {
			if e == *req.ReasoningEffort {
				ok = true
				break
			}
		}
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unsupported reasoning effort %q for this model (supported: %s)", *req.ReasoningEffort, strings.Join(efforts, ", "))})
			return
		}
	}
	summary, err := a.ag.UpdateConversation(u.Handle, r.PathValue("id"), req.ModelID, req.Title, req.ReasoningEffort)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Nudge the user's other open tabs: the conversation list changed.
	a.events.broadcast(u.Handle, event{Type: "conversation", ConversationID: r.PathValue("id"), Action: "updated"})
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
	// Nudge the user's other open tabs: the conversation list changed.
	a.events.broadcast(u.Handle, event{Type: "conversation", ConversationID: r.PathValue("id"), Action: "deleted"})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// getMessages returns the conversation's displayable history (user +
// assistant messages, oldest first) so a reloaded page can restore it. It
// supports lazy loading: ?limit=N returns the most recent N messages (the
// chat log's initial page), and ?before=SEQ narrows to the messages OLDER
// than that seq (the scroll-up cursor — the client prepends each page and
// scrolls up to load the rest). Omitting limit returns the full history
// (backward compatible); has_more reports whether older pages exist.
func (a *App) getMessages(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	before := 0
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			before = n
		}
	}
	var (
		msgs    []agent.ConversationMessage
		hasMore bool
		err     error
	)
	if limit > 0 {
		msgs, hasMore, err = a.ag.ConversationMessagesPaged(u.Handle, id, limit, before)
	} else {
		msgs, err = a.ag.ConversationMessages(u.Handle, id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "has_more": hasMore})
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

// searchAll backs the dedicated search page: it searches the user's data
// across every source (conversations semantically + memories + profile
// lexically) in one query. scope is the workspace id to limit file hits to
// ("" = the default: all of the user's data). q is required; limit defaults
// to 10 (max 25).
func (a *App) searchAll(w http.ResponseWriter, r *http.Request) {
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
	out, err := a.ag.SearchAll(r.Context(), u.Handle, q, r.URL.Query().Get("workspace"), limit)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	var req struct {
		Message string      `json:"message"`
		Images  []chatImage `json:"images"` // image attachments (vision aux only)
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Message == "" && len(req.Images) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a message or an image attachment is required"})
		return
	}
	images, err := decodeChatImages(req.Images)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errImageTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	// The turn runs on a context DETACHED from the request, exactly like
	// the streaming path: a caller that walks away (or a client that
	// drops) must not cancel in-flight work — it completes and is
	// persisted regardless. ("Conversations run detached" is uniform
	// across both chat paths; this one simply owes no push — its caller
	// holds the reply synchronously.)
	turnCtx := context.WithoutCancel(r.Context())
	convID, reply, err := a.ag.Turn(turnCtx, u.Handle, id, req.Message, images)
	if err != nil {
		log.Printf("turn: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation_id": convID, "reply": reply})
}

// sendMessageStream streams a chat turn as SSE (text/event-stream). Each text
// delta is a "message" event (JSON {delta, reasoning}); each tool call the
// model issues is a "tool" event ({name}) with a matching "tool_done"
// ({name, success}) once it finishes (the UI renders one indicator row per
// call — the model's next reply lands below the rows); when the stream
// completes a "done" event carries the conversation id, the full reply, and
// the API-reported usage (from the final SSE chunk); a failure sends an
// "error" event. A client disconnect does NOT cancel the turn: the turn runs
// on a context detached from the request, so in-flight work (a tool loop the
// user walked away from) completes and is persisted even when the viewer
// goes — the SSE stream is just a live view of it, and the emit stops writing
// to a gone client instead of aborting the turn. The non-streaming
// sendMessage remains for clients that don't want incremental updates.
func (a *App) sendMessageStream(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	var req struct {
		Message string      `json:"message"`
		Images  []chatImage `json:"images"` // image attachments (vision aux only)
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Message == "" && len(req.Images) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a message or an image attachment is required"})
		return
	}
	images, err := decodeChatImages(req.Images)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errImageTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
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

	writeEvent := sseEventWriter(w, flusher)

	// The emit callback is called synchronously (same goroutine as TurnStream),
	// so capturing the final chunk's usage here is race-free.
	var lastUsage *provider.Usage
	// Turn timing for the perf stats the UI prints under the reply: total
	// elapsed (start → stream end) + the time to the FIRST content delta
	// (reasoning or text — the "thinking" phase counts; a model that thinks
	// a long time before its first token shows it honestly).
	start := time.Now()
	var ttft time.Duration
	// The turn runs on a context DETACHED from the request: a client
	// disconnect (the user navigating away mid-turn, or closing the tab)
	// must not cancel the in-flight work — it completes and is persisted
	// regardless. The SSE stream is just a live view of it; the emit below
	// stops writing to a gone client (and swallows write errors) so the
	// turn never aborts because its viewer left.
	turnCtx := context.WithoutCancel(r.Context())
	// Multi-device sync: snapshot the newest persisted row BEFORE the turn
	// so the done event can carry the seqs of the rows THIS turn persisted
	// (the client tags its optimistically rendered rows with them — the
	// store is the source of truth, the tags stop a sync pull rendering
	// them twice). A snapshot failure degrades to an empty tag list (the
	// pull then reconciles the usual way).
	preSeq := 0
	if us, err := a.ag.UserStoreFor(u.Handle); err == nil {
		if n, err := us.MaxDisplaySeq(id); err == nil {
			preSeq = n
		}
	}
	convID, reply, err := a.ag.TurnStream(turnCtx, u.Handle, id, req.Message, images, func(c provider.Chunk) error {
		if c.Usage != nil {
			lastUsage = c.Usage
		}
		// The client went away (navigated / closed the tab): the turn
		// continues (its context is detached), but there is no viewer for
		// the events — stop writing.
		select {
		case <-r.Context().Done():
			return nil
		default:
		}
		// Live context usage: each model call's final chunk carries the
		// API's usage for THAT call (the OAI wire sets include_usage). The
		// UI updates the context bar on every one — during a long tool loop
		// the bar tracks the context filling, not just the previous turn's
		// settled value. (It fires once per loop iteration, at the end of
		// each call's stream.)
		if c.Usage != nil {
			_ = writeEvent("usage", map[string]any{
				"prompt_tokens":     c.Usage.PromptTokens,
				"completion_tokens": c.Usage.CompletionTokens,
				"total_tokens":      c.Usage.TotalTokens,
			})
		}
		// Context compaction: the harness emits a "start" indicator while
		// the summarizer runs, "reasoning" deltas as the summarizer thinks
		// (the UI streams them into the compaction row's thinking block,
		// same behavior as a regular message), and a "done" (with the
		// before/after token sizes + the compacted context) once the summary
		// is stored. The UI shows an animated "Compacting context…" row,
		// then settles it into a persistent "Context compacted from X to Y
		// tokens" marker with the compacted context in a collapsed block.
		if c.CompactionPhase != "" {
			_ = writeEvent("compaction", map[string]any{
				"phase":     c.CompactionPhase,
				"from":      c.CompactedFrom,
				"to":        c.CompactedTo,
				"reasoning": c.ReasoningDelta,   // non-empty only for phase "reasoning"
				"summary":   c.CompactedSummary, // non-empty only for phase "done"
			})
			// A "reasoning" chunk carries a ReasoningDelta — without this
			// return it would fall through to the message handler and be
			// appended to the turn's reply (the compaction's thinking is not
			// the reply).
			return nil
		}
		if c.ToolName != "" {
			// Harness-level tool-call indicators (not model deltas): the
			// UI renders one row per issued call — the model's next reply
			// (processing the results) lands below the rows. A write
			// failure (the client just disconnected) is swallowed — it must
			// never abort the in-flight turn.
			if c.ToolDone {
				_ = writeEvent("tool_done", map[string]any{"name": c.ToolName, "success": c.ToolSuccess})
			} else {
				_ = writeEvent("tool", map[string]any{"name": c.ToolName})
			}
			return nil
		}
		if c.TextDelta != "" || c.ReasoningDelta != "" {
			if ttft == 0 {
				ttft = time.Since(start)
			}
			_ = writeEvent("message", map[string]any{"delta": c.TextDelta, "reasoning": c.ReasoningDelta})
			return nil
		}
		return nil
	})
	if err != nil {
		log.Printf("turn (stream %s): %v", u.Handle, err)
		_ = writeEvent("error", map[string]string{"error": err.Error()})
		// Web push (the PWA stage 2): an errored turn notifies too —
		// the user must know the agent could not finish. convID is
		// the resolved conversation (a new chat created it before
		// failing, when it got that far); a push is a no-op when the
		// conversation is gone.
		a.pushTurnFailed(u.Handle, convID, err)
		return
	}
	elapsed := time.Since(start)
	var usage map[string]any
	if lastUsage != nil {
		usage = map[string]any{
			"prompt_tokens":     lastUsage.PromptTokens,
			"completion_tokens": lastUsage.CompletionTokens,
			"total_tokens":      lastUsage.TotalTokens,
		}
	}
	// Multi-device sync: the displayable rows this turn persisted (user
	// first, then the assistant rows in display order) — the client tags
	// its optimistically rendered rows with these seqs.
	var seqs []int
	if us, err := a.ag.UserStoreFor(u.Handle); err == nil {
		if s, err := us.DisplaySeqsAfter(convID, preSeq); err == nil {
			seqs = s
		}
	}
	if seqs == nil {
		seqs = []int{}
	}
	_ = writeEvent("done", map[string]any{
		"conversation_id": convID,
		"reply":           reply,
		"usage":           usage,
		"elapsed_ms":      elapsed.Milliseconds(),
		"ttft_ms":         ttft.Milliseconds(),
		"seqs":            seqs,
	})
	// Web push (the PWA stage 2): the turn COMPLETION is the primary
	// notification event — fired here (the settle point) on BOTH
	// outcomes (the error path fires above). Fire-and-forget: it never
	// delays the response, and it is a no-op when push is off or the
	// user has no subscribed devices.
	a.pushTurnDone(u.Handle, convID, reply)
}

// notify sends a notification to a user (by handle) over every
// configured transport: web push (the PWA stage 2 — the subscription
// list is the gate; a no-op when push is unconfigured or the user has
// no subscribed devices) + email. It is fire-and-forget: a
// notification must never block or break the calling path. Each
// transport is independently a no-op when unconfigured (the send
// failure is logged, not returned).
func (a *App) notify(handle, subject, body string) {
	// System notifications (container restart, disk unfreeze) ride the
	// push fan-out as a second transport on the SAME call — zero new
	// call sites. convID is empty: a system notification deep-links the
	// app root.
	a.pushSend(handle, subject, body, "", "system")
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
	if req.Email != "" && !email.ValidAddress(req.Email) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email address is not valid"})
		return
	}
	if err := a.srv.SetUserEmail(target, req.Email); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	log.Printf("email: %s address set by %s", target, u.Handle)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": req.Email})
}

// userProfile is the user's profile page data: what bevin currently knows
// about them (their stored profile — the durable KVs injected into the
// system prompt, so this is literally the profile bevin has of the user) +
// their email + the learned facts (active memories) + the persona list.
// Personas are a STUB for now (the persona editor ships later); the shape
// exists so the profile page renders its full layout from day one (empty
// states, not missing sections).
func (a *App) userProfile(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	prof, facts, personas, err := a.ag.UserProfileData(u.Handle)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"handle":       u.Handle,
		"display_name": u.DisplayName,
		"email":        u.Email,
		"profile":      prof,     // bevin's profile of the user (key: value)
		"facts":        facts,    // bevin's learned notes (active memories)
		"personas":     personas, // the persona editor ships later
	})
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
