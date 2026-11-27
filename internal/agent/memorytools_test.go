package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/store"
)

// The memory tools (remember / forget) + the search tool's memory leg
// (rankMemories — recall was folded into search, 2026-11-27).

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

func TestMemoryRememberSearchForget(t *testing.T) {
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

	// The search tool's memory leg (lexical mode — no embedder in the test
	// agent): the keyword match surfaces the memory, and the result line
	// carries the #id that forget / supersede_id need.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "Go web server project"})
	if !strings.Contains(out, "#1 [work] The user works on a Go web server project") {
		t.Fatalf("search missed the keyword match: %q", out)
	}
	// A query with no hit at all → the no-match result.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "zzzqqqxyz"})
	if !strings.Contains(out, "no matches") {
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
	// Forgotten memories don't surface in search.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "Go web server project"})
	if strings.Contains(out, "#1 [work]") {
		t.Fatalf("a forgotten memory must not be surfaced: %q", out)
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

// TestMemorySearchRanking checks the memory leg's blend in lexical mode
// (the test agent has no embedder): among the FTS hits, the higher
// confidence + recency memory ranks above the weaker one, and the limit
// caps the list. (The semantic mode's cosine component is the same blend
// recall used to run — the weights are the memW* constants.)
func TestMemorySearchRanking(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	_, us := newTestConversation(t, a, "u1")
	if _, err := us.CreateMemory("a", "The cat sleeps on the couch", nil, 0.5, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := us.CreateMemory("b", "The cat hates the couch", nil, 0.95, "test", nil); err != nil {
		t.Fatal(err)
	}
	out, err := a.SearchAll(context.Background(), "u1", "cat couch", "", "", 10)
	if err != nil {
		t.Fatalf("SearchAll: %v", err)
	}
	got := memIDs(out["memories"])
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("memories = %v, want [2 1] (the higher-confidence memory first)", got)
	}
	// The limit caps the list (one source, one line).
	out, err = a.SearchAll(context.Background(), "u1", "cat couch", "", "", 1)
	if err != nil {
		t.Fatalf("SearchAll limit: %v", err)
	}
	if got := memIDs(out["memories"]); len(got) != 1 || got[0] != 2 {
		t.Fatalf("limit=1 = %v, want just the top hit [2]", got)
	}
}
