// The admin dashboard. /admin (gated on an ADMIN session — the admin does not
// use the chat UI as a regular user) redirects to the static dashboard page
// (web/static/admin.html), which pulls live state from the /admin/api/*
// endpoints: the per-model execution-slot pools + their utilization, the
// global task queue, sidecar health, and the email config (with a test send).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/utopiafallen/bevin/internal/setup"
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
		"time": time.Now().UTC().Format(time.RFC3339),
		"pools": pools,
		"totals": map[string]any{"slots": totalSlots, "busy": busy, "queued": queued},
		"server": map[string]any{"data_dir": a.dataDir, "bind": a.cfg.Bind},
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
	for _, sc := range []struct{ kind, endpoint string }{
		{setup.KindQdrant, a.cfg.Sidecars.Qdrant},
		{setup.KindLlamaServer, a.cfg.Sidecars.LlamaServer},
		{setup.KindFFmpeg, a.cfg.Sidecars.FFmpeg},
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
	e := a.cfg.Email
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": a.mail != nil,
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

// adminEmailTest sends a test email through the SAVED config to the given
// address (the dashboard's email area). The request needs no SMTP fields —
// the saved config is authoritative here (the wizard's email step uses
// /setup/test-email with its draft instead).
func (a *App) adminEmailTest(w http.ResponseWriter, r *http.Request) {
	var p struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.mail == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "email is not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.mail.Send(ctx, p.To, "bevin — test email",
		"This is a test email from your bevin server.\n\nIf you can read it, your SMTP settings work.\n\n— bevin"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": p.To})
}
