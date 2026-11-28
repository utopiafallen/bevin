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
	_, base := buildRequest(conv, msgs, profile.Default(), nil, "")
	// With a tool call on the first assistant message (a large argument + a
	// result). Build the tcMap the way toolCallsForMessages would.
	tcMap := map[int64][]store.ToolCall{}
	// The assistant message at Seq 1 has ID... but buildRequest's msgs here
	// are synthetic (no IDs). Use ID 0 for the "let me check" message by
	// giving it an ID and keying the map on it.
	msgs[1].ID = 7
	tcMap[7] = []store.ToolCall{{ID: 1, MessageID: 7, Tool: "web_search", Args: bigArgs, Result: strings.Repeat("result. ", 100), Success: true}}
	_, withTC := buildRequest(conv, msgs, profile.Default(), tcMap, "")
	t.Logf("base=%d withTC=%d delta=%d", base, withTC, withTC-base)
	if withTC <= base {
		t.Fatalf("buildRequest with tool call = %d, baseline = %d — the tool args + result must raise the estimate", withTC, base)
	}
}

// TestBuildRequestSkillBlockStable pins the PREFIX-CACHE invariant the
// SKILL SYSTEM re-injection must obey: the loaded-skill block floats at
// the HEAD of the message list as one ephemeral user message, injected
// right after the compaction message (the summary's framing rides the
// system part) — never the system prompt itself. Because the block is
// anchored to the top of the window, history GROWS BELOW it: with the
// same active set, every request is an exact prefix extension of the
// previous one and the KV cache survives all turns. Only a load/unload
// (the block appears/disappears) or a compaction (the summary changes)
// re-encodes.
func TestBuildRequestSkillBlockStable(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	block := "Loaded skills (this conversation):\n\n### Skill: deploy-cli (global)\ndeploys the app\n\nrun the script\n"
	// Turn 1: the skill is already loaded; the block floats above the
	// first user message.
	msgs1 := []store.Message{
		{Role: roleUser, Content: "hello", Seq: 0},
	}
	// Turn 2 (same active set): the transcript grew below the block.
	msgs2 := append(append([]store.Message{}, msgs1...),
		store.Message{Role: roleAssistant, Content: "hi there", Seq: 1},
		store.Message{Role: roleUser, Content: "deploy it", Seq: 2},
	)
	req1, _ := buildRequest(conv, msgs1, profile.Default(), nil, block)
	req2, _ := buildRequest(conv, msgs2, profile.Default(), nil, block)
	// The system prompt is untouched by the block.
	if len(req1.System) == 0 || req1.System[0].Text != req2.System[0].Text {
		t.Fatalf("the system prompt changed between turns: %q vs %q", req1.System[0].Text, req2.System[0].Text)
	}
	// The block floats at the head of the message list (after the
	// system part, where the compaction's summary framing sits).
	if string(req1.Messages[0].Role) != "user" || req1.Messages[0].Text != block {
		t.Fatalf("the block must be the FIRST message, got role=%q text=%q", req1.Messages[0].Role, req1.Messages[0].Text)
	}
	// PREFIX EXTENSION: the whole previous request (block + history) is
	// byte-identical at the head of the next one — the KV prefix cache
	// survives the turn.
	if len(req2.Messages) <= len(req1.Messages) {
		t.Fatalf("messages: turn1=%d turn2=%d, the transcript must grow", len(req1.Messages), len(req2.Messages))
	}
	for i, m := range req1.Messages {
		if req2.Messages[i].Role != m.Role || req2.Messages[i].Text != m.Text {
			t.Fatalf("message %d diverged between turns: old %q/%q vs new %q/%q — the prefix cache breaks", i, m.Role, m.Text, req2.Messages[i].Role, req2.Messages[i].Text)
		}
	}
	// And the transcript is exactly the block + the two turns' rows.
	if len(req2.Messages) != len(msgs2)+1 {
		t.Fatalf("turn2 messages = %d, want %d (block + history)", len(req2.Messages), len(msgs2)+1)
	}
	// Unload: the block disappears, the (blockless) request is clean.
	reqNone, _ := buildRequest(conv, msgs2, profile.Default(), nil, "")
	if len(reqNone.Messages) != len(msgs2) {
		t.Fatalf("a blockless request must carry no extra message: %d vs %d", len(reqNone.Messages), len(msgs2))
	}
	// A load mid-conversation re-encodes exactly once (the block appears
	// at the head) — and from then on the requests extend again.
	reqLoaded, _ := buildRequest(conv, msgs2, profile.Default(), nil, block)
	if string(reqLoaded.Messages[0].Role) != "user" || reqLoaded.Messages[0].Text != block {
		t.Fatal("the block must sit at the head after a mid-conversation load")
	}
	// The block's tokens are counted (the compaction trigger sees the
	// same prompt the model does).
	{
		_, withTok := buildRequest(conv, msgs2, profile.Default(), nil, block)
		_, withoutTok := buildRequest(conv, msgs2, profile.Default(), nil, "")
		if withTok <= withoutTok {
			t.Fatalf("the block's tokens must raise the estimate: with=%d without=%d", withTok, withoutTok)
		}
	}
}
