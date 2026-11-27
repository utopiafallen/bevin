package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/store"
)

// The search tool + SearchAll's scopes (conversation / workspace). The
// test agent has no embedder configured, so a.vec is nil: the semantic
// conversation leg is deterministically empty, and the local sources
// (memories, profile, workspace files) exercise the scope logic.

func seedSearchFixtures(t *testing.T, a *Agent, handle string) (convA, convB string, wsID string, mA, mB, mNone int64) {
	t.Helper()
	convA, us := newTestConversation(t, a, handle)
	convBRow, err := us.CreateConversation("B", "m1")
	if err != nil {
		t.Fatalf("CreateConversation B: %v", err)
	}
	convB = convBRow.ID
	msgA, err := us.AppendMessage(convA, "user", "the zephyr plan", "", "", nil)
	if err != nil {
		t.Fatalf("AppendMessage A: %v", err)
	}
	msgB, err := us.AppendMessage(convB, "user", "zephyr again", "", "", nil)
	if err != nil {
		t.Fatalf("AppendMessage B: %v", err)
	}
	memA, err := us.CreateMemory("work", "the zephyr project kickoff", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory A: %v", err)
	}
	if err := us.SetMemoryProvenance(memA.ID, []int64{msgA.ID}); err != nil {
		t.Fatalf("SetMemoryProvenance A: %v", err)
	}
	memB, err := us.CreateMemory("work", "a zephyr follow-up", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory B: %v", err)
	}
	if err := us.SetMemoryProvenance(memB.ID, []int64{msgB.ID}); err != nil {
		t.Fatalf("SetMemoryProvenance B: %v", err)
	}
	memNone, err := us.CreateMemory("work", "another zephyr fact", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory none: %v", err)
	}
	ws, err := us.CreateWorkspace("Fixtures", "test ws", "/tmp/fixtures")
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	wsID = ws.ID
	for _, name := range []string{"zephyr-notes.md", "other.txt"} {
		if _, err := us.AddWorkspaceFile(wsID, name, name, "text/plain", 10, "", "user", "doc"); err != nil {
			t.Fatalf("AddWorkspaceFile %s: %v", name, err)
		}
	}
	return convA, convB, wsID, memA.ID, memB.ID, memNone.ID
}

func memIDs(v any) []int64 {
	mems := anySlice[store.Memory](v)
	out := make([]int64, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.ID)
	}
	return out
}

func TestSearchAllScopes(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	convA, convB, wsID, mA, mB, mNone := seedSearchFixtures(t, a, "u1")
	ctx := context.Background()

	// No scopes: every local source is searched whole.
	out, err := a.SearchAll(ctx, "u1", "zephyr", "", "", 10)
	if err != nil {
		t.Fatalf("SearchAll unscoped: %v", err)
	}
	got := memIDs(out["memories"])
	if len(got) != 3 {
		t.Fatalf("unscoped memories = %v, want all 3 (incl. the no-provenance %d)", got, mNone)
	}
	if convs := anySlice[SearchHit](out["conversations"]); len(convs) != 0 {
		t.Fatalf("no vector index configured: conversations = %v, want empty", convs)
	}
	scopeConvs, ok := out["conversations_scope"].([]any)
	if !ok || len(scopeConvs) != 2 {
		t.Fatalf("conversations_scope = %v, want the 2 conversations", out["conversations_scope"])
	}
	scopeWS, ok := out["workspaces"].([]any)
	if !ok || len(scopeWS) != 1 {
		t.Fatalf("workspaces = %v, want the 1 workspace", out["workspaces"])
	}

	// Conversation scope: the memory leg narrows to that conversation's
	// provenance (the no-provenance memory drops out).
	out, err = a.SearchAll(ctx, "u1", "zephyr", convA, "", 10)
	if err != nil {
		t.Fatalf("SearchAll conv scope: %v", err)
	}
	if got := memIDs(out["memories"]); len(got) != 1 || got[0] != mA {
		t.Fatalf("conv A memories = %v, want only %d", got, mA)
	}
	out, err = a.SearchAll(ctx, "u1", "zephyr", convB, "", 10)
	if err != nil {
		t.Fatalf("SearchAll conv B scope: %v", err)
	}
	if got := memIDs(out["memories"]); len(got) != 1 || got[0] != mB {
		t.Fatalf("conv B memories = %v, want only %d", got, mB)
	}

	// Workspace scope: the file leg narrows to that workspace.
	out, err = a.SearchAll(ctx, "u1", "zephyr", "", wsID, 10)
	if err != nil {
		t.Fatalf("SearchAll ws scope: %v", err)
	}
	files := anySlice[store.WorkspaceFile](out["workspace"])
	if len(files) != 1 || files[0].Name != "zephyr-notes.md" {
		t.Fatalf("ws files = %v, want only zephyr-notes.md", files)
	}
	// The other scopes are untouched by the workspace scope.
	if got := memIDs(out["memories"]); len(got) != 3 {
		t.Fatalf("workspace scope must not narrow memories: %v", got)
	}

	// Both scopes at once: each narrows its own source independently.
	out, err = a.SearchAll(ctx, "u1", "zephyr", convA, wsID, 10)
	if err != nil {
		t.Fatalf("SearchAll both scopes: %v", err)
	}
	if got := memIDs(out["memories"]); len(got) != 1 || got[0] != mA {
		t.Fatalf("both-scopes memories = %v, want only %d", got, mA)
	}
	if files := anySlice[store.WorkspaceFile](out["workspace"]); len(files) != 1 {
		t.Fatalf("both-scopes files = %v, want 1", files)
	}

	// Bad scope ids are 404s (the handler maps ErrNotFound), not empty
	// results.
	if _, err := a.SearchAll(ctx, "u1", "zephyr", "no-such-conv", "", 10); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown conversation: err = %v, want ErrNotFound", err)
	}
	if _, err := a.SearchAll(ctx, "u1", "zephyr", "", "no-such-ws", 10); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown workspace: err = %v, want ErrNotFound", err)
	}
}

func execSearchTool(t *testing.T, a *Agent, handle string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tl, ok := a.tools.Get("search")
	if !ok {
		t.Fatal("tool \"search\" not registered")
	}
	out, err := tl.Execute(context.Background(), handle, raw)
	if err != nil {
		t.Fatalf("search returned an error (must be a result string): %v", err)
	}
	return out
}

func TestSearchTool(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	convA, convB, wsID, _, _, _ := seedSearchFixtures(t, a, "u1")

	// Unscoped: the memories render, and the result carries the
	// not-configured-vector-index note (the test agent has no embedder).
	out := execSearchTool(t, a, "u1", map[string]any{"q": "zephyr"})
	if !strings.Contains(out, "MEMORIES") || !strings.Contains(out, "zephyr project kickoff") {
		t.Fatalf("unscoped search result missing the memories section:\n%s", out)
	}
	if !strings.Contains(out, "semantic conversation search is not configured") {
		t.Fatalf("unscoped search result missing the no-index note:\n%s", out)
	}
	if strings.Contains(out, "scoped to") {
		t.Fatalf("unscoped search result must not carry a scope note:\n%s", out)
	}

	// Conversation scope: the sections are labeled, the scope note rides
	// the result.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "zephyr", "conversation": convA})
	if !strings.Contains(out, "MEMORIES (this conversation)") {
		t.Fatalf("conversation-scoped result missing the scoped memories label:\n%s", out)
	}
	if !strings.Contains(out, "scoped to conversation "+convA) {
		t.Fatalf("conversation-scoped result missing the scope note:\n%s", out)
	}

	// Workspace scope: the file section names the workspace.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "zephyr", "workspace": wsID})
	if !strings.Contains(out, "WORKSPACE FILES ("+wsID+")") || !strings.Contains(out, "zephyr-notes.md") {
		t.Fatalf("workspace-scoped result missing the files section:\n%s", out)
	}

	// Unknown scope ids are friendly results, not errors.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "zephyr", "conversation": "nope"})
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("unknown conversation result = %q, want the not-exists signal", out)
	}
	out = execSearchTool(t, a, "u1", map[string]any{"q": "zephyr", "workspace": "nope"})
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("unknown workspace result = %q, want the not-exists signal", out)
	}

	// No matches anywhere.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "zzqqxx nothing here"})
	if !strings.Contains(out, "no matches") {
		t.Fatalf("empty search result = %q, want no-matches", out)
	}

	// A query with no q.
	out = execSearchTool(t, a, "u1", map[string]any{"q": "  "})
	if !strings.Contains(out, "needs a query") {
		t.Fatalf("blank query result = %q, want the needs-a-query signal", out)
	}

	// The limit is clamped (the fixture has 3 memories; asking for 1 must
	// not return more than 1 per source).
	out = execSearchTool(t, a, "u1", map[string]any{"q": "zephyr", "limit": 1})
	if n := strings.Count(out, "(confidence"); n != 1 {
		t.Fatalf("limit=1 returned %d memory lines, want 1:\n%s", n, out)
	}
	_ = convB
}
