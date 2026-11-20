package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/agent"
	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/email"
	"github.com/utopiafallen/bevin/internal/setup"
	"github.com/utopiafallen/bevin/internal/store"
)

// fakeSMTP is a minimal SMTP server that accepts one message + captures it.
type fakeSMTP struct {
	addr    string
	recv    chan string
	running int32 // atomic: 1 while serving, 0 after the test ends
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{addr: ln.Addr().String(), recv: make(chan string, 1), running: 1}
	go func() {
		defer ln.Close()
		for atomic.LoadInt32(&f.running) == 1 {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { atomic.StoreInt32(&f.running, 0) })
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(s string) { fmt.Fprint(conn, s) }
	w("220 fake ESMTP\r\n")
	for atomic.LoadInt32(&f.running) == 1 {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			w("250-fake\r\n250-AUTH PLAIN\r\n250 OK\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			w("250 OK\r\n")
		case strings.HasPrefix(cmd, "RCPT TO"):
			w("250 OK\r\n")
		case strings.HasPrefix(cmd, "DATA"):
			w("354 go ahead\r\n")
			var data []byte
			for {
				d, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimSpace(d) == "." {
					break
				}
				data = append(data, d...)
			}
			w("250 OK\r\n")
			select {
			case f.recv <- string(data):
			default:
			}
		case strings.HasPrefix(cmd, "QUIT"):
			w("221 bye\r\n")
			return
		default:
			w("250 OK\r\n")
		}
	}
}

// waitMsg blocks until the fake server captured a message (or times out).
func (f *fakeSMTP) waitMsg(t *testing.T) string {
	t.Helper()
	select {
	case m := <-f.recv:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the SMTP message")
		return ""
	}
}

// hostPort splits a 127.0.0.1:port address into host + port.
func hostPort(addr string) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	var pi int
	fmt.Sscanf(p, "%d", &pi)
	return h, pi
}

// testApp builds a minimal App with the email config pointing at a fake SMTP
// server + a store with one admin user (with a live session token "tok-test").
func testApp(t *testing.T, emailCfg config.Email) (*App, *store.ServerStore) {
	t.Helper()
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-test", "admin", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	prov := auth.NewInternalProvider(srv, time.Hour)
	cfg := &config.ServerConfig{
		DataDir: dir,
		Auth:    config.Auth{SessionTTLSeconds: 3600},
		Email:   emailCfg,
	}
	a := &App{cfg: cfg, prov: prov, srv: srv, ttl: time.Hour, dataDir: dir}
	a.mail = email.New(emailCfg) // nil when not configured
	a.routes()
	return a, srv
}

// newRequest builds an authenticated request (the "tok-test" admin session).
func newRequest(method, target string, body any) *http.Request {
	return newRequestAs(method, target, "tok-test", body)
}

// newAgentForTest builds an agent from a config (the test points the model at
// a fake backend).
func newAgentForTest(cfg *config.ServerConfig, dir string) (*agent.Agent, error) {
	return agent.New(cfg, dir)
}

// newRequestAs builds an authenticated request with a specific session token.
func newRequestAs(method, target, token string, body any) *http.Request {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, target, strings.NewReader(string(b)))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.AddCookie(&http.Cookie{Name: "bevin_session", Value: token})
	return r
}

func TestEmailAddressEndpoint(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	mux := http.NewServeMux()
	// Wire the routes through the App's own mux (a.routes already did this).
	_ = mux

	// The admin has no email yet + email is not configured.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/api/email", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var st map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["configured"] != false {
		t.Fatalf("configured = %v, want false", st["configured"])
	}
	if st["email"] != "" {
		t.Fatalf("email = %v, want empty", st["email"])
	}

	// Set the admin's email.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPut, "/api/email", map[string]string{"email": "admin@example.com"}))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT code = %d body=%s", w.Code, w.Body.String())
	}
	u, _ := srv.GetUser("admin")
	if u.Email != "admin@example.com" {
		t.Fatalf("stored email = %q", u.Email)
	}

	// An invalid email is rejected.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPut, "/api/email", map[string]string{"email": "not-an-email"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid email code = %d, want 400", w.Code)
	}

	// A CRLF in the address (header injection into the notification's To:
	// header) is rejected with a 400, not stored (security 2026-11-20).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPut, "/api/email", map[string]string{"email": "evil@example.com\r\nBcc: leak@evil.com"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CRLF email code = %d, want 400", w.Code)
	}
	u, _ = srv.GetUser("admin")
	if u.Email != "admin@example.com" {
		t.Fatalf("a rejected CRLF address clobbered the stored email: %q", u.Email)
	}
}

func TestNotifySendsToFakeSMTP(t *testing.T) {
	fs := newFakeSMTP(t)
	h, p := hostPort(fs.addr)
	a, srv := testApp(t, config.Email{
		Enabled: true, SMTPHost: h, SMTPPort: p,
		FromName: "Bevin", FromAddress: "bevin@example.com", UseStartTLS: false,
	})
	if a.mail == nil {
		t.Fatal("expected a configured sender")
	}
	// Give the admin an email address.
	if err := srv.SetUserEmail("admin", "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	// Fire a notification (fire-and-forget).
	a.notify("admin", "Task finished", "Your task is done.")
	msg := fs.waitMsg(t)
	if !strings.Contains(msg, "To: admin@example.com") {
		t.Fatalf("message missing recipient: %.200s", msg)
	}
	if !strings.Contains(msg, "Subject: Task finished") {
		t.Fatalf("message missing subject: %.200s", msg)
	}
	if !strings.Contains(msg, "Your task is done.") {
		t.Fatalf("message missing body: %.200s", msg)
	}
	if !strings.Contains(msg, "From: Bevin <bevin@example.com>") {
		t.Fatalf("message missing from: %.200s", msg)
	}
}

// TestNotifyNoopWhenNotConfigured verifies notify is a no-op when email is not
// configured (no sender, no panic).
func TestNotifyNoopWhenNotConfigured(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	if a.mail != nil {
		t.Fatal("expected a nil sender when email is not configured")
	}
	a.notify("admin", "subject", "body") // must not panic
}

// TestSetEmailAdminCanSetOthers verifies an admin can set another user's
// address (and a non-admin cannot).
func TestSetEmailAdminCanSetOthers(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	// Create a second (non-admin) user + a session for them.
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}

	// The admin sets bob's email.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPut, "/api/email", map[string]string{"handle": "bob", "email": "bob@example.com"}))
	if w.Code != http.StatusOK {
		t.Fatalf("admin set-bob code = %d body=%s", w.Code, w.Body.String())
	}
	u, _ := srv.GetUser("bob")
	if u.Email != "bob@example.com" {
		t.Fatalf("bob email = %q", u.Email)
	}

	// A non-admin (bob) cannot set the admin's email.
	req := newRequestAs(http.MethodPut, "/api/email", "tok-bob", map[string]string{"handle": "admin", "email": "x@y.z"})
	w = httptest.NewRecorder()
	a.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin cross-set code = %d, want 403", w.Code)
	}
}

// fakeOAIStream is an OpenAI-compatible /chat/completions endpoint that
// responds with a streaming SSE body: two text deltas + a final chunk carrying
// the usage (mirrors the real wire with stream_options.include_usage).
func fakeOAIStream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		emit := func(data string) {
			fmt.Fprint(w, "data: "+data+"\n\n")
			fl.Flush()
		}
		emit(`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`)
		emit(`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`)
		emit(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
		emit("[DONE]")
	}))
}

// configApp builds an App with a real agent + edit Manager + a valid saved
// config (the admin config endpoints exercise all three).
func configApp(t *testing.T) (*App, *config.ServerConfig) {
	t.Helper()
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-test", "admin", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	prov := auth.NewInternalProvider(srv, time.Hour)

	oai := fakeOAIStream(t)
	t.Cleanup(oai.Close)
	cfg := &config.ServerConfig{
		DataDir: dir,
		Auth:    config.Auth{SessionTTLSeconds: 3600, SessionSecret: "test-secret"},
		Models: []config.Model{{
			ID: "m1", Provider: "openai", BaseURL: oai.URL + "/v1",
			APIKey: "sk-test", ModelID: "gpt-4o", DisplayName: "GPT-4o",
			MaxContext: 128000, Slots: 1, Enabled: true,
		}},
	}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	edit := setup.New(dir, cfg.Bind, 0, srv, prov, nil)
	a := &App{cfg: cfg, prov: prov, ag: ag, srv: srv, ttl: time.Hour, dataDir: dir, edit: edit}
	a.routes()
	return a, cfg
}

// TestAdminModelsPageRedirect pins the models-editor inlining: the dedicated
// /admin/models page is gone (the editor lives on the dashboard's Models
// page), so the old URL 303s to /admin#models — and a non-admin is rejected
// rather than redirected.
func TestAdminModelsPageRedirect(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/models", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("admin GET /admin/models code = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin#models" {
		t.Fatalf("Location = %q, want /admin#models", loc)
	}
	// No session (a non-admin): rejected, not redirected.
	r := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated GET /admin/models code = %d, want 403", w.Code)
	}
}

// TestAdminUsersPageRedirect pins the users-panel inlining: the dedicated
// /admin/users page is gone (the panel lives on the dashboard's Users page),
// so the old URL 303s to /admin#users — and a non-admin is rejected rather
// than redirected (same contract as the models redirect).
func TestAdminUsersPageRedirect(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/users", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("admin GET /admin/users code = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin#users" {
		t.Fatalf("Location = %q, want /admin#users", loc)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated GET /admin/users code = %d, want 403", w.Code)
	}
}

// TestAdminEmailPageRedirect pins the email-editor inlining: the dedicated
// /admin/email page is gone (the editor lives on the dashboard's Email
// page, built from the shared setup-wizard email form), so the old URL 303s
// to /admin#email — and a non-admin is rejected rather than redirected
// (same contract as the models/users redirects).
func TestAdminEmailPageRedirect(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/email", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("admin GET /admin/email code = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin#email" {
		t.Fatalf("Location = %q, want /admin#email", loc)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/email", nil)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated GET /admin/email code = %d, want 403", w.Code)
	}
}

// TestAdminConfigEndpoints — the admin config pages' read/save API: admin
// gating, the models save (persisted + hot-reloaded into the agent), the
// nothing-to-save rejection, and the email placeholder defaults (a blank port
// is saved as 587, the password is preserved when left blank).
func TestAdminConfigEndpoints(t *testing.T) {
	a, cfg := configApp(t)

	// No session -> 403 on the config API.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/config", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated config read code = %d, want 403", w.Code)
	}

	// Admin read: the saved models + the email (password masked).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("config read code = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Models []config.Model `json:"models"`
		Email  map[string]any `json:"email"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "m1" {
		t.Fatalf("config read models = %+v", got.Models)
	}
	if _, has := got.Email["has_password"]; !has {
		t.Fatalf("email read missing has_password: %+v", got.Email)
	}
	if _, has := got.Email["password"]; has {
		t.Fatalf("email read must not return the password: %+v", got.Email)
	}

	// Save a model edit: slots 1 -> 3. It must persist to disk AND hot-reload
	// the running agent (the pool capacity changes without a restart).
	w = httptest.NewRecorder()
	edited := []config.Model{{ID: "m1", Provider: "openai", BaseURL: cfg.Models[0].BaseURL, APIKey: "sk-test", ModelID: "gpt-4o", DisplayName: "GPT-4o", MaxContext: 128000, Slots: 3, Enabled: true}}
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"models": edited}))
	if w.Code != http.StatusOK {
		t.Fatalf("config save code = %d body=%s", w.Code, w.Body.String())
	}
	var saveRes struct {
		OK       bool `json:"ok"`
		Reloaded bool `json:"reloaded"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &saveRes)
	if !saveRes.OK || !saveRes.Reloaded {
		t.Fatalf("save result = %+v, want ok+reloaded", saveRes)
	}
	onDisk, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Models[0].Slots != 3 {
		t.Fatalf("saved slots = %d, want 3", onDisk.Models[0].Slots)
	}
	pools := a.ag.Pools()
	if len(pools) != 1 || pools[0].Slots != 3 {
		t.Fatalf("agent pools after save = %+v, want m1 with 3 slots", pools)
	}
	// The session secret is preserved (a save is not a re-setup).
	if onDisk.Auth.SessionSecret != "test-secret" {
		t.Fatal("session secret should be preserved on a config save")
	}

	// Nothing to save -> 400.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty save code = %d, want 400", w.Code)
	}

	// An invalid catalog is rejected + does not clobber the saved one.
	w = httptest.NewRecorder()
	bad := []config.Model{{ID: "m1", Provider: "openai", BaseURL: cfg.Models[0].BaseURL, APIKey: "sk-test", ModelID: "gpt-4o", Enabled: false}}
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"models": bad}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid save code = %d, want 400", w.Code)
	}
	after, _ := config.Load(a.dataDir)
	if after.Models[0].Slots != 3 {
		t.Fatalf("a rejected save must not clobber the saved config, slots = %d", after.Models[0].Slots)
	}

	// Email save: a blank port takes the 587 placeholder default; a blank
	// password keeps the stored one.
	seed := &config.ServerConfig{DataDir: a.dataDir, Auth: cfg.Auth, Models: onDisk.Models,
		Email: config.Email{Enabled: true, SMTPHost: "smtp.example.com", SMTPPort: 2525, Username: "bevin@example.com", Password: "old-pw", FromName: "Bevin"}}
	if err := config.Save(a.dataDir, seed); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"email": map[string]any{
		"enabled": true, "smtp_host": "smtp.example.com", "smtp_port": 0,
		"username": "bevin@example.com", "password": "", "from_name": "Bevin",
	}}))
	if w.Code != http.StatusOK {
		t.Fatalf("email save code = %d body=%s", w.Code, w.Body.String())
	}
	emailAfter, _ := config.Load(a.dataDir)
	if emailAfter.Email.SMTPPort != 587 {
		t.Fatalf("blank port should save as 587, got %d", emailAfter.Email.SMTPPort)
	}
	if emailAfter.Email.Password != "old-pw" {
		t.Fatalf("a blank password should keep the stored one, got %q", emailAfter.Email.Password)
	}
}

// TestAdminProfilesEndpoint — GET /admin/api/profiles lists this build's
// registry (the admin models page's profile selects render from it): admin
// gated, and the response is the single source of truth the UI renders.
func TestAdminProfilesEndpoint(t *testing.T) {
	a, _ := configApp(t)

	// No session -> 403 (the admin gate).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/profiles", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated profiles code = %d, want 403", w.Code)
	}

	// Admin -> the registry: general present, with a description, and every
	// name resolvable (the UI builds its selects from this list).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/profiles", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("profiles code = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Profiles []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) == 0 {
		t.Fatal("no profiles listed")
	}
	var sawGeneral bool
	for _, p := range got.Profiles {
		if p.Name == "general" {
			sawGeneral = true
		}
		if p.Description == "" {
			t.Fatalf("profile %q has no description (the forms show it under the select)", p.Name)
		}
	}
	if !sawGeneral {
		t.Fatalf("general missing from the registry listing: %+v", got.Profiles)
	}

	// The admin model read carries the profile field (general after the
	// blank self-heal), so the forms preselect it.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("config read code = %d", w.Code)
	}
	var gotCfg struct {
		Models []config.Model `json:"models"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &gotCfg)
	if gotCfg.Models[0].Profile != "general" {
		t.Fatalf("model profile = %q, want general (blank self-heals at load)", gotCfg.Models[0].Profile)
	}

	// A model save with an UNKNOWN profile is rejected (loud, at save) and
	// does not clobber the saved config.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"models": []config.Model{{
		ID: "m1", Provider: "openai", BaseURL: "http://127.0.0.1:1/v1", ModelID: "gpt-4o",
		DisplayName: "GPT-4o", MaxContext: 128000, Slots: 1, Enabled: true, Profile: "no-such-profile",
	}}}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown-profile save code = %d, want 400", w.Code)
	}
	onDisk, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Models[0].Profile != "general" {
		t.Fatalf("a rejected save clobbered the saved profile: %q", onDisk.Models[0].Profile)
	}

	// A model save with a KNOWN profile is accepted + persisted (the
	// round-trip the form's select depends on).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"models": []config.Model{{
		ID: "m1", Provider: "openai", BaseURL: "http://127.0.0.1:1/v1", ModelID: "gpt-4o",
		DisplayName: "GPT-4o", MaxContext: 128000, Slots: 1, Enabled: true, Profile: "general",
	}}}))
	if w.Code != http.StatusOK {
		t.Fatalf("known-profile save code = %d, want 200", w.Code)
	}
	after, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Models[0].Profile != "general" {
		t.Fatalf("saved profile = %q, want general", after.Models[0].Profile)
	}
}

// twoModelApp builds an App with two available models (the model-switch
// tests need a second one to switch to) + a regular user "bob" with a live
// session.
func twoModelApp(t *testing.T) (*App, *config.ServerConfig, *store.ServerStore) {
	t.Helper()
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	prov := auth.NewInternalProvider(srv, time.Hour)
	oai := fakeOAIStream(t)
	t.Cleanup(oai.Close)
	cfg := &config.ServerConfig{
		DataDir: dir,
		Auth:    config.Auth{SessionTTLSeconds: 3600},
		Models: []config.Model{
			{ID: "m1", Provider: "openai", BaseURL: oai.URL + "/v1", APIKey: "sk-test", ModelID: "gpt-4o", DisplayName: "GPT-4o", MaxContext: 128000, Slots: 1, Enabled: true},
			{ID: "m2", Provider: "openai", BaseURL: oai.URL + "/v1", APIKey: "sk-test", ModelID: "gpt-4o-mini", DisplayName: "GPT-4o Mini", MaxContext: 128000, Slots: 1, Enabled: true},
		},
	}
	ag, err := agent.New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	a := &App{cfg: cfg, prov: prov, ag: ag, srv: srv, ttl: time.Hour, dataDir: dir}
	a.routes()
	return a, cfg, srv
}

// TestConversationManagement — the conversation lifecycle endpoints: list
// (slim rows, most recently active first), model switch (validated against
// the available set), rename, and delete (404 afterwards; the message
// endpoint follows).
func TestConversationManagement(t *testing.T) {
	a, _, srv := twoModelApp(t)

	// Create two conversations (bob's default model is m1 — the first
	// available one).
	mkConv := func(title string) string {
		t.Helper()
		w := httptest.NewRecorder()
		a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob", map[string]string{"title": title}))
		if w.Code != http.StatusOK {
			t.Fatalf("create %q code = %d body=%s", title, w.Code, w.Body.String())
		}
		var res map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return res["id"].(string)
	}
	cOld := mkConv("older one")
	cNew := mkConv("newer one")
	// Touch the old conversation so it is the most recently active. The
	// activity timestamps have second precision, so wait across a second
	// boundary to make the ordering assertion deterministic.
	time.Sleep(1100 * time.Millisecond)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations/"+cOld+"/messages/stream", "tok-bob", map[string]string{"message": "keep me warm"}))
	if w.Code != http.StatusOK {
		t.Fatalf("warm turn code = %d body=%s", w.Code, w.Body.String())
	}

	// List: both conversations, most recently active first, slim fields.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations", "tok-bob", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list code = %d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Conversations []struct {
			ID             string `json:"id"`
			Title          string `json:"title"`
			ActiveModelID  string `json:"active_model_id"`
			LastActivityAt string `json:"last_activity_at"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Conversations) != 2 {
		t.Fatalf("list = %+v, want 2 rows", list)
	}
	if list.Conversations[0].ID != cOld {
		t.Fatalf("list[0] = %+v, want the most recently active (%s)", list.Conversations[0], cOld)
	}
	if list.Conversations[0].ActiveModelID != "m1" || list.Conversations[0].Title != "older one" {
		t.Fatalf("row = %+v, want title 'older one' on m1", list.Conversations[0])
	}

	// Switch the newer conversation to m2: the response is the updated row.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPatch, "/api/conversations/"+cNew, "tok-bob", map[string]any{"model_id": "m2"}))
	if w.Code != http.StatusOK {
		t.Fatalf("model switch code = %d body=%s", w.Code, w.Body.String())
	}
	var up struct {
		Conversation struct {
			ID            string `json:"id"`
			Title         string `json:"title"`
			ActiveModelID string `json:"active_model_id"`
		} `json:"conversation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.Conversation.ID != cNew || up.Conversation.ActiveModelID != "m2" {
		t.Fatalf("updated = %+v, want %s on m2", up, cNew)
	}

	// A model that is not available is rejected (400, conversation intact).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPatch, "/api/conversations/"+cNew, "tok-bob", map[string]any{"model_id": "m-nope"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown model code = %d, want 400", w.Code)
	}

	// Rename: the title updates, the model is untouched.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPatch, "/api/conversations/"+cNew, "tok-bob", map[string]any{"title": "renamed"}))
	if w.Code != http.StatusOK {
		t.Fatalf("rename code = %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.Conversation.Title != "renamed" || up.Conversation.ActiveModelID != "m2" {
		t.Fatalf("renamed = %+v, want title 'renamed' still on m2", up.Conversation)
	}

	// A blank title is rejected.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPatch, "/api/conversations/"+cNew, "tok-bob", map[string]any{"title": "   "}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank title code = %d, want 400", w.Code)
	}
	// An empty patch is rejected.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPatch, "/api/conversations/"+cNew, "tok-bob", map[string]any{}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty patch code = %d, want 400", w.Code)
	}
	// Patching a missing conversation is 404.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPatch, "/api/conversations/nope", "tok-bob", map[string]any{"title": "x"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("patch missing code = %d, want 404", w.Code)
	}

	// Single get: the slim row (title + active model), as the UI reads it
	// before switching the model.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations/"+cNew, "tok-bob", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get code = %d body=%s", w.Code, w.Body.String())
	}
	one := struct {
		Conversation struct {
			Title         string `json:"title"`
			ActiveModelID string `json:"active_model_id"`
		} `json:"conversation"`
	}{}
	_ = json.Unmarshal(w.Body.Bytes(), &one)
	if one.Conversation.Title != "renamed" || one.Conversation.ActiveModelID != "m2" {
		t.Fatalf("single get = %+v, want 'renamed' on m2", one.Conversation)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations/nope", "tok-bob", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("get missing code = %d, want 404", w.Code)
	}
	// The {id} wildcard must not shadow the literal /search route.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations/search", "tok-bob", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("search-without-q code = %d, want 400 (the literal route), not the wildcard's 404", w.Code)
	}

	// Delete: ok, then the conversation is gone from the list and the
	// message endpoint 404s; deleting again is 404.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/conversations/"+cNew, "tok-bob", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("delete code = %d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations", "tok-bob", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("post-delete list code = %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Conversations) != 1 || list.Conversations[0].ID != cOld {
		t.Fatalf("list after delete = %+v, want only the older conversation", list)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations/"+cNew+"/messages", "tok-bob", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("messages after delete code = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/conversations/"+cNew, "tok-bob", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("second delete code = %d, want 404", w.Code)
	}

	// Per-user isolation: a second user's list is empty (routing is by
	// session, and each user only ever sees their own conversations).
	if _, err := srv.CreateUser("carol", "Carol", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-carol", "carol", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations", "tok-carol", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("carol list code = %d", w.Code)
	}
	var carol struct {
		Conversations []any `json:"conversations"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &carol)
	if len(carol.Conversations) != 0 {
		t.Fatalf("carol sees %d conversations, want 0 (per-user isolation)", len(carol.Conversations))
	}
}

// reasoningModelApp builds twoModelApp's setup with m2 under the qwen3.8
// profile (the one with the low/medium/xhigh reasoning-effort control) —
// the create-with-effort tests need a model with a real effort set.
func reasoningModelApp(t *testing.T) (*App, *store.ServerStore) {
	t.Helper()
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	prov := auth.NewInternalProvider(srv, time.Hour)
	oai := fakeOAIStream(t)
	t.Cleanup(oai.Close)
	cfg := &config.ServerConfig{
		DataDir: dir,
		Auth:    config.Auth{SessionTTLSeconds: 3600},
		Models: []config.Model{
			{ID: "m1", Provider: "openai", BaseURL: oai.URL + "/v1", APIKey: "sk-test", ModelID: "gpt-4o", DisplayName: "GPT-4o", MaxContext: 128000, Slots: 1, Enabled: true},
			{ID: "m2", Provider: "openai", BaseURL: oai.URL + "/v1", APIKey: "sk-test", ModelID: "gpt-4o-mini", DisplayName: "GPT-4o Mini", MaxContext: 128000, Slots: 1, Enabled: true, Profile: "qwen3.8"},
		},
	}
	ag, err := agent.New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	a := &App{cfg: cfg, prov: prov, ag: ag, srv: srv, ttl: time.Hour, dataDir: dir}
	a.routes()
	return a, srv
}

// TestCreateConversationWithModelAndEffort — the new-conversation create
// carries the UI's pre-first-send choice: an explicit available model + a
// supported effort are persisted to the row; an unavailable model and an
// unsupported effort are 400s that create NOTHING (the create is the
// moment the conversation comes into existence — a rejection must not
// leave a phantom row).
func TestCreateConversationWithModelAndEffort(t *testing.T) {
	a, _ := reasoningModelApp(t)

	listCount := func() int {
		t.Helper()
		w := httptest.NewRecorder()
		a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations", "tok-bob", nil))
		var l struct {
			Conversations []any `json:"conversations"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &l)
		return len(l.Conversations)
	}

	// Pin model m2 + effort "medium": the row is created with both (read
	// back through the single get).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob",
		map[string]any{"title": "pinned", "model_id": "m2", "reasoning_effort": "medium"}))
	if w.Code != http.StatusOK {
		t.Fatalf("pinned create code = %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	pinnedID := created["id"].(string)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations/"+pinnedID, "tok-bob", nil))
	var one struct {
		Conversation struct {
			ActiveModelID   string `json:"active_model_id"`
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"conversation"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &one)
	if one.Conversation.ActiveModelID != "m2" || one.Conversation.ReasoningEffort != "medium" {
		t.Fatalf("pinned row = %+v, want m2 + medium", one.Conversation)
	}

	// A plain create (no model/effort): the server default + the profile
	// default.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob", map[string]string{"title": "plain"}))
	if w.Code != http.StatusOK {
		t.Fatalf("plain create code = %d body=%s", w.Code, w.Body.String())
	}
	var plain map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &plain)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/conversations/"+plain["id"].(string), "tok-bob", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &one)
	if one.Conversation.ActiveModelID != "m1" || one.Conversation.ReasoningEffort != "" {
		t.Fatalf("plain row = %+v, want m1 + no effort", one.Conversation)
	}

	// An unavailable model: 400, nothing created.
	before := listCount()
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob",
		map[string]any{"title": "bad", "model_id": "m-nope"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unavailable model code = %d, want 400", w.Code)
	}
	if got := listCount(); got != before {
		t.Fatalf("a rejected create still wrote a conversation (%d -> %d)", before, got)
	}

	// An effort the model doesn't support: 400 naming the supported
	// levels, nothing created.
	before = listCount()
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob",
		map[string]any{"title": "bad-effort", "model_id": "m2", "reasoning_effort": "ultra"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad effort code = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "low, medium, xhigh") {
		t.Fatalf("bad effort body = %s, want the supported levels named", w.Body.String())
	}
	// ... and an effort for a model that takes none.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob",
		map[string]any{"title": "bad-effort-2", "model_id": "m1", "reasoning_effort": "low"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("effort-for-no-effort-model code = %d, want 400", w.Code)
	}
	if got := listCount(); got != before {
		t.Fatalf("a rejected create still wrote a conversation (%d -> %d)", before, got)
	}
}

// TestMeDefaultModel — /api/me carries the user's per-user default model
// (agent.toml): "" when unset, the stored id otherwise (the client's
// new-conversation preview preselects it the same way the create endpoint
// resolves it).
func TestMeDefaultModel(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	// A regular user (the admin is not a regular user — /api/me works for
	// any session, but the user is who onboards a default).
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	// Unset: empty.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/me", "tok-bob", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("me code = %d", w.Code)
	}
	var meBody map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &meBody)
	if meBody["default_model_id"] != "" {
		t.Fatalf("default_model_id = %v, want empty (no agent.toml)", meBody["default_model_id"])
	}
	// Set: the stored id (the file is read fresh — no restart needed).
	if err := os.MkdirAll(filepath.Join(a.dataDir, "users", "bob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.dataDir, "users", "bob", "agent.toml"), []byte("default_model_id = \"m2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/me", "tok-bob", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &meBody)
	if meBody["default_model_id"] != "m2" {
		t.Fatalf("default_model_id = %v, want m2", meBody["default_model_id"])
	}
}

// TestSendMessageStream verifies the SSE chat endpoint: it emits "message"
// events for the deltas + a "done" event carrying the conversation id, the full
// reply, and the API-reported usage (from the final chunk).
func TestSendMessageStream(t *testing.T) {
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-test", "admin", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	// The chat API is the user interaction surface — the admin is not a
	// regular user (it would 403), so this test chats as a regular user.
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	prov := auth.NewInternalProvider(srv, time.Hour)

	// A fake OAI backend that streams + reports usage.
	oai := fakeOAIStream(t)
	t.Cleanup(oai.Close)
	cfg := &config.ServerConfig{
		DataDir: dir,
		Auth:    config.Auth{SessionTTLSeconds: 3600},
		Models: []config.Model{{
			ID: "m1", Provider: "openai", BaseURL: oai.URL + "/v1",
			APIKey: "k", ModelID: "gpt-4o", DisplayName: "GPT-4o",
			MaxContext: 128000, Slots: 1, Enabled: true,
		}},
	}
	ag, err := newAgentForTest(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	a := &App{cfg: cfg, prov: prov, ag: ag, srv: srv, ttl: time.Hour, dataDir: dir}
	a.routes()

	// Create a conversation (as the regular user — the admin is gated out of
	// the chat API).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob", map[string]string{"title": "t"}))
	if w.Code != http.StatusOK {
		t.Fatalf("createConversation code = %d body=%s", w.Code, w.Body.String())
	}
	var conv map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &conv)
	convID := conv["id"].(string)

	// Stream a message.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations/"+convID+"/messages/stream", "tok-bob", map[string]string{"message": "hi"}))
	if w.Code != http.StatusOK {
		t.Fatalf("stream code = %d body=%s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	// The deltas are emitted as "message" events.
	if !strings.Contains(body, "event: message") {
		t.Fatalf("body missing 'message' events: %.300s", body)
	}
	if !strings.Contains(body, `"delta":"Hel"`) || !strings.Contains(body, `"delta":"lo"`) {
		t.Fatalf("body missing the text deltas: %.300s", body)
	}
	// The "done" event carries the conversation id + the full reply + the usage.
	if !strings.Contains(body, "event: done") {
		t.Fatalf("body missing the 'done' event: %.300s", body)
	}
	if !strings.Contains(body, `"reply":"Hello"`) {
		t.Fatalf("done event missing the full reply: %.300s", body)
	}
	// The usage (from the final chunk) is reported.
	if !strings.Contains(body, `"total_tokens":9`) {
		t.Fatalf("done event missing the usage: %.300s", body)
	}
	if !strings.Contains(body, convID) {
		t.Fatalf("done event missing the conversation id: %.300s", body)
	}
}

// TestAdminToolsConfigEndpoints — the admin Tools page's read/save API:
// GET returns the web_search settings with the API key masked (has_api_key;
// the key itself never leaves the server) + the build's provider registry
// (the UI's select renders from it); POST saves the area (persisted +
// hot-reloaded) and a blank API key keeps the stored one (the masked-secret
// pattern, same as email).
func TestAdminToolsConfigEndpoints(t *testing.T) {
	a, _ := configApp(t)

	// Admin read: the tools area is present, the key is masked, the provider
	// registry is exposed, and a fresh config reads as disabled.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("config read code = %d", w.Code)
	}
	var got struct {
		Tools map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	ws, ok := got.Tools["web_search"].(map[string]any)
	if !ok {
		t.Fatalf("tools read missing web_search: %+v", got.Tools)
	}
	if _, has := ws["has_api_key"]; !has {
		t.Fatalf("web_search read missing has_api_key: %+v", ws)
	}
	if _, has := ws["api_key"]; has {
		t.Fatalf("web_search read must not return the API key: %+v", ws)
	}
	if _, has := got.Tools["providers"]; !has {
		t.Fatalf("tools read missing the provider registry: %+v", got.Tools)
	}
	if ws["enabled"] != false {
		t.Fatalf("a fresh config's web_search should read as disabled: %+v", ws)
	}

	// Save: enable + provider + key. It persists to disk AND hot-reloads the
	// running agent (the next tool call / turn sees it — the npCfg pattern).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"tools": map[string]any{
		"web_search": map[string]any{"enabled": true, "provider": "exa", "api_key": "exa-123"},
	}}))
	if w.Code != http.StatusOK {
		t.Fatalf("tools save code = %d body=%s", w.Code, w.Body.String())
	}
	var saveRes struct {
		OK       bool `json:"ok"`
		Reloaded bool `json:"reloaded"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &saveRes)
	if !saveRes.OK || !saveRes.Reloaded {
		t.Fatalf("save result = %+v, want ok+reloaded", saveRes)
	}
	onDisk, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !onDisk.Tools.WebSearch.Enabled || onDisk.Tools.WebSearch.APIKey != "exa-123" {
		t.Fatalf("the tools save did not persist: %+v", onDisk.Tools.WebSearch)
	}
	a.cfgMu.Lock()
	live := a.cfg.Tools.WebSearch
	a.cfgMu.Unlock()
	if !live.Enabled || live.APIKey != "exa-123" {
		t.Fatalf("the hot reload did not apply the tools save: %+v", live)
	}

	// Save again with a blank key + disabled: the stored key is preserved
	// (masked-secret pattern) and the disable persists.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"tools": map[string]any{
		"web_search": map[string]any{"enabled": false, "provider": "exa", "api_key": ""},
	}}))
	if w.Code != http.StatusOK {
		t.Fatalf("tools disable save code = %d body=%s", w.Code, w.Body.String())
	}
	after, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Tools.WebSearch.APIKey != "exa-123" {
		t.Fatalf("a blank key should keep the stored one, got %q", after.Tools.WebSearch.APIKey)
	}
	if after.Tools.WebSearch.Enabled {
		t.Fatal("the disable save should have persisted")
	}

	// The read now reports has_api_key=true (the form's "saved — leave blank
	// to keep" placeholder).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/config", nil))
	var got2 struct {
		Tools map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got2)
	if got2.Tools["web_search"].(map[string]any)["has_api_key"] != true {
		t.Fatalf("has_api_key should be true once a key is saved: %+v", got2.Tools["web_search"])
	}

	// An invalid tools area (enabled, no provider/key) is rejected + does not
	// clobber the saved one.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"tools": map[string]any{
		"web_search": map[string]any{"enabled": true},
	}}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an invalid tools save code = %d, want 400", w.Code)
	}
	final, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if final.Tools.WebSearch.APIKey != "exa-123" {
		t.Fatalf("a rejected tools save clobbered the saved key: %+v", final.Tools.WebSearch)
	}
}
