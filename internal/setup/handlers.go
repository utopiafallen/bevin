package setup

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/email"
	"github.com/utopiafallen/bevin/internal/provider"
)

//go:embed wizard.html
var wizardFS embed.FS

var wizardTmpl = template.Must(template.New("wizard.html").Funcs(template.FuncMap{
	"jsonify": func(v any) template.JS {
		b, _ := json.Marshal(v)
		return template.JS(b)
	},
}).ParseFS(wizardFS, "wizard.html"))

// Step order for the wizard (matches the plan's §10).
var stepOrder = []string{"server", "admin", "models", "aux", "sidecars", "email", "review"}

// Handler serves the setup wizard API. Every /setup route is gated on the
// one-time setup token. After finalize, the in-process transition callback
// swaps the running router from setup to normal mode (no restart).
type Handler struct {
	mgr        *Manager
	token      string
	transition func()
}

// NewHandler builds the setup handler. token is the one-time setup token;
// transition is called (non-nil) once finalize succeeds.
func NewHandler(mgr *Manager, token string, transition func()) *Handler {
	return &Handler{mgr: mgr, token: token, transition: transition}
}

// Routes registers the setup routes on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/setup", h.page)
	mux.HandleFunc("/setup/state", h.state)
	mux.HandleFunc("/setup/advance", h.advance)
	mux.HandleFunc("/setup/back", h.back)
	mux.HandleFunc("/setup/cancel", h.cancel)
	mux.HandleFunc("/setup/test-model", h.testModel)
	mux.HandleFunc("/setup/test-sidecar", h.testSidecar)
	mux.HandleFunc("/setup/probe-model", h.probeModel)
	mux.HandleFunc("/setup/test-email", h.testEmail)
	mux.HandleFunc("/setup/sidecars/start", h.sidecarsStart)
	mux.HandleFunc("/setup/sidecars/status", h.sidecarsStatus)
	mux.HandleFunc("/setup/finalize", h.finalize)
}

// adminGate is a pluggable auth check for the normal-mode edit routes. It
// returns the authenticated user's role (empty on failure); the caller
// requires "admin".
type adminGate func(r *http.Request) (role string, ok bool)

// EditHandler serves the setup wizard in normal mode (to edit the live config).
// It is gated on an admin session (via the gate function) rather than the
// one-time setup token, and finalizes via FinalizeEdit (no re-seed, no
// secret regeneration).
type EditHandler struct {
	mgr  *Manager
	gate adminGate
}

// NewEditHandler builds the normal-mode edit handler. gate authenticates the
// request and returns the user's role.
func NewEditHandler(mgr *Manager, gate adminGate) *EditHandler {
	return &EditHandler{mgr: mgr, gate: gate}
}

func (h *EditHandler) admin(w http.ResponseWriter, r *http.Request) bool {
	role, ok := h.gate(r)
	if !ok || role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return false
	}
	return true
}

// Routes registers the normal-mode edit routes on mux. The methods are explicit
// (GET for the page + state, POST for the mutations) rather than any-method, so
// they don't conflict with the "GET /" static-asset catch-all the normal-mode
// app registers — Go's ServeMux rejects an any-method route that a method+subtree
// route would shadow.
func (h *EditHandler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/setup", h.page)
	mux.HandleFunc("GET /admin/setup/state", h.state)
	mux.HandleFunc("POST /admin/setup/advance", h.advance)
	mux.HandleFunc("POST /admin/setup/back", h.back)
	mux.HandleFunc("POST /admin/setup/cancel", h.cancel)
	mux.HandleFunc("POST /admin/setup/test-model", h.testModel)
	mux.HandleFunc("POST /admin/setup/probe-model", h.probeModel)
	mux.HandleFunc("POST /admin/setup/test-email", h.testEmail)
	mux.HandleFunc("POST /admin/setup/sidecars/start", h.sidecarsStart)
	mux.HandleFunc("GET /admin/setup/sidecars/status", h.sidecarsStatus)
	mux.HandleFunc("POST /admin/setup/finalize", h.finalize)
}

func (h *EditHandler) page(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if err := h.mgr.LoadFromConfig(); err != nil {
		http.Error(w, "failed to load config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = wizardTmpl.Execute(w, map[string]any{"DataDir": h.mgr.dataDir, "Server": map[string]any{"bind": h.mgr.bind, "port": h.mgr.port, "dataDir": h.mgr.dataDir}, "Steps": stepOrder, "Edit": true})
}

func (h *EditHandler) state(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.mgr.LoadFromConfig(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"setup":   true,
		"answers": h.mgr.Answers(),
		"steps":   stepOrder,
		"data_dir": h.mgr.dataDir,
		"edit":    true,
	})
}

func (h *EditHandler) advance(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p advancePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var err error
	switch p.Step {
	case "server", "admin", "review":
		// review-only / pre-filled in edit mode
	case "models":
		err = h.mgr.SetModels(p.Models)
	case "aux":
		err = h.mgr.SetAux(p.Aux)
	case "sidecars":
		// display-only; the server auto-provisions the sidecars (no user state)
	case "email":
		err = h.mgr.SetEmail(p.Email)
	default:
		err = fmt.Errorf("unknown step %q", p.Step)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *EditHandler) back(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p struct {
		Step string `json:"step"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p)
	h.mgr.StepBack(p.Step)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *EditHandler) cancel(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mgr.Cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *EditHandler) testModel(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	var m config.Model
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := provider.New(strings.ToLower(m.Provider), m.BaseURL, m.APIKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	models, err := p.Models(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model_count": len(models), "models": models})
}

func (h *EditHandler) probeModel(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := probeEndpoint(r.Context(), p.BaseURL, p.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": res.Provider, "models": res.Models})
}

func (h *EditHandler) finalize(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := h.mgr.FinalizeEdit(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Hot-reload the running agent with the freshly saved config so the edit
	// applies without a restart (the first-run finalize has no hook: the
	// in-process transition builds a brand-new app).
	reloaded := false
	if hook := h.mgr.onFinalize; hook != nil {
		if herr := hook(cfg); herr != nil {
			log.Printf("bevin: edit finalize: agent reload: %v", herr)
		} else {
			reloaded = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reloaded": reloaded, "data_dir": cfg.DataDir})
}

// sidecarsStart (edit mode) starts the provisioning run (idempotent — in edit
// mode it probes the already-running sidecars, fast) + returns the current
// status. Gated on an admin session.
func (h *EditHandler) sidecarsStart(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mgr.StartProvision()
	writeSidecarsStatus(w, h.mgr)
}

// sidecarsStatus (edit mode) returns the current per-sidecar provisioning
// status. Gated on an admin session.
func (h *EditHandler) sidecarsStatus(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeSidecarsStatus(w, h.mgr)
}

// gate checks the one-time setup token (query param, echoed into a cookie on
// first load).
func (h *Handler) gate(w http.ResponseWriter, r *http.Request) bool {
	token := r.URL.Query().Get("token")
	if token == "" {
		if c, err := r.Cookie("bevin_setup"); err == nil {
			token = c.Value
		}
	}
	if token == "" || token != h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if r.URL.Query().Get("token") != "" {
		http.SetCookie(w, &http.Cookie{Name: "bevin_setup", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	return true
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = wizardTmpl.Execute(w, map[string]any{"DataDir": h.mgr.dataDir, "Server": map[string]any{"bind": h.mgr.bind, "port": h.mgr.port, "dataDir": h.mgr.dataDir}, "Steps": stepOrder})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// sidecarView is a SidecarProgress + its display name / description (for the
// wizard's progress view).
type sidecarView struct {
	SidecarProgress
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// writeSidecarsStatus serializes the current provisioning state (the per-sidecar
// progress + the recorded endpoints). Shared by the setup + edit handlers.
func writeSidecarsStatus(w http.ResponseWriter, mgr *Manager) {
	done, running, statuses, endpoints := mgr.ProvisionStatus()
	views := make([]sidecarView, 0, len(statuses))
	for _, s := range statuses {
		meta := SidecarMeta[s.Kind]
		views = append(views, sidecarView{SidecarProgress: s, Name: meta.Name, Desc: meta.Desc})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"done":      done,
		"running":   running,
		"sidecars":  views,
		"endpoints": endpoints,
	})
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"setup":   h.mgr.IsSetup(),
		"answers": h.mgr.Answers(),
		"steps":   stepOrder,
		"data_dir": h.mgr.dataDir,
	})
}

type advancePayload struct {
	Step     string            `json:"step"`
	Admin    AdminAnswers      `json:"admin"`
	Models   []config.Model    `json:"models"`
	Aux      []config.AuxModel `json:"aux"`
	Sidecars config.Sidecars   `json:"sidecars"`
	Email    config.Email      `json:"email"`
}

func (h *Handler) advance(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p advancePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var err error
	switch p.Step {
	case "server":
		// review-only; no state
	case "admin":
		err = h.mgr.SetAdmin(p.Admin)
	case "models":
		err = h.mgr.SetModels(p.Models)
	case "aux":
		err = h.mgr.SetAux(p.Aux)
	case "sidecars":
		// display-only; the server auto-provisions the sidecars (no user state)
	case "email":
		err = h.mgr.SetEmail(p.Email)
	case "review":
		// no state; finalize is a separate call
	default:
		err = fmt.Errorf("unknown step %q", p.Step)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) back(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p struct {
		Step string `json:"step"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p)
	h.mgr.StepBack(p.Step)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mgr.Cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) testModel(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	var m config.Model
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := provider.New(strings.ToLower(m.Provider), m.BaseURL, m.APIKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	models, err := p.Models(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model_count": len(models), "models": models})
}

func (h *Handler) testSidecar(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	var p struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ok, err := h.mgr.sidecars.Probe(r.Context(), h.mgr.dataDir, p.Kind, p.Endpoint)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
}

// probeModel detects the provider kind for a base URL (testing the /models
// signature) + lists the models it serves, with context lengths where the API
// exposes them. Gated on the setup token like the other setup endpoints.
func (h *Handler) probeModel(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := probeEndpoint(r.Context(), p.BaseURL, p.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": res.Provider, "models": res.Models})
}

// TestEmail sends a test email. When smtp carries a host, it sends through
// those settings (the wizard's email step tests the draft before it is
// saved); otherwise it sends through the SAVED config (the admin dashboard
// tests the live config).
//
// The contract: a request with an SMTP host ALWAYS dials. The wizard's
// payload does not carry the draft's Enabled flag (its test section is only
// shown when the draft is enabled), so Enabled is forced here — relying on
// email.New's nil-for-unconfigured behavior would otherwise turn the test
// into a silent no-op that reports success without ever connecting.
func (m *Manager) TestEmail(ctx context.Context, smtp config.Email, to string) error {
	if strings.TrimSpace(smtp.SMTPHost) == "" {
		cfg, err := config.Load(m.dataDir)
		if err != nil {
			return fmt.Errorf("email is not configured yet — fill in the SMTP settings first")
		}
		smtp = cfg.Email
	}
	if strings.TrimSpace(smtp.SMTPHost) == "" {
		return fmt.Errorf("no SMTP host configured")
	}
	// Same defaults the saved config gets at save time (Validate): port 587,
	// from = username, from name "bevin".
	smtp.Enabled = true
	if smtp.SMTPPort <= 0 {
		smtp.SMTPPort = 587
	}
	if smtp.FromAddress == "" {
		smtp.FromAddress = smtp.Username
	}
	if smtp.FromName == "" {
		smtp.FromName = "bevin"
	}
	if smtp.FromAddress == "" {
		return fmt.Errorf("a from address (or username) is required to send email")
	}
	s := email.New(smtp)
	if s == nil {
		return fmt.Errorf("no SMTP host configured")
	}
	return s.Send(ctx, to, "bevin — test email",
		"This is a test email from your bevin server.\n\nIf you can read it, your SMTP settings work.\n\n— bevin")
}

// handleTestEmail is the shared body of the /setup/test-email (wizard) and
// /admin/setup/test-email (edit mode) endpoints; the callers do the gating.
func handleTestEmail(w http.ResponseWriter, r *http.Request, m *Manager) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p struct {
		To   string       `json:"to"`
		SMTP config.Email `json:"smtp"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := m.TestEmail(ctx, p.SMTP, strings.TrimSpace(p.To)); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": p.To})
}

func (h *Handler) testEmail(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	handleTestEmail(w, r, h.mgr)
}

func (h *EditHandler) testEmail(w http.ResponseWriter, r *http.Request) {
	if !h.admin(w, r) {
		return
	}
	handleTestEmail(w, r, h.mgr)
}

// sidecarsStart (setup mode) starts the sidecar provisioning run (idempotent)
// + returns the current status. The wizard calls this when the admin lands on
// the display-only sidecars step, then polls sidecarsStatus for progress.
func (h *Handler) sidecarsStart(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mgr.StartProvision()
	writeSidecarsStatus(w, h.mgr)
}

// sidecarsStatus (setup mode) returns the current per-sidecar provisioning
// status (the wizard streams it on the sidecars step).
func (h *Handler) sidecarsStatus(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeSidecarsStatus(w, h.mgr)
}

func (h *Handler) finalize(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := h.mgr.Finalize(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if h.transition != nil {
		h.transition()
	}
	// Issue the admin's session cookie now, so the completion screen's "Go to
	// the admin dashboard" button works immediately: the setup cookie alone
	// cannot enter the admin area (it is not a user session). Failure is
	// non-fatal — the admin can still log in at the login page.
	a := h.mgr.Answers()
	if a.Admin.Handle != "" {
		if u, tok, aerr := h.mgr.prov.Authenticate(r.Context(), a.Admin.Handle, a.Admin.Password); aerr == nil && u != nil {
			ttl := config.DefaultSessionTTLSeconds * time.Second
			if tp, ok := h.mgr.prov.(interface{ TTL() time.Duration }); ok {
				if p := tp.TTL(); p > 0 {
					ttl = p
				}
			}
			auth.SetSessionCookie(w, tok, ttl)
		} else if aerr != nil {
			log.Printf("bevin: setup finalize: admin session: %v", aerr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data_dir": cfg.DataDir})
}