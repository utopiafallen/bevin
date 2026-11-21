package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
)

func TestIsModelUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"openai 404 does not exist", errors.New(`openai: 404 Not Found: {"error":{"message":"The model 'gpt-9' does not exist"}}`), true},
		{"anthropic model not found", errors.New(`anthropic: 404 Not Found: {"type":"error","message":"model: Not found"}`), true},
		{"llama unknown model", errors.New(`llama: 404 Not Found: unknown model 'foo'`), true},
		{"generic 404 no model", errors.New(`openai: 404 Not Found: page not found`), false},
		{"rate limit", errors.New(`openai: 429 Too Many Requests: rate limit exceeded`), false},
		{"timeout", errors.New(`context deadline exceeded`), false},
		{"connection refused", errors.New(`openai: Post http://x: dial tcp: connection refused`), false},
		{"500 server error", errors.New(`openai: 500 Internal Server Error: boom`), false},
	}
	for _, c := range cases {
		if got := isModelUnavailable(c.err); got != c.want {
			t.Errorf("%s: isModelUnavailable = %v, want %v", c.name, got, c.want)
		}
	}
}

func newTestAgent(t *testing.T) (*Agent, *config.ServerConfig, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.ServerConfig{
		Version: config.ConfigVersion,
		Auth:    config.Auth{Mode: "internal", SessionSecret: "test-secret"},
		Models: []config.Model{
			{ID: "m1", Provider: "openai", BaseURL: "http://u", ModelID: "x", DisplayName: "X", MaxContext: 1000, Slots: 1, Enabled: true},
			{ID: "m2", Provider: "openai", BaseURL: "http://u", ModelID: "y", DisplayName: "Y", MaxContext: 1000, Slots: 1, Enabled: true},
		},
	}
	a, err := New(cfg, dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, cfg, dir
}

func TestSetModelAutoDisabledPersists(t *testing.T) {
	a, cfg, dir := newTestAgent(t)
	defer a.Close()

	// Initially no model is auto-disabled.
	if cfg.Models[0].AutoDisabled || cfg.Models[1].AutoDisabled {
		t.Fatal("expected no model to start auto-disabled")
	}

	// Auto-disable m1 -> the in-memory config + the persisted config update.
	a.SetModelAutoDisabled("m1", true)
	if !cfg.Models[0].AutoDisabled {
		t.Fatal("m1 should be auto-disabled in memory")
	}
	if cfg.Models[1].AutoDisabled {
		t.Fatal("m2 should not be auto-disabled")
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load after auto-disable: %v", err)
	}
	if !loaded.Models[0].AutoDisabled || loaded.Models[1].AutoDisabled {
		t.Fatalf("persisted state = %v/%v, want true/false", loaded.Models[0].AutoDisabled, loaded.Models[1].AutoDisabled)
	}

	// Re-enable m1 (a later success) -> persisted state clears.
	a.SetModelAutoDisabled("m1", false)
	loaded, err = config.Load(dir)
	if err != nil {
		t.Fatalf("Load after re-enable: %v", err)
	}
	if loaded.Models[0].AutoDisabled {
		t.Fatal("m1 should be re-enabled (auto-disabled cleared) in persisted config")
	}

	// Unknown model -> no-op (no panic, no change).
	a.SetModelAutoDisabled("nope", true)
	if cfg.Models[0].AutoDisabled || cfg.Models[1].AutoDisabled {
		t.Fatal("unknown model should not change state")
	}
}

func TestModelsFiltersAutoDisabled(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	// Both models available -> both returned.
	if got := len(a.Models()); got != 2 {
		t.Fatalf("Models() = %d, want 2", got)
	}
	// Auto-disable m1 -> only m2 returned.
	a.SetModelAutoDisabled("m1", true)
	got := a.Models()
	if len(got) != 1 || got[0].ID != "m2" {
		t.Fatalf("Models() after auto-disable = %+v, want only m2", got)
	}
	// AllModels still returns both (with the auto-disabled state).
	if got := len(a.AllModels()); got != 2 {
		t.Fatalf("AllModels() = %d, want 2", got)
	}
}

var _ = fmt.Sprintf

// TestConversationMessagesEmptyReply pins the display rules: an empty
// assistant row that carries a P7 tool call is the model's tool-call turn —
// it is INCLUDED in the display so the calls have a place in the flow (the
// UI renders only the indicator rows, no bubble; the turn's text lands on
// the final reply row) — while an empty row WITHOUT tool calls is a
// genuine empty reply and must stay in the history (the UI renders it as
// "(empty reply)") — otherwise the user's question looks unanswered after
// a page reload (2026-11-19: the blanket empty-row filter made the
// streamed-tool-call era's empty replies vanish from restored chat logs).
func TestConversationMessagesEmptyReply(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := us.CreateConversation("t", "")
	if err != nil {
		t.Fatal(err)
	}
	// A question + a genuine empty reply (no tool calls).
	if _, err := us.AppendMessage(conv.ID, roleUser, "hi", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(conv.ID, roleAssistant, "", "the model thought but said nothing", "", nil); err != nil {
		t.Fatal(err)
	}
	// The tool-call turn: an empty row WITH a P7 call (indicator rows only,
	// no bubble), then the turn's final text reply.
	if _, err := us.AppendMessage(conv.ID, roleUser, "do it", "", "", nil); err != nil {
		t.Fatal(err)
	}
	toolRow, err := us.AppendMessage(conv.ID, roleAssistant, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendToolCall(toolRow.ID, "fake_tool", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(conv.ID, roleAssistant, "done", "", "", nil); err != nil {
		t.Fatal(err)
	}

	msgs, err := a.ConversationMessages("u1", conv.ID)
	if err != nil {
		t.Fatalf("ConversationMessages: %v", err)
	}
	var rows []string
	var emptyReasoning string
	var toolCallsAt []int
	for i, m := range msgs {
		rows = append(rows, m.Role+":"+m.Content)
		// The genuine empty reply is the empty row WITHOUT tool calls (the
		// tool-call row is also empty — it carries only the indicator rows).
		if m.Role == roleAssistant && m.Content == "" && len(m.ToolCalls) == 0 {
			emptyReasoning = m.Reasoning
		}
		if len(m.ToolCalls) > 0 {
			toolCallsAt = append(toolCallsAt, i)
		}
	}
	want := []string{"user:hi", "assistant:", "user:do it", "assistant:", "assistant:done"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %v (the tool-call row stays — it carries the indicator rows; the genuine empty reply stays too)", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %q, want %q (rows = %v)", i, rows[i], want[i], rows)
		}
	}
	// The empty reply keeps its reasoning trace (the UI's thinking block).
	if emptyReasoning != "the model thought but said nothing" {
		t.Fatalf("empty reply's reasoning = %q, want the stored trace", emptyReasoning)
	}
	// Exactly the tool-call row carries calls, and they are the stored P7
	// rows (name + success).
	if len(toolCallsAt) != 1 || toolCallsAt[0] != 3 {
		t.Fatalf("tool_calls on rows %v, want only the tool-call row (index 3)", toolCallsAt)
	}
	got := msgs[3].ToolCalls
	if len(got) != 1 || got[0].Name != "fake_tool" {
		t.Fatalf("tool-call row's calls = %+v, want [fake_tool]", got)
	}
}

// TestCreateConversationModelAndEffort pins the new-conversation create
// path: model resolution (a per-user agent.toml default beats the server
// default while it is still available, and falls back when it is not),
// the explicit model pin (an unavailable model is rejected), and the
// reasoning-effort pin (validated against the model's profile levels and
// persisted; a bad level is rejected with the supported ones named).
func TestCreateConversationModelAndEffort(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.ServerConfig{
		Version: config.ConfigVersion,
		Auth:    config.Auth{Mode: "internal", SessionSecret: "test-secret"},
		Models: []config.Model{
			{ID: "m1", Provider: "openai", BaseURL: "http://u", ModelID: "x", DisplayName: "X", MaxContext: 1000, Slots: 1, Enabled: true},
			{ID: "m2", Provider: "openai", BaseURL: "http://u", ModelID: "y", DisplayName: "Y", MaxContext: 1000, Slots: 1, Enabled: true, Profile: "qwen3.8"},
		},
	}
	a, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	getModel := func(id string) (string, string) {
		t.Helper()
		us, err := a.userStore("u1")
		if err != nil {
			t.Fatal(err)
		}
		c, err := us.GetConversation(id)
		if err != nil {
			t.Fatal(err)
		}
		return c.ActiveModelID, c.ReasoningEffort
	}

	// No user default: the server default (first available) is taken, no
	// effort stored.
	id, err := a.CreateConversation(context.Background(), "u1", "plain", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m, e := getModel(id); m != "m1" || e != "" {
		t.Fatalf("plain create = (%q, %q), want (m1, \"\")", m, e)
	}

	// An explicit model + an effort its profile supports: both are
	// honored + persisted.
	id, err = a.CreateConversation(context.Background(), "u1", "pinned", "m2", "medium")
	if err != nil {
		t.Fatalf("pinned create: %v", err)
	}
	if m, e := getModel(id); m != "m2" || e != "medium" {
		t.Fatalf("pinned create = (%q, %q), want (m2, medium)", m, e)
	}

	// An unavailable model is rejected (nothing is created).
	before := func() int {
		us, _ := a.userStore("u1")
		convos, _ := us.ListConversations()
		return len(convos)
	}()
	if _, err := a.CreateConversation(context.Background(), "u1", "bad", "m-nope", ""); err == nil {
		t.Fatal("create with an unavailable model should fail")
	}
	after := func() int {
		us, _ := a.userStore("u1")
		convos, _ := us.ListConversations()
		return len(convos)
	}()
	if before != after {
		t.Fatalf("a rejected create still wrote a conversation (%d -> %d)", before, after)
	}

	// An effort the model's profile doesn't support is rejected, naming
	// the supported levels.
	if _, err := a.CreateConversation(context.Background(), "u1", "bad-effort", "m2", "ultra"); err == nil || !strings.Contains(err.Error(), "low, medium, xhigh") {
		t.Fatalf("bad effort err = %v, want a rejection naming the supported levels", err)
	}
	// ... and so is an effort for a model that takes none.
	if _, err := a.CreateConversation(context.Background(), "u1", "bad-effort-2", "m1", "low"); err == nil {
		t.Fatal("an effort for a model that takes none should fail")
	}

	// The per-user agent.toml default (m2) beats the server default while
	// it is available.
	if err := config.SaveAgent(filepath.Join(dir, "users", "u1"), &config.AgentConfig{DefaultModelID: "m2"}); err != nil {
		t.Fatal(err)
	}
	if got := a.NewConversationModel("u1"); got != "m2" {
		t.Fatalf("NewConversationModel = %q, want the user default m2", got)
	}
	id, err = a.CreateConversation(context.Background(), "u1", "user-default", "", "")
	if err != nil {
		t.Fatalf("create with user default: %v", err)
	}
	if m, _ := getModel(id); m != "m2" {
		t.Fatalf("user-default create = %q, want m2", m)
	}

	// A user default that is no longer available falls back to the server
	// default.
	if err := config.SaveAgent(filepath.Join(dir, "users", "u1"), &config.AgentConfig{DefaultModelID: "m-gone"}); err != nil {
		t.Fatal(err)
	}
	if got := a.NewConversationModel("u1"); got != "m1" {
		t.Fatalf("NewConversationModel with a dead default = %q, want the server default m1", got)
	}
}
