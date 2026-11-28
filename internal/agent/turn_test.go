package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// fakeProvider records every Chat request + returns a fixed reply (or cycles
// through canned replies). It implements the provider.Provider interface so the
// Agent can be exercised without a real model backend. When usage is set, every
// Result carries it (so the usage-based compaction path can be exercised).
type fakeProvider struct {
	mu       sync.Mutex
	calls    []provider.ChatRequest
	models   []string // the model argument of every Chat/Stream call (wire-level check)
	replies  []string
	replyIdx int
	usage    *provider.Usage
	// toolCalls, when set, ride on the FIRST Chat/Stream call (a tool-call
	// turn); later calls return the normal reply cycle. Test-only.
	toolCalls []provider.ToolCall
	// summarizeReply, when non-empty, is returned for summarization
	// (compaction) meta-calls — detected by the summarization system prompt
	// (isSummarizeRequest) — WITHOUT consuming the reply cycle, so chat calls
	// see deterministic replies. Test-only.
	summarizeReply string
	// firstCallErr, when non-nil, is returned (and consumed) on the FIRST
	// Chat/Stream call — exercises the mid-loop error path (e.g. the
	// context-overflow backstop). Test-only.
	firstCallErr error
	firstDone    bool
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Chat(_ context.Context, model string, req provider.ChatRequest) (*provider.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	f.models = append(f.models, model)
	first := len(f.calls) == 1
	if first && f.firstCallErr != nil {
		f.firstDone = true
		return nil, f.firstCallErr
	}
	reply := "ok"
	if isSummarizeRequest(req) && f.summarizeReply != "" {
		reply = f.summarizeReply
	} else if len(f.replies) > 0 {
		reply = f.replies[f.replyIdx%len(f.replies)]
		f.replyIdx++
	}
	res := &provider.Result{Text: reply}
	if f.usage != nil {
		res.Usage = f.usage
	}
	if first && len(f.toolCalls) > 0 {
		res.ToolCalls = f.toolCalls
	}
	return res, nil
}

func (f *fakeProvider) Stream(_ context.Context, model string, req provider.ChatRequest, emit func(provider.Chunk) error) (*provider.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.models = append(f.models, model)
	first := len(f.calls) == 1
	if first && f.firstCallErr != nil {
		f.firstDone = true
		f.mu.Unlock()
		return nil, f.firstCallErr
	}
	reply := "ok"
	if isSummarizeRequest(req) && f.summarizeReply != "" {
		reply = f.summarizeReply
	} else if len(f.replies) > 0 {
		reply = f.replies[f.replyIdx%len(f.replies)]
		f.replyIdx++
	}
	usage := f.usage
	f.mu.Unlock()
	// Simulate a stream: emit the reply as text deltas + a final chunk that
	// carries the usage (mirrors the real providers, where usage arrives in the
	// last SSE chunk).
	if emit != nil && reply != "" {
		mid := len(reply) / 2
		parts := []string{reply[:mid], reply[mid:]}
		for _, part := range parts {
			if err := emit(provider.Chunk{TextDelta: part}); err != nil {
				return &provider.Result{Text: reply}, err
			}
		}
		if usage != nil {
			if err := emit(provider.Chunk{Usage: usage, Done: true}); err != nil {
				return &provider.Result{Text: reply}, err
			}
		}
	}
	res := &provider.Result{Text: reply}
	if usage != nil {
		res.Usage = usage
	}
	if first && len(f.toolCalls) > 0 {
		res.ToolCalls = f.toolCalls
	}
	return res, nil
}
func (f *fakeProvider) Models(_ context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (f *fakeProvider) Embed(_ context.Context, _ string, _ []string) ([][]float32, error) {
	return nil, nil
}

func (f *fakeProvider) lastCall() provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeProvider) lastModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.models[len(f.models)-1]
}

func (f *fakeProvider) allModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.models...)
}

func (f *fakeProvider) allCalls() []provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.ChatRequest{}, f.calls...)
}

// isSummarizeRequest reports whether a Chat request is a compaction
// (summarization) meta-call — the summarizer uses its own system prompt
// (the "context summary" phrasing), which is the same discriminator the
// conversation-id and effort tests use.
func isSummarizeRequest(req provider.ChatRequest) bool {
	return len(req.System) > 0 && strings.Contains(req.System[0].Text, "context summary")
}

// TestTurnUsesProviderModelName — the conversation stores the INTERNAL model
// id (a slug derived from the upstream name), but every wire request must
// carry the VERBATIM upstream model name. A model whose upstream name
// contains / . : _ or uppercase (e.g. a llama.cpp GGUF ref like
// "unsloth/Qwen3.8-27B-GGUF:UD-IQ3_S") slugifies to a different string —
// sending the slug made the provider answer "model not found" on the first
// real chat. The summarization (compaction) call is checked too: it is a
// provider call and must carry the upstream name as well.
func TestTurnUsesProviderModelName(t *testing.T) {
	raw := "unsloth/Qwen3.8-27B-GGUF:UD-IQ3_S"
	slug := config.ModelIDSlug(raw)
	if slug == raw {
		t.Fatalf("test precondition: slug %q must differ from %q", slug, raw)
	}
	dir := t.TempDir()
	cfg := &config.ServerConfig{
		Version: config.ConfigVersion,
		Auth:    config.Auth{Mode: "internal", SessionSecret: "test-secret"},
		Models: []config.Model{{
			ID: slug, Provider: "openai", BaseURL: "http://u", ModelID: raw,
			DisplayName: "Q", MaxContext: 1000, Slots: 1, Enabled: true,
		}},
	}
	a, err := New(cfg, dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	// Reports 900 total tokens (over the 800 budget) so the post-turn
	// compaction fires once the conversation has > KeepLastMessages
	// messages — exercising the summarization provider call too.
	fp := &fakeProvider{usage: &provider.Usage{PromptTokens: 500, CompletionTokens: 400, TotalTokens: 900}}
	a.providers[slug] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	// The conversation's active model is the INTERNAL id (the slug).
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.ActiveModelID != slug {
		t.Fatalf("active model = %q, want the internal slug %q", conv.ActiveModelID, slug)
	}
	// Four user turns = eight messages: enough (> KeepLastMessages) for a
	// summarization call to happen (fires from the 3rd turn's post-compact
	// onward).
	for i := 0; i < 4; i++ {
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi", nil); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
	// Every wire call — the turns AND the summarization — carried the
	// verbatim upstream name, never the slug.
	got := fp.allModels()
	if len(got) < 5 {
		t.Fatalf("%d provider calls, want >= 5 (4 turns + >= 1 summarization)", len(got))
	}
	for i, m := range got {
		if m != raw {
			t.Fatalf("provider call %d used model %q, want the upstream name %q (the internal id is only for bookkeeping)", i, m, raw)
		}
	}
	// The store kept the INTERNAL id on the messages (bookkeeping is by id,
	// wire is by name).
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if m.ModelID != nil && *m.ModelID != slug {
			t.Fatalf("stored message model id = %q, want the internal id %q", *m.ModelID, slug)
		}
	}
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestBuildRequest selects the frozen system prompt + the latest summary (as a
// second system part) + only the raw turns since that summary.
func TestBuildRequest(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	msgs := []store.Message{
		{Role: roleUser, Content: "hello", Seq: 0},
		{Role: roleAssistant, Content: "hi", Seq: 1},
		{Role: roleSummary, Content: "prev summary", Seq: 2},
		{Role: roleUser, Content: "how are you", Seq: 3},
		{Role: roleAssistant, Content: "fine", Seq: 4},
	}
	req, tokens := buildRequest(conv, msgs, profile.Default(), nil, "")
	if len(req.System) != 2 {
		t.Fatalf("System = %d parts, want 2 (frozen prompt + summary)", len(req.System))
	}
	if req.System[0].Text != "You are Bevin." {
		t.Fatalf("System[0] = %q, want the frozen prompt", req.System[0].Text)
	}
	if !strings.Contains(req.System[1].Text, "prev summary") {
		t.Fatalf("System[1] = %q, want the summary folded in", req.System[1].Text)
	}
	// Only the turns AFTER the summary (seq 3, 4) are in the messages.
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2 (the turns since the summary)", len(req.Messages))
	}
	if req.Messages[0].Role != provider.RoleUser || req.Messages[0].Text != "how are you" {
		t.Fatalf("Messages[0] = %+v, want the user turn after the summary", req.Messages[0])
	}
	if req.Messages[1].Role != provider.RoleAssistant || req.Messages[1].Text != "fine" {
		t.Fatalf("Messages[1] = %+v, want the assistant turn after the summary", req.Messages[1])
	}
	if tokens <= 0 {
		t.Fatalf("tokens = %d, want > 0", tokens)
	}
}

// TestBuildRequestNoSummary uses the frozen prompt + all raw turns when no
// summary exists yet.
func TestBuildRequestNoSummary(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	msgs := []store.Message{
		{Role: roleUser, Content: "hello", Seq: 0},
		{Role: roleAssistant, Content: "hi", Seq: 1},
	}
	req, _ := buildRequest(conv, msgs, profile.Default(), nil, "")
	if len(req.System) != 1 {
		t.Fatalf("System = %d parts, want 1 (no summary yet)", len(req.System))
	}
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2 (all turns)", len(req.Messages))
	}
}

// TestBuildRequestReasoningEffort verifies the request builder's effort gate:
// the conversation's stored reasoning-effort choice is sent only when the
// active model's profile supports it — a supported level is sent verbatim,
// while an unsupported profile, an unknown level (stale after a profile
// reassignment), and an empty choice all send nothing (the provider's own
// default then applies; nothing invalid ever goes on the wire).
func TestBuildRequestReasoningEffort(t *testing.T) {
	q, err := profile.Get("qwen3.8")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []store.Message{{Role: roleUser, Content: "hi", Seq: 0}}

	// Supported level: sent verbatim.
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin.", ReasoningEffort: "xhigh"}
	req, _ := buildRequest(conv, msgs, q, nil, "")
	if req.ReasoningEffort != "xhigh" {
		t.Fatalf("supported: ReasoningEffort = %q, want %q", req.ReasoningEffort, "xhigh")
	}

	// A profile with no effort support: dropped (even a supported-looking value).
	req, _ = buildRequest(conv, msgs, profile.Default(), nil, "")
	if req.ReasoningEffort != "" {
		t.Fatalf("no support: ReasoningEffort = %q, want \"\"", req.ReasoningEffort)
	}

	// A level the profile doesn't know (stale after a reassignment): dropped.
	convUnknown := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin.", ReasoningEffort: "high"}
	req, _ = buildRequest(convUnknown, msgs, q, nil, "")
	if req.ReasoningEffort != "" {
		t.Fatalf("unknown level: ReasoningEffort = %q, want \"\"", req.ReasoningEffort)
	}

	// No stored choice: nothing to send.
	convNone := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	req, _ = buildRequest(convNone, msgs, q, nil, "")
	if req.ReasoningEffort != "" {
		t.Fatalf("no choice: ReasoningEffort = %q, want \"\"", req.ReasoningEffort)
	}
}

// TestTurnCarriesReasoningEffort drives the full turn path: the conversation's
// stored effort reaches the provider request only when the model's assigned
// profile supports it (resolved per turn from live config — a profile
// reassignment applies from the next turn, no restart), and the summarization
// (compaction) call — also a provider call — runs under the same effort.
func TestTurnCarriesReasoningEffort(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	// m1 gets the qwen3.8 profile: it supports the per-conversation effort.
	cfg.Models[0].Profile = "qwen3.8"
	fp := &fakeProvider{}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	if err := us.SetConversationReasoningEffort(convID, "xhigh"); err != nil {
		t.Fatal(err)
	}
	// The conversation row round-trips the choice.
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ReasoningEffort != "xhigh" {
		t.Fatalf("stored ReasoningEffort = %q, want %q", conv.ReasoningEffort, "xhigh")
	}
	if _, _, err := a.Turn(context.Background(), "u1", convID, "hi", nil); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got := fp.lastCall().ReasoningEffort; got != "xhigh" {
		t.Fatalf("request ReasoningEffort = %q, want %q (the conversation's choice; the model's profile supports it)", got, "xhigh")
	}

	// A level the profile doesn't know is dropped (a stale choice must never
	// go on the wire).
	if err := us.SetConversationReasoningEffort(convID, "high"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Turn(context.Background(), "u1", convID, "hi", nil); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got := fp.lastCall().ReasoningEffort; got != "" {
		t.Fatalf("request ReasoningEffort = %q, want \"\" (the profile doesn't support %q)", got, "high")
	}

	// Live reassignment to general: the profile takes no effort, so even a
	// supported value is dropped from the next turn on.
	cfg.Models[0].Profile = "general"
	if err := us.SetConversationReasoningEffort(convID, "low"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Turn(context.Background(), "u1", convID, "hi", nil); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got := fp.lastCall().ReasoningEffort; got != "" {
		t.Fatalf("request ReasoningEffort = %q, want \"\" (general takes no effort)", got)
	}

	// The summarization (compaction) call is a provider call too — it must run
	// under the same effort as the chat turns. The fake reports 900 total
	// tokens every call (over the 800 budget), so the post-turn compaction
	// fires once the conversation has more than KeepLastMessages raw
	// messages.
	cfg.Models[0].Profile = "qwen3.8"
	fp2 := &fakeProvider{usage: &provider.Usage{PromptTokens: 500, CompletionTokens: 400, TotalTokens: 900}}
	a.providers["m1"] = fp2
	if err := us.SetConversationReasoningEffort(convID, "medium"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi", nil); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
	calls := fp2.allCalls()
	if len(calls) < 5 {
		t.Fatalf("%d provider calls, want >= 5 (4 chat turns + >= 1 summarization)", len(calls))
	}
	// Every provider call in this window — the chat turns AND the
	// summarization compaction calls — carries the same effort.
	for i, c := range calls {
		if c.ReasoningEffort != "medium" {
			t.Fatalf("provider call %d ReasoningEffort = %q, want %q (chat and summarization calls both run under the conversation's effort)", i, c.ReasoningEffort, "medium")
		}
	}
	// At least one call was a summarization (compaction) call — proving the
	// compaction path was exercised and inherited the effort (not just the
	// chat path). The summarizer uses its own meta system prompt.
	var sawSummary bool
	for _, c := range calls {
		if len(c.System) > 0 && strings.Contains(c.System[0].Text, "context summary") {
			sawSummary = true
			break
		}
	}
	if !sawSummary {
		t.Fatalf("no summarization call among %d provider calls — the compaction path was not exercised", len(calls))
	}
}

// TestTurnCarriesConversationID drives the full turn path: the conversation's
// completions carry the conversation's id (the value the request builder sets
// verbatim, which the OAI wire sends as conversation_id for a llama.cpp proxy
// to save/load the slot's KV cache per conversation), while the internal
// summarization (compaction) meta-calls deliberately carry NO id — their
// prompt has its own system prompt, so it is not a prefix of the
// conversation's prompt (a proxy keyed on the id would load a mismatched
// cache or save over the conversation's reusable one).
func TestTurnCarriesConversationID(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// Reports 900 total tokens (over the 800 budget) so the post-turn
	// compaction fires — exercising the meta-call path too.
	fp := &fakeProvider{usage: &provider.Usage{PromptTokens: 500, CompletionTokens: 400, TotalTokens: 900}}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	// Four turns: enough for a summarization call among the provider calls.
	for i := 0; i < 4; i++ {
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi", nil); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
	calls := fp.allCalls()
	if len(calls) < 5 {
		t.Fatalf("%d provider calls, want >= 5 (4 chat turns + >= 1 summarization)", len(calls))
	}
	var sawChat, sawSummary bool
	for i, c := range calls {
		// The summarizer's meta system prompt is the discriminator (the
		// same check the effort test uses).
		if len(c.System) > 0 && strings.Contains(c.System[0].Text, "context summary") {
			sawSummary = true
			if c.ConversationID != "" {
				t.Fatalf("provider call %d: the summarization meta-call carries ConversationID %q, want empty (its prompt is not a prefix of the conversation's prompt)", i, c.ConversationID)
			}
			continue
		}
		sawChat = true
		if c.ConversationID != convID {
			t.Fatalf("provider call %d: ConversationID = %q, want %q (the conversation's id, verbatim)", i, c.ConversationID, convID)
		}
	}
	if !sawChat || !sawSummary {
		t.Fatalf("test did not exercise both paths (sawChat=%v sawSummary=%v)", sawChat, sawSummary)
	}
}

// TestTurnSummarizesOnBudget drives a conversation past the summarization
// budget and verifies a chained summary is produced + stored, and that the
// next request uses the summary (not the full history).
func TestTurnSummarizesOnBudget(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	// m1's MaxContext is 1000 in the test config -> budget = 800 tokens.
	if cfg.Models[0].MaxContext != 1000 {
		t.Fatalf("test precondition: m1 MaxContext = %d, want 1000", cfg.Models[0].MaxContext)
	}
	fp := &fakeProvider{}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	// Each user message is ~100 chars (~25 tokens); the reply is small. The
	// prompt crosses the 800-token budget after ~30 turns, so run well past it.
	for i := 0; i < 45; i++ {
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hello "+strings.Repeat("x", 100), nil); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
	// A summary was produced + stored.
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var summaries int
	var fullMsgs int
	for _, m := range msgs {
		switch m.Role {
		case roleSummary:
			summaries++
		case roleUser, roleAssistant, roleTool:
			fullMsgs++
		}
	}
	if summaries == 0 {
		t.Fatalf("no summary produced after %d turns (summarization trigger did not fire); %d messages", 45, len(msgs))
	}
	// Full history is retained (the user + assistant turns are still in the store).
	if fullMsgs < 45 {
		t.Fatalf("full history not retained: %d user/assistant turns, want >= 45", fullMsgs)
	}
	// The latest request used the summary (a second system part) + a small set of
	// recent turns (not the full history).
	last := fp.lastCall()
	if len(last.System) < 2 {
		t.Fatalf("latest request System = %d parts, want >= 2 (frozen prompt + summary)", len(last.System))
	}
	if len(last.Messages) >= 45 {
		t.Fatalf("latest request sent %d messages, want fewer (compacted into the summary)", len(last.Messages))
	}
}

// TestTurnCompactsOnReportedUsage verifies the compaction trigger uses the
// API-reported usage (res.Usage) when present — not the char/4 estimate. The
// fake provider reports TotalTokens=900 (over the 800 budget) while the actual
// messages are tiny (the estimate would be far under budget), so the summarization
// fires only because of the reported usage.
func TestTurnCompactsOnReportedUsage(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	if cfg.Models[0].MaxContext != 1000 {
		t.Fatalf("test precondition: m1 MaxContext = %d, want 1000", cfg.Models[0].MaxContext)
	}
	// Reports 900 total tokens (prompt + completion) every call — over the 800
	// budget, while the actual messages are a couple of chars (the estimate is
	// negligible).
	fp := &fakeProvider{usage: &provider.Usage{PromptTokens: 500, CompletionTokens: 400, TotalTokens: 900}}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi", nil); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
	// A summary was produced (the trigger fired on the reported usage, not the
	// tiny estimate).
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var summaries int
	for _, m := range msgs {
		if m.Role == roleSummary {
			summaries++
		}
	}
	if summaries == 0 {
		t.Fatalf("no summary produced — the usage-based trigger did not fire (%d messages)", len(msgs))
	}
}

// TestTurnNoUsageFallsBackToEstimate verifies the estimator is used when the
// backend omits usage (the pre-existing estimate-driven path still works). This
// is covered end-to-end by TestTurnSummarizesOnBudget (no usage set on the fake
// provider), but asserted explicitly here for the no-usage branch.
func TestTurnNoUsageFallsBackToEstimate(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fp := &fakeProvider{} // no usage
	a.providers["m1"] = fp
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	// No error + a reply across turns (the estimate-driven compaction path).
	for i := 0; i < 5; i++ {
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi "+strings.Repeat("y", 200), nil); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
}

// TestBuildRequestTimestampUserOnly verifies that the completion request
// stamps USER messages only: the user's turn carries the UTC timestamp
// (RFC3339, without nanoseconds) and the assistant's earlier reply is sent
// with its plain stored content (stamped replies confused the model; the UI
// shows both messages' timestamps from the stored created_at).
func TestBuildRequestTimestampUserOnly(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	msgs := []store.Message{
		{Role: roleUser, Content: "hello", Seq: 0, CreatedAt: "2026-11-12T10:30:00.123456789Z"},
		{Role: roleAssistant, Content: "hi", Seq: 1, CreatedAt: "2026-11-12T10:30:05Z"},
	}
	req, _ := buildRequest(conv, msgs, profile.Default(), nil, "")
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	// The user's message is stamped (RFC3339, UTC, without the nanoseconds).
	if req.Messages[0].Text != "[2026-11-12T10:30:00Z] hello" {
		t.Fatalf("Messages[0] = %q, want the user message stamped", req.Messages[0].Text)
	}
	// The assistant's reply is plain (no timestamp prefix).
	if req.Messages[1].Text != "hi" {
		t.Fatalf("Messages[1] = %q, want the plain reply (no timestamp)", req.Messages[1].Text)
	}
}

// TestBuildRequestWithReasoning verifies the request builder replays an
// assistant's stored reasoning in the neutral message's Reasoning field (the
// provider encodes it in the API's native field — OAI wire:
// reasoning_content), while the message TEXT stays the plain reply (user
// turns carry the timestamp prefix, assistant turns don't — default on,
// the profile's ReasoningInTranscript), and the user message is stamped.
func TestBuildRequestWithReasoning(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	msgs := []store.Message{
		{Role: roleUser, Content: "solve it", Seq: 0, CreatedAt: "2026-11-12T10:30:00Z"},
		{Role: roleAssistant, Content: "the answer is 42", Reasoning: "step one: read the problem\nstep two: compute", Seq: 1, CreatedAt: "2026-11-12T10:30:05Z"},
	}
	req, _ := buildRequest(conv, msgs, profile.Default(), nil, "")
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Text != "[2026-11-12T10:30:00Z] solve it" {
		t.Fatalf("Messages[0] = %q, want the stamped user turn", req.Messages[0].Text)
	}
	if req.Messages[0].Reasoning != "" {
		t.Fatalf("Messages[0].Reasoning = %q, want empty (user turn)", req.Messages[0].Reasoning)
	}
	// The reply text is plain (content only — no timestamp prefix, and the
	// reasoning is NOT framed into the text).
	if req.Messages[1].Text != "the answer is 42" {
		t.Fatalf("Messages[1] = %q, want the plain reply (no timestamp, no reasoning in the text)", req.Messages[1].Text)
	}
	if req.Messages[1].Reasoning != "step one: read the problem\nstep two: compute" {
		t.Fatalf("Messages[1].Reasoning = %q, want the stored trace (the API's native field)", req.Messages[1].Reasoning)
	}
}

// TestAssistantFacingNoReasoningWhenDisabled verifies the override path: with
// includeReasoning=false (a profile whose ReasoningInTranscript is off) the
// Reasoning field is left empty even when the message carries a stored
// trace, and a no-trace message is identical either way.
func TestAssistantFacingNoReasoningWhenDisabled(t *testing.T) {
	m := store.Message{Role: roleAssistant, Content: "hi", Reasoning: "internal thought", CreatedAt: "2026-11-12T10:30:05Z"}
	got := assistantFacing(m, false)
	if got.Reasoning != "" {
		t.Fatalf("Reasoning = %q, want empty (override disabled)", got.Reasoning)
	}
	if got.Text != "hi" {
		t.Fatalf("Text = %q, want the plain reply (assistant turns carry no timestamp)", got.Text)
	}
	// An assistant message without a trace is identical either way (the model
	// didn't think, or thinking was disabled — the field is simply absent).
	m2 := store.Message{Role: roleAssistant, Content: "hi", CreatedAt: "2026-11-12T10:30:05Z"}
	if assistantFacing(m2, true).Reasoning != "" || assistantFacing(m2, false).Reasoning != "" {
		t.Fatal("no-trace message must carry no reasoning regardless of the flag")
	}
}

// TestTurnSendsBackPreviousReasoning verifies the full turn path (not just
// buildRequest): after an assistant reply with a stored reasoning trace, the
// NEXT request to the model carries that trace in the message's Reasoning
// field (the "send all reasoning back" default — the provider encodes it in
// the API's native field), the message text stays the plain reply (no
// timestamp prefix on assistant turns, no [thinking] framing), so the model
// keeps continuity with its own earlier thinking in the channel it produced
// it in.
func TestTurnSendsBackPreviousReasoning(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fp := &fakeProvider{}
	a.providers["m1"] = fp
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	// Simulate an earlier turn whose reasoning was persisted.
	model := "m1"
	if _, err := us.AppendMessage(convID, roleUser, "the first question", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(convID, roleAssistant, "the first answer", "the first turn's stored reasoning trace", "", &model); err != nil {
		t.Fatal(err)
	}
	// A new user message: the next request must include the prior reasoning.
	if _, _, err := a.Turn(context.Background(), "u1", convID, "the next question", nil); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	last := fp.lastCall()
	found := false
	for _, m := range last.Messages {
		if m.Reasoning == "the first turn's stored reasoning trace" {
			found = true
			// The text of the same message must NOT carry the trace (no
			// [thinking] framing — it goes in the API's native field only).
			if strings.Contains(m.Text, "the first turn's stored reasoning trace") || strings.Contains(m.Text, "[thinking]") {
				t.Fatalf("message text = %q must not contain the reasoning (it belongs in the API's reasoning field)", m.Text)
			}
		}
	}
	if !found {
		t.Fatalf("next request does not send the prior reasoning back:\n%+v", last.Messages)
	}
	// The persisted reasoning is intact (the send-back does not mutate the store).
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	var sawReasoning bool
	for _, m := range msgs {
		if m.Reasoning == "the first turn's stored reasoning trace" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatal("stored reasoning trace lost from the conversation")
	}
}

// TestCompactPreservesLastMessages verifies a compaction preserves the last
// KeepLastMessages messages verbatim (only the older messages are folded
// into the summary, so the chat resumes exactly where it left off).
func TestCompactPreservesLastMessages(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	if cfg.Models[0].MaxContext != 1000 {
		t.Fatalf("test precondition: m1 MaxContext = %d, want 1000", cfg.Models[0].MaxContext)
	}
	fp := &fakeProvider{}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	// Add enough messages to exceed the budget (each ~100 chars).
	for i := 0; i < 45; i++ {
		if _, err := us.AppendMessage(convID, roleUser, "hello "+strings.Repeat("x", 100), "", "", nil); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
		if _, err := us.AppendMessage(convID, roleAssistant, "hi", "", "", &conv.ActiveModelID); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}
	// Force a compaction (the prompt is well over the budget).
	if err := a.compactIfNeeded(context.Background(), us, "u1", conv, fp, 0, profile.Default(), false, nil); err != nil {
		t.Fatalf("compactIfNeeded: %v", err)
	}
	// The request builder now sends the summary + the last KeepLastMessages
	// messages verbatim — not the full history (the older messages are
	// folded into the summary). The store retains the full history (the
	// summary only changes what the request builder sends).
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var summaries int
	for _, m := range msgs {
		if m.Role == roleSummary {
			summaries++
		}
	}
	if summaries == 0 {
		t.Fatal("no summary produced — the compaction did not fire")
	}
	req, _ := buildRequest(conv, msgs, profile.Default(), nil, "")
	if len(req.System) < 2 {
		t.Fatalf("request System = %d parts, want >= 2 (frozen prompt + summary)", len(req.System))
	}
	if len(req.Messages) != profile.Default().KeepLastMessages {
		t.Fatalf("request sent %d messages, want %d (the last KeepLastMessages verbatim)", len(req.Messages), profile.Default().KeepLastMessages)
	}
}

// TestCompactCountsToolLoopIterationsAsMessages pins the compaction's unit:
// the verbatim tail is the last KeepLastMessages MESSAGES (stored rows), not
// the last user turns. Each tool-loop iteration is its OWN assistant message
// (the row that carries that iteration's tool calls) and counts as one, so a
// long tool loop under a single user message folds the user message (and the
// early iterations) into the summary and keeps only the loop's last
// iterations verbatim.
func TestCompactCountsToolLoopIterationsAsMessages(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	if cfg.Models[0].MaxContext != 1000 {
		t.Fatalf("test precondition: m1 MaxContext = %d, want 1000", cfg.Models[0].MaxContext)
	}
	fp := &fakeProvider{}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	model := conv.ActiveModelID
	// ONE user message + SEVEN tool-loop iterations (each a separate
	// assistant row carrying one completed P7 tool call) = eight messages.
	if _, err := us.AppendMessage(convID, roleUser, "do the thing", "", "", nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		msg, err := us.AppendMessage(convID, roleAssistant, fmt.Sprintf("iteration %d", i), "", "", &model)
		if err != nil {
			t.Fatal(err)
		}
		if err := us.RecordToolCall(msg.ID, "t", `{}`, fmt.Sprintf("result %d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	// Drive the compaction via the API-reported usage (900 > the 800
	// budget) — the content is a few chars, so only the reported number
	// can trigger it.
	if err := a.compactIfNeeded(context.Background(), us, "u1", conv, fp, 900, profile.Default(), false, nil); err != nil {
		t.Fatalf("compactIfNeeded: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var summaries int
	for _, m := range msgs {
		if m.Role == roleSummary {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("summaries = %d, want 1 (the compaction)", summaries)
	}
	tcs, err := toolCallsForMessages(us, msgs)
	if err != nil {
		t.Fatalf("toolCallsForMessages: %v", err)
	}
	req, _ := buildRequest(conv, msgs, profile.Default(), tcs, "")
	// Five message rows (iterations 3..7), each expanded by its one
	// completed tool call into a synthesized role=tool result: ten wire
	// messages. If the slice ran on user turns, all seven iterations (and
	// the user message) would be verbatim — twenty-one wire messages.
	if len(req.Messages) != 10 {
		t.Fatalf("request sent %d wire messages, want 10 (the last 5 message rows, each with its tool result)", len(req.Messages))
	}
	// The fold boundary is ROW-level: the user message and iterations 1-2
	// went into the summary; the tail starts at iteration 3.
	if req.Messages[0].Role != provider.RoleAssistant || req.Messages[0].Text != "iteration 3" {
		t.Fatalf("first wire message = %+v, want the assistant row for iteration 3 (the user message was folded)", req.Messages[0])
	}
	if req.Messages[1].Role != provider.RoleTool || req.Messages[1].Text != "result 3" {
		t.Fatalf("second wire message = %+v, want iteration 3's tool result (the tail's pairs are intact)", req.Messages[1])
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != provider.RoleTool || last.Text != "result 7" {
		t.Fatalf("last wire message = %+v, want iteration 7's tool result (the loop's end is verbatim)", last)
	}
}

// TestTurnStream verifies the streaming turn: the emit callback receives the
// deltas, the reply is aggregated + persisted, and the final chunk carries the
// usage (the API-reported accounting the post-compact prefers).
func TestTurnStream(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fp := &fakeProvider{usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	a.providers["m1"] = fp
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	var mu sync.Mutex
	var chunks []provider.Chunk
	conv, reply, err := a.TurnStream(context.Background(), "u1", convID, "hi", nil, func(c provider.Chunk) error {
		mu.Lock()
		chunks = append(chunks, c)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("TurnStream: %v", err)
	}
	if reply != "ok" {
		t.Fatalf("reply = %q, want %q", reply, "ok")
	}
	if conv != convID {
		t.Fatalf("conv = %q, want %q", conv, convID)
	}
	// The emit callback received the deltas + a final chunk with usage.
	mu.Lock()
	defer mu.Unlock()
	if len(chunks) == 0 {
		t.Fatal("no chunks emitted (the stream did not emit deltas)")
	}
	var emitted string
	var gotUsage bool
	for _, c := range chunks {
		emitted += c.TextDelta
		if c.Usage != nil {
			gotUsage = true
		}
	}
	if emitted != "ok" {
		t.Fatalf("emitted text = %q, want %q", emitted, "ok")
	}
	if !gotUsage {
		t.Fatal("no chunk carried the usage (the final chunk should carry it)")
	}
	// The reply is persisted in the store.
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var assistant int
	for _, m := range msgs {
		if m.Role == roleAssistant && m.Content == "ok" {
			assistant++
		}
	}
	if assistant != 1 {
		t.Fatalf("assistant reply not persisted (%d)", assistant)
	}
}

// TestTurnStreamToolIndicators verifies the tool-call indicator events on
// the stream: for each call the model issues, emit gets Chunk{ToolName}
// (issued) and, once it finishes, Chunk{ToolName, ToolDone, ToolSuccess} —
// one pair per call, in issue order (the UI renders one indicator row per
// call; the model's next reply lands below the rows). A tool error is
// ToolSuccess=false, never a turn failure.
func TestTurnStreamToolIndicators(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fp := &fakeProvider{
		replies:   []string{"narration", "final"},
		toolCalls: []provider.ToolCall{{Name: "alpha", Arguments: `{"x":1}`}, {Name: "beta", Arguments: `{}`}},
	}
	a.providers["m1"] = fp
	// alpha succeeds; beta errors (the failure path).
	if err := a.tools.Register(Tool{Name: "alpha", Execute: func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
		return "alpha result", nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := a.tools.Register(Tool{Name: "beta", Execute: func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
		return "", errors.New("beta exploded")
	}}); err != nil {
		t.Fatal(err)
	}
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	var mu sync.Mutex
	var chunks []provider.Chunk
	_, reply, err := a.TurnStream(context.Background(), "u1", convID, "go", nil, func(c provider.Chunk) error {
		mu.Lock()
		chunks = append(chunks, c)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("TurnStream: %v", err)
	}
	if reply != "final" {
		t.Fatalf("reply = %q, want %q (the tool-call turn's narration is not the reply)", reply, "final")
	}
	mu.Lock()
	// The exact sequence: the narration deltas, then per call an issued →
	// done pair (in issue order, the done carrying the outcome), then the
	// final reply's deltas.
	var seq []string
	for _, c := range chunks {
		switch {
		case c.ToolName != "" && !c.ToolDone:
			seq = append(seq, "issued:"+c.ToolName)
		case c.ToolName != "":
			seq = append(seq, "done:"+c.ToolName+":"+boolStr(c.ToolSuccess))
		case c.TextDelta != "":
			seq = append(seq, "text:"+c.TextDelta)
		}
	}
	want := []string{
		"text:narr", "text:ation",
		"issued:alpha", "done:alpha:true",
		"issued:beta", "done:beta:false",
		"text:fi", "text:nal",
	}
	if len(seq) != len(want) {
		t.Fatalf("seq = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("seq[%d] = %q, want %q (seq = %v)", i, seq[i], want[i], seq)
		}
	}
	mu.Unlock()
	// The P7 rows record both calls with their outcomes (the wire pairs
	// them for the model's next call).
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var toolRow int64
	for _, m := range msgs {
		if m.Role == roleAssistant && m.Content == "narration" {
			toolRow = m.ID
		}
	}
	if toolRow == 0 {
		t.Fatal("the narration row (carrying the tool calls) was not persisted")
	}
	rows, err := us.ToolCallsForMessage(toolRow)
	if err != nil {
		t.Fatalf("ToolCallsForMessage: %v", err)
	}
	if len(rows) != 2 || rows[0].Tool != "alpha" || rows[1].Tool != "beta" || rows[0].Success != true || rows[1].Success != false {
		t.Fatalf("P7 rows = %+v, want alpha(success) + beta(failure)", rows)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestTurnCompactsMidLoopOnReportedUsage is the regression for the
// context-overflow-during-a-tool-loop bug: the in-loop pre-compact trigger
// must use the previous model call's REAL API-reported usage (lastUsage), not
// just the char/4 estimate. The fake reports TotalTokens=900 (over the 800
// budget) while the actual messages are tiny (the estimate is far under
// budget), and the turn issues a tool call — so the only thing that can drive
// the compaction is the reported usage from the tool-call call, and it must
// fire MID-loop (before the final answer is requested), not just post-compact.
func TestTurnCompactsMidLoopOnReportedUsage(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	if cfg.Models[0].MaxContext != 1000 {
		t.Fatalf("test precondition: m1 MaxContext = %d, want 1000", cfg.Models[0].MaxContext)
	}
	// Seed > KeepLastMessages turns so the mid-loop compaction has something to
	// fold (compactIfNeeded is a no-op with only the last KeepLastMessages).
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	model := conv.ActiveModelID
	for i := 0; i < 6; i++ {
		if _, err := us.AppendMessage(convID, roleUser, "seeded question", "", "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := us.AppendMessage(convID, roleAssistant, "seeded answer", "", "", &model); err != nil {
			t.Fatal(err)
		}
	}
	// The tool-call call reports 900 total tokens (over the 800 budget) while
	// the messages are a few chars (the estimate is negligible) — so the
	// compaction fires ONLY because of the reported usage. summarizeReply keeps
	// the summarization meta-call from consuming the chat reply cycle.
	fp := &fakeProvider{
		usage:          &provider.Usage{PromptTokens: 500, CompletionTokens: 400, TotalTokens: 900},
		toolCalls:      []provider.ToolCall{{Name: "t", Arguments: `{}`}},
		summarizeReply: "mid-loop summary",
		replies:        []string{"narration", "final"},
	}
	a.providers["m1"] = fp
	if err := a.tools.Register(Tool{Name: "t", Execute: func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
		return "t result", nil
	}}); err != nil {
		t.Fatal(err)
	}
	_, reply, err := a.Turn(context.Background(), "u1", convID, "go", nil)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if reply != "final" {
		t.Fatalf("reply = %q, want %q", reply, "final")
	}
	// A summary was produced.
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var summaries int
	for _, m := range msgs {
		if m.Role == roleSummary {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("summaries = %d, want exactly 1 (the mid-loop compaction)", summaries)
	}
	// The discriminator: the FINAL chat call (the one that produced the reply)
	// already carried the summary — a second system part. If only the
	// post-compact had fired, the summary would have been created AFTER this
	// call and it would have carried a single system part.
	calls := fp.allCalls()
	var lastChat *provider.ChatRequest
	for i := range calls {
		if !isSummarizeRequest(calls[i]) {
			lastChat = &calls[i]
		}
	}
	if lastChat == nil {
		t.Fatal("no chat calls recorded")
	}
	if len(lastChat.System) < 2 {
		t.Fatalf("final chat call System = %d parts, want >= 2 (frozen + summary present BEFORE the final answer — the compaction fired mid-loop)", len(lastChat.System))
	}
}

// TestTurnRecoverFromContextOverflow verifies the overflow backstop: when a
// model call fails with a context-overflow error (a single large tool result
// can push the real prompt past the model's context in one step, ahead of both
// the estimate and the recorded usage), the harness force-compacts the older
// turns and retries the call once, instead of surfacing the error and killing
// the turn. The turn must complete, a summary must exist, and the model must
// NOT be auto-disabled (an overflow is not an unavailable model).
func TestTurnRecoverFromContextOverflow(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	if cfg.Models[0].MaxContext != 1000 {
		t.Fatalf("test precondition: m1 MaxContext = %d, want 1000", cfg.Models[0].MaxContext)
	}
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	model := conv.ActiveModelID
	// Seed > KeepLastMessages turns so the forced compaction has something to fold.
	for i := 0; i < 6; i++ {
		if _, err := us.AppendMessage(convID, roleUser, "seeded question", "", "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := us.AppendMessage(convID, roleAssistant, "seeded answer", "", "", &model); err != nil {
			t.Fatal(err)
		}
	}
	// The FIRST call overflows (the real llama.cpp EXCEED_CONTEXT_SIZE wording);
	// the forced compaction's summarization returns summarizeReply; the retry
	// (a fresh chat call) returns "recovered".
	fp := &fakeProvider{
		firstCallErr:   errors.New(`llama: 400: {"error":{"message":"input (256333 tokens) is larger than the max context size (250112 tokens). skipping","type":"invalid_request_error"}}`),
		summarizeReply: "overflow-summary",
		replies:        []string{"recovered"},
	}
	a.providers["m1"] = fp
	_, reply, err := a.Turn(context.Background(), "u1", convID, "go", nil)
	if err != nil {
		t.Fatalf("Turn: %v (the overflow backstop should have recovered, not surfaced the error)", err)
	}
	if reply != "recovered" {
		t.Fatalf("reply = %q, want %q (the retried call's answer)", reply, "recovered")
	}
	// A summary was produced by the forced compaction.
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var summaries int
	for _, m := range msgs {
		if m.Role == roleSummary {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("summaries = %d, want exactly 1 (the forced compaction)", summaries)
	}
	// The overflow is NOT an unavailable model: m1 must not be auto-disabled.
	if a.isAutoDisabled(conv.ActiveModelID) {
		t.Fatal("the model was auto-disabled — a context overflow is not an unavailable model")
	}
	// The retry happened: 3 provider calls (the failed call, the forced
	// compaction's summarization, the successful retry).
	if got := fp.callCount(); got != 3 {
		t.Fatalf("provider calls = %d, want 3 (failed call + forced-compaction summary + retry)", got)
	}
}
