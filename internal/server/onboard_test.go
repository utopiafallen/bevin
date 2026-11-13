package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/agent"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/store"
)

// TestOnboardingFlow — the first-time "Meet Bevin" flow: a regular user gets
// complete=false on first probe; completing it writes the per-user default
// model + completion flag to agent.toml, the display name + email to the
// user row, and seeds the "Meet Bevin" conversation with exactly one welcome
// message. It is idempotent (re-completing reuses the conversation and does
// not duplicate the welcome). Admins are rejected (chatGate); bad model ids
// and emails 400.
func TestOnboardingFlow(t *testing.T) {
	a, cfg := configApp(t)
	if _, err := a.srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}

	// First probe: not complete yet.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/onboarding", "tok-bob", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/onboarding = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"complete":false`) {
		t.Fatalf("want complete=false: %s", w.Body.String())
	}

	// Invalid model id → 400.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/onboarding", "tok-bob", map[string]string{"model_id": "nope"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad model_id = %d, want 400", w.Code)
	}

	// Invalid email → 400.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/onboarding", "tok-bob", map[string]string{"email": "not-an-email"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad email = %d, want 400", w.Code)
	}

	// Admin is rejected (the admin has no user UI).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/api/onboarding", map[string]string{"display_name": "Admin"}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("admin POST /api/onboarding = %d, want 403", w.Code)
	}

	// Complete it.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/onboarding", "tok-bob", map[string]string{
		"display_name": "Bobby", "model_id": "m1", "email": "bob@example.com",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/onboarding = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"complete":true`) || !strings.Contains(w.Body.String(), `"conversation_id"`) {
		t.Fatalf("bad completion response: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Hi Bobby") {
		t.Fatalf("welcome message should address the display name: %s", w.Body.String())
	}

	// agent.toml: per-user default model + completion flag.
	ac, err := config.LoadAgent(filepath.Join(cfg.DataDir, "users", "bob"))
	if err != nil {
		t.Fatal(err)
	}
	if !ac.OnboardingComplete || ac.OnboardingAt == "" || ac.DefaultModelID != "m1" {
		t.Fatalf("agent.toml after onboarding = %+v", ac)
	}

	// User row: display name + email.
	u, err := a.srv.GetUser("bob")
	if err != nil {
		t.Fatal(err)
	}
	if u.DisplayName != "Bobby" || u.Email != "bob@example.com" {
		t.Fatalf("user row after onboarding = %+v", u)
	}

	// The seeded conversation: right title, right model, frozen prompt,
	// exactly one welcome message.
	us, err := store.OpenUser(filepath.Join(cfg.DataDir, "users", "bob"))
	if err != nil {
		t.Fatal(err)
	}
	defer us.Close()
	conv, err := us.FindConversationByTitle("Meet Bevin")
	if err != nil {
		t.Fatalf("Meet Bevin conversation missing: %v", err)
	}
	if conv.ActiveModelID != "m1" {
		t.Fatalf("conversation model = %q, want m1", conv.ActiveModelID)
	}
	if conv.FrozenSystemPrompt == "" {
		t.Fatal("conversation has no frozen system prompt")
	}
	msgs, err := us.ListMessages(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Role != "assistant" || !strings.Contains(msgs[0].Content, "Hi Bobby") {
		t.Fatalf("seeded messages = %+v", msgs)
	}

	// Second probe: complete.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/onboarding", "tok-bob", nil))
	if !strings.Contains(w.Body.String(), `"complete":true`) {
		t.Fatalf("GET after completion = %s", w.Body.String())
	}

	// Idempotent re-completion: same conversation, no duplicated welcome.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/onboarding", "tok-bob", map[string]string{"model_id": "m1"}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), conv.ID) {
		t.Fatalf("re-completion = %d %s", w.Code, w.Body.String())
	}
	msgs, _ = us.ListMessages(conv.ID)
	if len(msgs) != 1 {
		t.Fatalf("welcome message duplicated on re-completion: %d messages", len(msgs))
	}
}

// TestOnboardingSkip — skip completes with the defaults: no name/email
// written, the server-default model, and the conversation is still seeded
// (the user always lands somewhere real).
func TestOnboardingSkip(t *testing.T) {
	a, cfg := configApp(t)
	if _, err := a.srv.CreateUser("dave", "", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-dave", "dave", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/onboarding", "tok-dave", map[string]string{}))
	if w.Code != http.StatusOK {
		t.Fatalf("skip POST = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Hi dave") {
		t.Fatalf("skipped onboarding should still welcome by handle: %s", w.Body.String())
	}
	u, err := a.srv.GetUser("dave")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "" || u.DisplayName != "" {
		t.Fatalf("skip should not write name/email: %+v", u)
	}
	ac, err := config.LoadAgent(filepath.Join(cfg.DataDir, "users", "dave"))
	if err != nil {
		t.Fatal(err)
	}
	if !ac.OnboardingComplete || ac.DefaultModelID != "" {
		t.Fatalf("skip agent.toml = %+v (want complete, blank default model)", ac)
	}
}

// TestOnboardingPerUserDefault — EnsureConversation uses the user's
// onboarding-chosen model for new conversations, and falls back to the
// server default when the stored choice is no longer a configured model.
func TestOnboardingPerUserDefault(t *testing.T) {
	a, cfg := configApp(t)
	if _, err := a.srv.CreateUser("erin", "", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-erin", "erin", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.CreateUser("frank", "", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-frank", "frank", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.CreateUser("gail", "", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-gail", "gail", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}

	// Erin onboarded with the (only) model m1 — new conversations must use
	// m1 even though it is also the server default.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/onboarding", "tok-erin", map[string]string{"model_id": "m1"}))
	if w.Code != http.StatusOK {
		t.Fatalf("erin onboarding = %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-erin", map[string]string{"title": "work"}))
	if w.Code != http.StatusOK {
		t.Fatalf("erin new conversation = %d: %s", w.Code, w.Body.String())
	}
	var conv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}
	us, err := store.OpenUser(filepath.Join(cfg.DataDir, "users", "erin"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := us.GetConversation(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.ActiveModelID != "m1" {
		t.Fatalf("erin conversation model = %q, want m1 (her onboarding choice)", c.ActiveModelID)
	}
	us.Close()

	// Frank stores a default that is no longer configured → falls back to
	// the server default instead of breaking conversation creation.
	frankDir := filepath.Join(cfg.DataDir, "users", "frank")
	if err := os.MkdirAll(frankDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ac := &config.AgentConfig{DefaultModelID: "m-gone"}
	if err := config.SaveAgent(frankDir, ac); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-frank", map[string]string{"title": "work"}))
	if w.Code != http.StatusOK {
		t.Fatalf("frank new conversation = %d (stale default must fall back, not break): %s", w.Code, w.Body.String())
	}
	var fconv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fconv); err != nil {
		t.Fatal(err)
	}
	fus, err := store.OpenUser(frankDir)
	if err != nil {
		t.Fatal(err)
	}
	fc, err := fus.GetConversation(fconv.ID)
	if err != nil {
		t.Fatal(err)
	}
	fus.Close()
	if fc.ActiveModelID != "m1" {
		t.Fatalf("frank conversation model = %q, want m1 (server default fallback)", fc.ActiveModelID)
	}
	// Gail: onboarding state is per-user — Erin is done, Gail is not.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/onboarding", "tok-gail", nil))
	if !strings.Contains(w.Body.String(), `"complete":false`) {
		t.Fatalf("gail onboarding after erin completed = %s (want complete:false)", w.Body.String())
	}
}

// TestOnboardingFailureDoesNotComplete — when conversation seeding fails
// (no available models), the completion flag must NOT be persisted: the user
// stays on the card and can retry (the status keeps reporting complete=false).
func TestOnboardingFailureDoesNotComplete(t *testing.T) {
	a, cfg := configApp(t)
	if _, err := a.srv.CreateUser("hank", "", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-hank", "hank", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	// Swap in an agent with no available models (the only model is
	// auto-disabled), so conversation seeding fails.
	ag2, err := agent.New(&config.ServerConfig{
		DataDir: cfg.DataDir,
		Auth:    cfg.Auth,
		Models: []config.Model{{
			ID: "m1", Provider: "openai", BaseURL: "http://x/v1",
			ModelID: "gpt-4o", Slots: 1, Enabled: true, AutoDisabled: true,
		}},
	}, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	a.ag = ag2

	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/onboarding", "tok-hank", map[string]string{"display_name": "Hank"}))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("seeding failure = %d, want 500: %s", w.Code, w.Body.String())
	}

	// The completion flag must not have been persisted.
	ac, err := config.LoadAgent(filepath.Join(cfg.DataDir, "users", "hank"))
	if err != nil {
		t.Fatal(err)
	}
	if ac.OnboardingComplete {
		t.Fatal("completion flag persisted despite the seeding failure")
	}

	// The status keeps reporting not-complete (retryable).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/onboarding", "tok-hank", nil))
	if !strings.Contains(w.Body.String(), `"complete":false`) {
		t.Fatalf("status after failed completion = %s (want complete:false)", w.Body.String())
	}
}
