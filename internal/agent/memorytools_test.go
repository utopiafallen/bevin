package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/store"
)

// The memory tools (recall / remember / forget).

func newTestConversation(t *testing.T, a *Agent, handle string) (string, *store.UserStore) {
	t.Helper()
	us, err := a.userStore(handle)
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	conv, err := us.CreateConversation("Test chat", "m1")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv.ID, us
}

func execMemoryTool(t *testing.T, a *Agent, tool, handle, convID string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tl, ok := a.tools.Get(tool)
	if !ok {
		t.Fatalf("tool %q not registered", tool)
	}
	out, err := tl.Execute(withConversation(context.Background(), convID), handle, raw)
	if err != nil {
		t.Fatalf("%s returned an error (must be a result string): %v", tool, err)
	}
	return out
}

func withConversation(ctx context.Context, convID string) context.Context {
	if convID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxConversationKey, convID)
}

func TestMemoryRememberRecallForget(t *testing.T) {
	a, _, _ := newTestAgent(t)
	_, us := newTestConversation(t, a, "u1")
	const conv = "c1"
	_ = conv

	// remember.
	out := execMemoryTool(t, a, "remember", "u1", conv, map[string]any{
		"facet": "work", "statement": "The user works on a Go web server project",
	})
	if !strings.Contains(out, "remembered as #1") {
		t.Fatalf("want remembered, got %q", out)
	}

	// recall (lexical mode — no embedder in the test agent): the keyword
	// match ranks the memory first.
	out = execMemoryTool(t, a, "recall", "u1", conv, map[string]any{"query": "Go web server project"})
	if !strings.Contains(out, "#1 [work] The user works on a Go web server project") {
		t.Fatalf("recall missed the keyword match: %q", out)
	}
	// A query with no lexical hit at all → the no-match result.
	out = execMemoryTool(t, a, "recall", "u1", conv, map[string]any{"query": "zzzqqqxyz"})
	if !strings.Contains(out, "no stored memories match") {
		t.Fatalf("want the no-match result, got %q", out)
	}

	// remember validation.
	out = execMemoryTool(t, a, "remember", "u1", conv, map[string]any{"statement": "no facet"})
	if !strings.Contains(out, "needs both") {
		t.Fatalf("want the facet requirement, got %q", out)
	}
	out = execMemoryTool(t, a, "remember", "u1", conv, map[string]any{
		"facet": "x", "statement": strings.Repeat("y", 1001),
	})
	if !strings.Contains(out, "too long") {
		t.Fatalf("want the length refusal, got %q", out)
	}

	// forget.
	out = execMemoryTool(t, a, "forget", "u1", conv, map[string]any{"id": 1})
	if !strings.Contains(out, "forgotten #1") {
		t.Fatalf("want forgotten, got %q", out)
	}
	mem, err := us.GetMemory(1)
	if err != nil || mem.Status != "superseded" {
		t.Fatalf("forget must mark superseded (row retained): %+v, %v", mem, err)
	}
	if mem.SupersededBy != nil {
		t.Fatalf("a forget carries no replacement, got %v", *mem.SupersededBy)
	}
	// Forgotten memories are not recalled.
	out = execMemoryTool(t, a, "recall", "u1", conv, map[string]any{"query": "Go web server project"})
	if strings.Contains(out, "#1 [work]") {
		t.Fatalf("a forgotten memory must not be recalled: %q", out)
	}
	// Forgetting again: not active.
	out = execMemoryTool(t, a, "forget", "u1", conv, map[string]any{"id": 1})
	if !strings.Contains(out, "not active") {
		t.Fatalf("want not-active, got %q", out)
	}
	// Forgetting an unknown id.
	out = execMemoryTool(t, a, "forget", "u1", conv, map[string]any{"id": 999})
	if !strings.Contains(out, "no memory with id 999") {
		t.Fatalf("want unknown-id result, got %q", out)
	}
}

func TestMemoryRememberSupersede(t *testing.T) {
	a, _, _ := newTestAgent(t)
	_, us := newTestConversation(t, a, "u1")
	const conv = "c1"

	out := execMemoryTool(t, a, "remember", "u1", conv, map[string]any{
		"facet": "home", "statement": "The user lives in Portland",
	})
	if !strings.Contains(out, "remembered as #1") {
		t.Fatalf("want #1, got %q", out)
	}
	out = execMemoryTool(t, a, "remember", "u1", conv, map[string]any{
		"facet": "home", "statement": "The user moved to Seattle", "supersede_id": 1,
	})
	if !strings.Contains(out, "superseded #1") {
		t.Fatalf("want superseded, got %q", out)
	}
	old, err := us.GetMemory(1)
	if err != nil || old.Status != "superseded" || old.SupersededBy == nil || *old.SupersededBy != 2 {
		t.Fatalf("the old memory must point at the new one: %+v, %v", old, err)
	}
	new, err := us.GetMemory(2)
	if err != nil || new.Status != "active" {
		t.Fatalf("the new memory must be active: %+v, %v", new, err)
	}
	// Superseding a non-existent id: the new fact is kept, nothing replaced.
	out = execMemoryTool(t, a, "remember", "u1", conv, map[string]any{
		"facet": "home", "statement": "The user rents now", "supersede_id": 42,
	})
	if !strings.Contains(out, "does not name an existing memory") || !strings.Contains(out, "remembered as #3") {
		t.Fatalf("want the bad-supersede result, got %q", out)
	}
	// Superseding an already-superseded id: refused cleanly.
	out = execMemoryTool(t, a, "remember", "u1", conv, map[string]any{
		"facet": "home", "statement": "The user bought a house", "supersede_id": 1,
	})
	if !strings.Contains(out, "not active") {
		t.Fatalf("want the not-active refusal, got %q", out)
	}
}

// TestMemoryRecallRanking checks the blend: with an embedder wired (fake,
// via the vector index seam is not available here — so this exercises the
// lexical-mode ordering), a keyword hit with higher confidence + recency
// ranks above a weaker hit.
func TestMemoryRecallRanking(t *testing.T) {
	a, _, _ := newTestAgent(t)
	_, us := newTestConversation(t, a, "u1")
	const conv = "c1"
	if _, err := us.CreateMemory("a", "The cat sleeps on the couch", nil, 0.5, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := us.CreateMemory("b", "The cat hates the couch", nil, 0.95, "test", nil); err != nil {
		t.Fatal(err)
	}
	out := execMemoryTool(t, a, "recall", "u1", conv, map[string]any{"query": "cat couch"})
	i1 := strings.Index(out, "#1 [a]")
	i2 := strings.Index(out, "#2 [b]")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("both memories should match: %q", out)
	}
	if !(i2 < i1) {
		t.Fatalf("the higher-confidence memory must rank first:\n%s", out)
	}
	// top caps the list.
	out = execMemoryTool(t, a, "recall", "u1", conv, map[string]any{"query": "cat couch", "top": 1})
	if strings.Count(out, "#") != 1 {
		t.Fatalf("top=1 must return one memory:\n%s", out)
	}
}
