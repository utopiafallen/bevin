package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
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
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Chat(_ context.Context, model string, req provider.ChatRequest) (*provider.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	f.models = append(f.models, model)
	reply := "ok"
	if len(f.replies) > 0 {
		reply = f.replies[f.replyIdx%len(f.replies)]
		f.replyIdx++
	}
	res := &provider.Result{Text: reply}
	if f.usage != nil {
		res.Usage = f.usage
	}
	return res, nil
}

func (f *fakeProvider) Stream(_ context.Context, model string, req provider.ChatRequest, emit func(provider.Chunk) error) (*provider.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.models = append(f.models, model)
	reply := "ok"
	if len(f.replies) > 0 {
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
	// compaction fires once the conversation has > keepLastTurns turns —
	// exercising the summarization provider call too.
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
	// Four turns: enough for > keepLastTurns messages so a summarization
	// call happens (fires from the 3rd turn's post-compact onward).
	for i := 0; i < 4; i++ {
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi"); err != nil {
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
	req, tokens := buildRequest(conv, msgs)
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
	req, _ := buildRequest(conv, msgs)
	if len(req.System) != 1 {
		t.Fatalf("System = %d parts, want 1 (no summary yet)", len(req.System))
	}
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2 (all turns)", len(req.Messages))
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
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hello "+strings.Repeat("x", 100)); err != nil {
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
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi"); err != nil {
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
		if _, _, err := a.Turn(context.Background(), "u1", convID, "hi "+strings.Repeat("y", 200)); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
}

// TestBuildRequestWithTimestamp verifies the timestamp is prepended to each
// message (so the model knows when it was sent) when the CreatedAt is set.
func TestBuildRequestWithTimestamp(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	msgs := []store.Message{
		{Role: roleUser, Content: "hello", Seq: 0, CreatedAt: "2026-11-12T10:30:00.123456789Z"},
		{Role: roleAssistant, Content: "hi", Seq: 1, CreatedAt: "2026-11-12T10:30:05Z"},
	}
	req, _ := buildRequest(conv, msgs)
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	// The timestamp is prepended (RFC3339, UTC, without the nanoseconds).
	if req.Messages[0].Text != "[2026-11-12T10:30:00Z] hello" {
		t.Fatalf("Messages[0] = %q, want the timestamp prepended", req.Messages[0].Text)
	}
	if req.Messages[1].Text != "[2026-11-12T10:30:05Z] hi" {
		t.Fatalf("Messages[1] = %q, want the timestamp prepended", req.Messages[1].Text)
	}
}

// TestBuildRequestWithReasoning verifies the request builder replays an
// assistant's stored reasoning in the neutral message's Reasoning field (the
// provider encodes it in the API's native field — OAI wire:
// reasoning_content), while the message TEXT stays the plain timestamp +
// reply (default on — reasoningInTranscript), and user messages are
// unaffected.
func TestBuildRequestWithReasoning(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	msgs := []store.Message{
		{Role: roleUser, Content: "solve it", Seq: 0, CreatedAt: "2026-11-12T10:30:00Z"},
		{Role: roleAssistant, Content: "the answer is 42", Reasoning: "step one: read the problem\nstep two: compute", Seq: 1, CreatedAt: "2026-11-12T10:30:05Z"},
	}
	req, _ := buildRequest(conv, msgs)
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Text != "[2026-11-12T10:30:00Z] solve it" {
		t.Fatalf("Messages[0] = %q, want the user turn untouched", req.Messages[0].Text)
	}
	if req.Messages[0].Reasoning != "" {
		t.Fatalf("Messages[0].Reasoning = %q, want empty (user turn)", req.Messages[0].Reasoning)
	}
	// The reply text is plain (timestamp + content) — the reasoning is NOT
	// framed into the text.
	if req.Messages[1].Text != "[2026-11-12T10:30:05Z] the answer is 42" {
		t.Fatalf("Messages[1] = %q, want the timestamp + reply only (no reasoning in the text)", req.Messages[1].Text)
	}
	if req.Messages[1].Reasoning != "step one: read the problem\nstep two: compute" {
		t.Fatalf("Messages[1].Reasoning = %q, want the stored trace (the API's native field)", req.Messages[1].Reasoning)
	}
}

// TestAssistantFacingNoReasoningWhenDisabled verifies the override path: with
// includeReasoning=false (a future ModelProfile policy) the Reasoning field is
// left empty even when the message carries a stored trace, and a no-trace
// message is identical either way.
func TestAssistantFacingNoReasoningWhenDisabled(t *testing.T) {
	m := store.Message{Role: roleAssistant, Content: "hi", Reasoning: "internal thought", CreatedAt: "2026-11-12T10:30:05Z"}
	got := assistantFacing(m, false)
	if got.Reasoning != "" {
		t.Fatalf("Reasoning = %q, want empty (override disabled)", got.Reasoning)
	}
	if got.Text != "[2026-11-12T10:30:05Z] hi" {
		t.Fatalf("Text = %q, want the plain timestamped reply", got.Text)
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
// the API's native field), the message text stays the plain timestamped
// reply (no [thinking] framing), so the model keeps continuity with its own
// earlier thinking in the channel it produced it in.
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
	if _, _, err := a.Turn(context.Background(), "u1", convID, "the next question"); err != nil {
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

// TestCompactPreservesLastTurns verifies a compaction preserves the last
// keepLastTurns messages verbatim (only the older turns are folded into the
// summary, so the chat resumes exactly where it left off).
func TestCompactPreservesLastTurns(t *testing.T) {
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
	if err := a.compactIfNeeded(context.Background(), us, conv, fp, 0); err != nil {
		t.Fatalf("compactIfNeeded: %v", err)
	}
	// The request builder now sends the summary + the last keepLastTurns
	// messages verbatim — not the full history (the older turns are folded into
	// the summary). The store retains the full history (the summary only
	// changes what the request builder sends).
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
	req, _ := buildRequest(conv, msgs)
	if len(req.System) < 2 {
		t.Fatalf("request System = %d parts, want >= 2 (frozen prompt + summary)", len(req.System))
	}
	if len(req.Messages) != keepLastTurns {
		t.Fatalf("request sent %d messages, want %d (the last keepLastTurns verbatim)", len(req.Messages), keepLastTurns)
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
	conv, reply, err := a.TurnStream(context.Background(), "u1", convID, "hi", func(c provider.Chunk) error {
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
