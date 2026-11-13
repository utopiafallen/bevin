package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
		Models []config.Model  `json:"models"`
		Email  map[string]any  `json:"email"`
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
