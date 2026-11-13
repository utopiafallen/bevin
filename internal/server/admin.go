// The admin dashboard. /admin (gated on an ADMIN session — the admin does not
// use the chat UI as a regular user) redirects to the static dashboard page
// (web/static/admin.html), which pulls live state from the /admin/api/*
// endpoints: the per-model execution-slot pools + their utilization, the
// global task queue, sidecar health, and the email config (with a test send).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/setup"
	"github.com/utopiafallen/bevin/internal/store"
)

// requireAdmin wraps a handler so only an authenticated ADMIN may reach it.
func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, ok := a.adminGate(r)
		if !ok || role != "admin" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
			return
		}
		next(w, r)
	}
}

// adminPage is the gated entry point: /admin → /admin.html (the page shell is
// static; all data comes from the gated /admin/api/* endpoints).
func (a *App) adminPage(w http.ResponseWriter, r *http.Request) {
	if role, ok := a.adminGate(r); !ok || role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/admin.html", http.StatusFound)
}

// adminStatus is the live execution state for the dashboard: every model's
// slot pool (capacity + busy + the turns waiting for a slot) + the server
// basics.
func (a *App) adminStatus(w http.ResponseWriter, _ *http.Request) {
	pools := a.ag.Pools()
	totalSlots, busy, queued := 0, 0, 0
	for _, p := range pools {
		totalSlots += p.Slots
		busy += p.Busy
		queued += len(p.Waiting)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"time":   time.Now().UTC().Format(time.RFC3339),
		"pools":  pools,
		"totals": map[string]any{"slots": totalSlots, "busy": busy, "queued": queued},
		"server": map[string]any{"data_dir": a.dataDir, "bind": a.liveCfg().Bind},
	})
}

// adminSidecars is the live health of every configured sidecar (probed now).
func (a *App) adminSidecars(w http.ResponseWriter, r *http.Request) {
	sm := setup.NewSidecarManager(nil, nil) // the real probes (loopback HTTP / binary check)
	type row struct {
		Kind     string `json:"kind"`
		Endpoint string `json:"endpoint"`
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
	}
	rows := []row{}
	cfg := a.liveCfg()
	for _, sc := range []struct{ kind, endpoint string }{
		{setup.KindQdrant, cfg.Sidecars.Qdrant},
		{setup.KindLlamaServer, cfg.Sidecars.LlamaServer},
		{setup.KindFFmpeg, cfg.Sidecars.FFmpeg},
	} {
		if sc.endpoint == "" {
			rows = append(rows, row{sc.kind, "", false, "not configured"})
			continue
		}
		ok, err := sm.Probe(r.Context(), a.dataDir, sc.kind, sc.endpoint)
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		rows = append(rows, row{sc.kind, sc.endpoint, ok, errStr})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sidecars": rows})
}

// adminEmail is the live email config (the password is never returned — only
// whether one is set) + whether a sender is built.
func (a *App) adminEmail(w http.ResponseWriter, _ *http.Request) {
	e := a.liveCfg().Email
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": a.liveMail() != nil,
		"smtp": map[string]any{
			"smtp_host":    e.SMTPHost,
			"smtp_port":    e.SMTPPort,
			"username":     e.Username,
			"has_password": e.Password != "",
			"from_address": e.FromAddress,
			"from_name":    e.FromName,
			"use_starttls": e.UseStartTLS,
		},
	})
}

// adminEmailTest sends a test email. When the body carries an SMTP host it
// tests that draft (the admin email editor tests the draft before saving);
// otherwise it tests the SAVED config (the dashboard's email area). Same
// contract as the wizard's test: a request with a host ALWAYS dials.
func (a *App) adminEmailTest(w http.ResponseWriter, r *http.Request) {
	var p struct {
		To   string       `json:"to"`
		SMTP config.Email `json:"smtp"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.edit.TestEmail(ctx, p.SMTP, strings.TrimSpace(p.To)); err != nil {
		code := http.StatusBadGateway
		if strings.Contains(err.Error(), "not configured") {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": p.To})
}

// adminConfig is the current server config, the areas the admin config pages
// edit: models + aux (full config) + email + oauth (both masked: a stored
// password/secret is never returned — only whether one is set). Read from
// disk: disk is the commit of record, so the page always sees the current
// values (no staleness after a save).
func (a *App) adminConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(a.dataDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": cfg.Models,
		"aux":    cfg.Aux,
		"email": map[string]any{
			"enabled":      cfg.Email.Enabled,
			"smtp_host":    cfg.Email.SMTPHost,
			"smtp_port":    cfg.Email.SMTPPort,
			"username":     cfg.Email.Username,
			"has_password": cfg.Email.Password != "",
			"from_address": cfg.Email.FromAddress,
			"from_name":    cfg.Email.FromName,
			"use_starttls": cfg.Email.UseStartTLS,
		},
		"oauth": map[string]any{
			"enabled":      cfg.Auth.OAuth.Enabled,
			"name":         cfg.Auth.OAuth.Name,
			"client_id":    cfg.Auth.OAuth.ClientID,
			"has_secret":   cfg.Auth.OAuth.ClientSecret != "",
			"auth_url":     cfg.Auth.OAuth.AuthURL,
			"token_url":    cfg.Auth.OAuth.TokenURL,
			"userinfo_url": cfg.Auth.OAuth.UserinfoURL,
			"scopes":       cfg.Auth.OAuth.Scopes,
			"redirect_uri": cfg.Auth.OAuth.RedirectURI,
		},
	})
}

// adminConfigSave saves a config-area edit from the admin config pages (a
// subset of {models, aux, email, oauth}; a missing key is unchanged) +
// hot-reloads the running agent so the change applies without a restart. A
// blank email password / OAuth client secret keeps the stored one (the UI is
// never given the stored secret to echo back).
func (a *App) adminConfigSave(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Models *[]config.Model    `json:"models"`
		Aux    *[]config.AuxModel `json:"aux"`
		Email  *config.Email      `json:"email"`
		OAuth  *config.OAuth      `json:"oauth"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if p.Models == nil && p.Aux == nil && p.Email == nil && p.OAuth == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to save"})
		return
	}
	if p.Email != nil && p.Email.Password == "" {
		if cur, err := config.Load(a.dataDir); err == nil {
			p.Email.Password = cur.Email.Password // blank = keep the stored one
		}
	}
	if p.OAuth != nil && p.OAuth.ClientSecret == "" {
		if cur, err := config.Load(a.dataDir); err == nil {
			p.OAuth.ClientSecret = cur.Auth.OAuth.ClientSecret // blank = keep the stored one
		}
	}
	cfg, err := a.edit.CommitEdit(p.Models, p.Aux, p.Email, p.OAuth)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("config: saved by admin (%s)", clientIP(r))
	reloaded := a.applyConfig(cfg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reloaded": reloaded})
}

// adminProbeModel detects the provider + lists the models for a base URL (the
// admin models page's Detect button) — the same probe the setup wizard uses.
func (a *App) adminProbeModel(w http.ResponseWriter, r *http.Request) {
	var p struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := setup.ProbeEndpoint(r.Context(), p.BaseURL, p.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": res.Provider, "models": res.Models})
}

// adminModelsPage / adminEmailPage are the gated entry points to the admin
// config pages (static HTML; all data via the gated /admin/api/* endpoints).
// The pages themselves are reachable at their static URLs too (like
// /admin.html) — the APIs are what are gated.
func (a *App) adminModelsPage(w http.ResponseWriter, r *http.Request) {
	if role, ok := a.adminGate(r); !ok || role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/admin-models.html", http.StatusFound)
}

func (a *App) adminEmailPage(w http.ResponseWriter, r *http.Request) {
	if role, ok := a.adminGate(r); !ok || role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/admin-email.html", http.StatusFound)
}

func (a *App) adminUsersPage(w http.ResponseWriter, r *http.Request) {
	if role, ok := a.adminGate(r); !ok || role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/admin-users.html", http.StatusFound)
}

// adminUsersList is the full user registry — every account the system knows
// about, from BOTH IdPs: the server's own (internal handles) and the external
// one (OAuth, labeled by origin + provider subject).
func (a *App) adminUsersList(w http.ResponseWriter, r *http.Request) {
	users, err := a.srv.ListUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// adminUsersAdd creates an INTERNAL account (the server's own IdP) — separate
// from any external IdP. Only the admin can create users; the new account
// always gets the "user" role (there is one admin).
func (a *App) adminUsersAdd(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Handle      string `json:"handle"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p.Handle = strings.TrimSpace(p.Handle)
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	if !setup.ValidHandle(p.Handle) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "handle must be 2-32 chars, lowercase letters/digits, starting with a letter or digit"})
		return
	}
	if p.DisplayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a display name is required"})
		return
	}
	if len(p.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the password must be at least 8 characters"})
		return
	}
	if _, err := a.srv.GetUser(p.Handle); err == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a user with that handle already exists"})
		return
	} else if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	u, err := a.prov.CreateUserProvisioned(r.Context(), p.Handle, p.DisplayName, "user", p.Password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("bevin: admin: created internal user %s (%s)", p.Handle, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u})
}

// adminUsersStatus enables/disables any account (from either IdP). A
// disabled account can no longer sign in (internal or OAuth) and its live
// sessions stop working on the next request. The admin cannot change their
// own status (locking the last admin out is not a recovery path).
func (a *App) adminUsersStatus(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Handle string `json:"handle"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if p.Status != "active" && p.Status != "disabled" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be active or disabled"})
		return
	}
	selfUser, ok := a.sessionUser(r)
	if !ok || selfUser.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	if strings.TrimSpace(p.Handle) == selfUser.Handle {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "you cannot change your own status"})
		return
	}
	if err := a.srv.SetUserStatus(strings.TrimSpace(p.Handle), p.Status); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("bevin: admin: set user %s → %s (%s)", p.Handle, p.Status, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
