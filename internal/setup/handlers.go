package setup

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/utopiafallen/alcyona-agent/internal/config"
	"github.com/utopiafallen/alcyona-agent/internal/provider"
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
var stepOrder = []string{"server", "admin", "models", "aux", "embedder", "sidecars", "review"}

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
	mux.HandleFunc("/setup/finalize", h.finalize)
}

// gate checks the one-time setup token (query param, echoed into a cookie on
// first load).
func (h *Handler) gate(w http.ResponseWriter, r *http.Request) bool {
	token := r.URL.Query().Get("token")
	if token == "" {
		if c, err := r.Cookie("alcyona_setup"); err == nil {
			token = c.Value
		}
	}
	if token == "" || token != h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if r.URL.Query().Get("token") != "" {
		http.SetCookie(w, &http.Cookie{Name: "alcyona_setup", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
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
	Embedder config.Embedder   `json:"embedder"`
	Sidecars config.Sidecars   `json:"sidecars"`
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
	case "embedder":
		err = h.mgr.SetEmbedder(p.Embedder)
	case "sidecars":
		err = h.mgr.SetSidecars(p.Sidecars)
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data_dir": cfg.DataDir})
}