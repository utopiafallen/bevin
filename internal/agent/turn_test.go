package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// fakeProvider records every Chat request + returns a fixed reply (or cycles
// through canned replies). It implements the provider.Provider interface so the
// Agent can be exercised without a real model backend.
type fakeProvider struct {
	mu       sync.Mutex
	calls    []provider.ChatRequest
	replies  []string
	replyIdx int
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Chat(_ context.Context, _ string, req provider.ChatRequest) (*provider.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	reply := "ok"
	if len(f.replies) > 0 {
		reply = f.replies[f.replyIdx%len(f.replies)]
		f.replyIdx++
	}
	return &provider.Result{Text: reply}, nil
}

func (f *fakeProvider) Stream(_ context.Context, _ string, _ provider.ChatRequest, _ func(provider.Chunk) error) (*provider.Result, error) {
	return &provider.Result{Text: "ok"}, nil
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
