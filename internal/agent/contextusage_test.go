package agent

import (
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/store"
)

func strPtr(s string) *string { return &s }

// TestContextUsageCountsToolTranscript verifies the context estimate (the
// context bar's token count, from Agent.ContextUsage) includes the model's
// TOOL transcript — the tool-call ARGUMENTS and the tool RESULTS. Before the
// fix, ContextUsage built the request with a nil tool-call map, so a
// tool-using conversation was undercounted by its entire tool transcript
// (often the LARGEST part of the context) — the context bar looked "wildly
// off" once tool loops existed. The estimate must match what the live turn
// actually sends to the model (runTurn passes the real tool-call rows).
func TestContextUsageCountsToolTranscript(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	// Conversation WITH a tool call: the user asks, the assistant calls a
	// tool (a sizable argument), the tool returns a large result, then the
	// assistant replies.
	withTools, err := us.CreateConversation("with tools", "m1")
	if err != nil {
		t.Fatal(err)
	}
	bigArgs := `{"query": "` + strings.Repeat("search term ", 40) + `"}` // ~500 chars
	bigResult := strings.Repeat("tool output line. ", 200)               // ~3.6KB
	if _, err := us.AppendMessage(withTools.ID, roleUser, "look things up", "", "", nil); err != nil {
		t.Fatal(err)
	}
	asst, err := us.AppendMessage(withTools.ID, roleAssistant, "let me check", "", "", strPtr("m1"))
	if err != nil {
		t.Fatal(err)
	}
	tcid, err := us.AppendToolCall(asst.ID, "web_search", bigArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := us.CompleteToolCall(tcid, bigResult, true); err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(withTools.ID, roleAssistant, "here is what I found", "", "", strPtr("m1")); err != nil {
		t.Fatal(err)
	}
	// Conversation WITHOUT a tool call: the SAME message text (so the only
	// difference is the tool transcript).
	plain, err := us.CreateConversation("plain", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(plain.ID, roleUser, "look things up", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(plain.ID, roleAssistant, "let me check", "", "", strPtr("m1")); err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(plain.ID, roleAssistant, "here is what I found", "", "", strPtr("m1")); err != nil {
		t.Fatal(err)
	}

	wt, err := a.ContextUsage("u1", withTools.ID)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := a.ContextUsage("u1", plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	tokWith := wt["tokens"].(int)
	tokPlain := pl["tokens"].(int)
	t.Logf("with-tools=%d plain=%d delta=%d", tokWith, tokPlain, tokWith-tokPlain)
	// The tool transcript (args + result) must be counted: the with-tools
	// estimate is well above the plain one (same message text).
	if tokWith <= tokPlain {
		t.Fatalf("ContextUsage with tools = %d, plain = %d — the tool transcript (args + result) must be counted", tokWith, tokPlain)
	}
	// The large tool result (~3.6KB ≈ 900 tokens at ~4 chars/token) + the
	// args must show up: the delta is large, not just the args alone.
	if tokWith-tokPlain < 500 {
		t.Fatalf("delta = %d tokens, want a large delta (the tool result alone is ~900 tokens)", tokWith-tokPlain)
	}
}

// TestBuildRequestCountsToolCallArguments verifies the request builder's token
// estimate counts the tool-call ARGUMENTS (they ride on the assistant
// message's tool_calls and are part of the prompt the model sends) — not just
// the message text. A conversation with a tool call estimates higher than the
// identical conversation without one, even with the same message text.
func TestBuildRequestCountsToolCallArguments(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	bigArgs := strings.Repeat(`"padding argument content "`, 30) // ~750 chars
	// Same message text in both; the only difference is the tool call.
	msgs := []store.Message{
		{Role: roleUser, Content: "look things up", Seq: 0},
		{Role: roleAssistant, Content: "let me check", Seq: 1},
		{Role: roleAssistant, Content: "here is what I found", Seq: 2},
	}
	// Without tool calls (nil map): the baseline.
	_, base := buildRequest(conv, msgs, profile.Default(), nil)
	// With a tool call on the first assistant message (a large argument + a
	// result). Build the tcMap the way toolCallsForMessages would.
	tcMap := map[int64][]store.ToolCall{}
	// The assistant message at Seq 1 has ID... but buildRequest's msgs here
	// are synthetic (no IDs). Use ID 0 for the "let me check" message by
	// giving it an ID and keying the map on it.
	msgs[1].ID = 7
	tcMap[7] = []store.ToolCall{{ID: 1, MessageID: 7, Tool: "web_search", Args: bigArgs, Result: strings.Repeat("result. ", 100), Success: true}}
	_, withTC := buildRequest(conv, msgs, profile.Default(), tcMap)
	t.Logf("base=%d withTC=%d delta=%d", base, withTC, withTC-base)
	if withTC <= base {
		t.Fatalf("buildRequest with tool call = %d, baseline = %d — the tool args + result must raise the estimate", withTC, base)
	}
}
