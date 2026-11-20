package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/utopiafallen/bevin/internal/browser"
	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// scriptedProvider is a fake provider that replays a fixed SCRIPT of
// results (one per Chat call) — the tool-loop tests need a first call that
// carries tool calls and a second that is the final answer. It records
// every request (the wire assertions live here, not in the turn logic).
type scriptedProvider struct {
	mu     sync.Mutex
	calls  []provider.ChatRequest
	script []provider.Result
	idx    int
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Chat(_ context.Context, _ string, req provider.ChatRequest) (*provider.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.idx >= len(s.script) {
		return &provider.Result{Text: "unexpected extra call"}, nil
	}
	r := s.script[s.idx]
	s.idx++
	return &r, nil
}

func (s *scriptedProvider) Stream(_ context.Context, _ string, req provider.ChatRequest, _ func(provider.Chunk) error) (*provider.Result, error) {
	return s.Chat(context.Background(), "", req)
}
func (s *scriptedProvider) Models(_ context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (s *scriptedProvider) Embed(_ context.Context, _ string, _ []string) ([][]float32, error) {
	return nil, nil
}

func (s *scriptedProvider) allCalls() []provider.ChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]provider.ChatRequest{}, s.calls...)
}

// TestToolRegistry exercises the WS1 registry: registration order is the
// wire order (stable prompts), a duplicate name fails loudly (a wiring bug
// — two tools with one name make the model's choice ambiguous), Get is the
// dispatch path, and Names is sorted (the unknown-tool error lists them).
func TestToolRegistry(t *testing.T) {
	r := NewToolRegistry()
	if r.Len() != 0 {
		t.Fatal("a fresh registry must be empty")
	}
	mk := func(name string) Tool {
		return Tool{
			Name: name, Description: "d-" + name,
			Parameters: map[string]any{"type": "object"},
			Execute:    func(context.Context, string, json.RawMessage) (string, error) { return name, nil },
		}
	}
	if err := r.Register(mk("zeta")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(mk("alpha")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(mk("alpha")); err == nil {
		t.Fatal("a duplicate name must fail (a wiring bug)")
	}
	if err := r.Register(Tool{Name: "", Execute: func(context.Context, string, json.RawMessage) (string, error) { return "", nil }}); err == nil {
		t.Fatal("a tool without a name must fail")
	}
	if err := r.Register(Tool{Name: "noexec"}); err == nil {
		t.Fatal("a tool without an executor must fail")
	}
	if got := r.Names(); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("Names = %v, want [alpha zeta] (sorted)", got)
	}
	wire := r.Wire()
	if len(wire) != 2 || wire[0].Name != "zeta" || wire[1].Name != "alpha" {
		t.Fatalf("Wire = %+v, want registration order [zeta alpha]", wire)
	}
	if wire[0].Description != "d-zeta" {
		t.Fatalf("Wire[0].Description = %q", wire[0].Description)
	}
	if _, ok := r.Get("alpha"); !ok {
		t.Fatal("Get(alpha) must resolve")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get(nope) must not resolve")
	}
}

// TestWireTurnMessagesToolCalls is the P7→wire pairing contract: an
// assistant message's COMPLETE tool calls are sent as its tool_calls list,
// immediately followed by one role=tool result per call, and the assistant
// entry + the result share the derived pairing id (tc<row-id>). IN-FLIGHT
// rows (result="", success=false — a crash between request and execution)
// are excluded from BOTH sides, so the pair can never orphan.
func TestWireTurnMessagesToolCalls(t *testing.T) {
	turns := []store.Message{
		{Role: roleUser, Content: "check the site", Seq: 0},
		{Role: roleAssistant, Content: "let me look", Seq: 1, ID: 101},
		{Role: roleAssistant, Content: "the page says hello", Seq: 2, ID: 102},
	}
	tcMap := map[int64][]store.ToolCall{
		101: {
			{ID: 501, MessageID: 101, Tool: "web_browser", Args: `{"command":"read"}`, Result: "the page says hello", Success: true},
			{ID: 502, MessageID: 101, Tool: "web_browser", Args: `{"command":"click"}`}, // in-flight (a crash case)
		},
	}
	got := wireTurnMessages(turns, tcMap, false)
	// [user, assistant{1 complete call}, tool{result}, assistant{final}]
	if len(got) != 4 {
		t.Fatalf("wire = %d messages, want 4:\n%+v", len(got), got)
	}
	if got[1].Role != provider.RoleAssistant || len(got[1].ToolCalls) != 1 {
		t.Fatalf("assistant message = %+v, want exactly one (the completed) tool call", got[1])
	}
	if got[1].ToolCalls[0].ID != "tc501" || got[1].ToolCalls[0].Name != "web_browser" || got[1].ToolCalls[0].Arguments != `{"command":"read"}` {
		t.Fatalf("tool_calls entry = %+v", got[1].ToolCalls[0])
	}
	if got[2].Role != provider.RoleTool || got[2].ToolCallID != "tc501" || got[2].Text != "the page says hello" {
		t.Fatalf("result message = %+v, want the paired tool result (id tc501)", got[2])
	}
	if got[3].Role != provider.RoleAssistant || got[3].Text != "the page says hello" || len(got[3].ToolCalls) != 0 {
		t.Fatalf("final message = %+v", got[3])
	}
	// The in-flight row appears NOWHERE on the wire.
	for i, m := range got {
		if strings.Contains(m.ToolCallID, "tc502") {
			t.Fatalf("in-flight row leaked into message %d: %+v", i, m)
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "tc502" {
				t.Fatalf("in-flight row leaked into message %d's tool_calls", i)
			}
		}
	}
}

// TestBuildRequestNilTCMapUnchanged: a conversation with no tool activity
// (a nil tcMap) produces EXACTLY the pre-tools request shape — the tools
// machinery must be invisible when idle.
func TestBuildRequestNilTCMapUnchanged(t *testing.T) {
	conv := &store.Conversation{ID: "c1", FrozenSystemPrompt: "You are Bevin."}
	msgs := []store.Message{
		{Role: roleUser, Content: "hello", Seq: 0},
		{Role: roleAssistant, Content: "hi", Seq: 1},
	}
	req, _ := buildRequest(conv, msgs, profile.Default(), nil)
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	for _, m := range req.Messages {
		if len(m.ToolCalls) != 0 || m.Role == provider.RoleTool {
			t.Fatalf("no-tool conversation leaked tool wire: %+v", m)
		}
	}
}

// TestTurnToolLoopRoundTrip drives the FULL loop through a real Agent: the
// model's first call requests a tool, the harness executes it (persisted as
// a P7 row before execution, completed after), and the second call's wire
// carries the assistant's tool_calls + the paired role=tool result — the
// reply is the final text, and the store holds exactly the expected rows
// (a tool result is NEVER a message row — the pair-safe invariant).
func TestTurnToolLoopRoundTrip(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	var echoArgs string
	var mu sync.Mutex
	if err := a.tools.Register(Tool{
		Name:        "fake_echo",
		Description: "echo its arguments back",
		Parameters:  map[string]any{"type": "object"},
		Execute: func(_ context.Context, _ string, args json.RawMessage) (string, error) {
			mu.Lock()
			echoArgs = string(args)
			mu.Unlock()
			return "echo:" + string(args), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	fp := &scriptedProvider{script: []provider.Result{
		{Text: "checking", ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "fake_echo", Arguments: `{"x":1}`}}},
		{Text: "the answer is 42"},
	}}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	_, reply, err := a.Turn(context.Background(), "u1", convID, "use the tool")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if reply != "the answer is 42" {
		t.Fatalf("reply = %q, want the final call's text", reply)
	}

	calls := fp.allCalls()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool call + final answer)", len(calls))
	}
	// The FIRST call carried the tool list (web_browser + fake_echo).
	names := map[string]bool{}
	for _, tl := range calls[0].Tools {
		names[tl.Name] = true
	}
	if !names["web_browser"] || !names["fake_echo"] {
		t.Fatalf("first call tools = %v, want web_browser + fake_echo", names)
	}
	// The SECOND call's wire: the user turn, the narration assistant with
	// its (model-side) tool call, the paired role=tool result, and nothing
	// else.
	m := calls[1].Messages
	if len(m) != 3 {
		t.Fatalf("second call wire = %d messages, want 3:\n%+v", len(m), m)
	}
	if m[0].Role != provider.RoleUser {
		t.Fatalf("wire[0] = %+v, want the user turn", m[0])
	}
	if m[1].Role != provider.RoleAssistant || len(m[1].ToolCalls) != 1 {
		t.Fatalf("wire[1] = %+v, want the narration with one tool call", m[1])
	}
	if !strings.HasPrefix(m[1].ToolCalls[0].ID, "tc") {
		t.Fatalf("pairing id = %q, want the derived tc<row-id>", m[1].ToolCalls[0].ID)
	}
	if m[2].Role != provider.RoleTool || m[2].ToolCallID != m[1].ToolCalls[0].ID {
		t.Fatalf("wire[2] = %+v, want the tool result paired to the same id", m[2])
	}
	if !strings.Contains(m[2].Text, "echo:{\"x\":1}") {
		t.Fatalf("tool result = %q, want the executor's output", m[2].Text)
	}
	mu.Lock()
	if echoArgs != `{"x":1}` {
		t.Fatalf("executor args = %q, want the model's raw JSON verbatim", echoArgs)
	}
	mu.Unlock()

	// Store: the P7 row is complete (executed, success), and a tool result
	// is NOT a message row (the pair-safe invariant — the compactor's
	// slicing can never split a pair).
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	var narrations, toolRows int
	var narrationID int64
	for _, msg := range msgs {
		switch msg.Role {
		case roleAssistant:
			if msg.Content == "checking" {
				narrations++
				narrationID = msg.ID
			}
		case roleTool:
			toolRows++
		}
	}
	if narrations != 1 {
		t.Fatalf("narration assistant rows = %d, want 1", narrations)
	}
	if toolRows != 0 {
		t.Fatalf("role=tool MESSAGE rows = %d, want 0 (results live only in P7)", toolRows)
	}
	rows, err := us.ToolCallsForMessage(narrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Success || rows[0].Tool != "fake_echo" || !strings.Contains(rows[0].Result, "echo:") {
		t.Fatalf("P7 rows = %+v, want one completed fake_echo row", rows)
	}
}

// TestTurnNoProgressStop: a runaway model that returns the SAME tool call
// every time (the worst-case loop) is stopped by the no-progress confidence
// accumulator — WELL BEFORE the old 21-iteration cap — the reply carries the
// "stopped, not done" notice, the final call went out with NO tools on the
// wire, and the unexecuted calls are recorded as FAILED P7 rows (complete
// pairs: the model sees why they never ran).
func TestTurnNoProgressStop(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	if err := a.tools.Register(Tool{
		Name:        "fake_loop",
		Description: "a tool a runaway model loops on",
		Parameters:  map[string]any{"type": "object"},
		Execute:     func(context.Context, string, json.RawMessage) (string, error) { return "still looping", nil },
	}); err != nil {
		t.Fatal(err)
	}
	// The model ALWAYS returns the same tool call (the worst case).
	fp := &scriptedProvider{script: make([]provider.Result, 40)}
	for i := range fp.script {
		fp.script[i] = provider.Result{
			Text:      "looping",
			ToolCalls: []provider.ToolCall{{ID: "call", Name: "fake_loop", Arguments: `{}`}},
		}
	}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	_, reply, err := a.Turn(context.Background(), "u1", convID, "loop")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	// The reply carries the "stopped, not done" notice (the user must know the
	// trajectory was stopped, not finished — a silent cut would look complete).
	if !strings.Contains(reply, "stopped this turn") {
		t.Fatalf("reply = %q, want the 'stopped, not done' notice", reply)
	}
	calls := fp.allCalls()
	// Stopped well BEFORE the old 21-iteration cap (the confidence threshold
	// fires on the sustained repeat), but it did loop a few times first.
	if len(calls) > 15 {
		t.Errorf("provider calls = %d, want well under the old 21-iteration cap", len(calls))
	}
	if len(calls) < 5 {
		t.Errorf("provider calls = %d, want at least a few iterations before the stop", len(calls))
	}
	// The final call went out with NO tools on the wire (the forced text-only
	// call).
	if got := len(calls[len(calls)-1].Tools); got != 0 {
		t.Errorf("final call tools = %d, want 0 (the forced text-only call)", got)
	}
	// The final call's call was recorded as a failed row, never executed; the
	// earlier iterations' calls WERE executed.
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	var executed, notExecuted int
	for _, msg := range msgs {
		if msg.Role != roleAssistant {
			continue
		}
		rows, err := us.ToolCallsForMessage(msg.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if strings.HasPrefix(row.Result, "not executed") {
				notExecuted++
			} else {
				executed++
			}
		}
	}
	if executed < 5 {
		t.Errorf("executed tool calls = %d, want at least 5 (the model looped before stopping)", executed)
	}
	if notExecuted != 1 {
		t.Errorf("not-executed rows = %d, want 1 (the final call's single call)", notExecuted)
	}
}

// TestExecuteToolCallUnknown: an unknown tool name (a model imitating a
// removed/renamed tool from its own history — the self-healing path of the
// tool-evolution invariants) gets a structured error naming the available
// tools, as a RESULT (ok=false), never a turn failure.
func TestExecuteToolCallUnknown(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	out, ok := a.executeToolCall(context.Background(), "u1", provider.ToolCall{Name: "no_such_tool", Arguments: "{}"})
	if ok {
		t.Fatal("an unknown tool must report failure")
	}
	if !strings.Contains(out, "unknown tool") || !strings.Contains(out, "web_browser") {
		t.Fatalf("unknown-tool result = %q, want it naming the available tools (incl. web_browser)", out)
	}
}

// TestWebBrowserToolRegistered: the web_browser tool (WS5) is registered by
// default with its command surface — the model's first look at the tool is
// its schema, and the command enum must match the executor's cases. The
// security pin: NO evaluate / expression surface (model-controlled JS in
// the page was removed 2026-11-19 — a prompt-injected page could route
// attacker code into the user's logged-in profile).
func TestWebBrowserToolRegistered(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	tl, ok := a.tools.Get("web_browser")
	if !ok {
		t.Fatal("web_browser must be registered by default")
	}
	schema, _ := json.Marshal(tl.Parameters)
	for _, want := range []string{`"openTab"`, `"navigate"`, `"read"`, `"readItems"`, `"scroll"`, `"click"`, `"type"`, `"select"`, `"waitFor"`, `"listTabs"`, `"closeTab"`} {
		if !strings.Contains(string(schema), want) {
			t.Fatalf("web_browser schema missing command %s:\n%s", want, schema)
		}
	}
	if strings.Contains(string(schema), `evaluate`) || strings.Contains(string(schema), `expression`) {
		t.Fatalf("web_browser schema must NOT expose model-controlled JS (evaluate/expression):\n%s", schema)
	}
}

// TestTurnStreamExecutesToolCalls pins the PRODUCTION failure end to end:
// the chat UI STREAMS every turn, and the OAI wire delivers a model tool
// call as delta.tool_calls fragments. Before the stream decoder learned
// them, a tool call arrived as an EMPTY reply (finish_reason "tool_calls",
// no content) — the harness saw zero tool calls, ended the turn, and the
// user got an empty bubble (the 2026-11-19 sephora.com conversation: the
// model's reasoning said it would open the site, nothing executed). This
// drives a REAL streaming OpenAI provider (an SSE fake) through the full
// turn: the first call streams a tool call, the harness must execute it
// (P7 row) and call again with the paired result; the second call streams
// the final text.
func TestTurnStreamExecutesToolCalls(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	var mu sync.Mutex
	var echoRan bool
	if err := a.tools.Register(Tool{
		Name:        "fake_echo",
		Description: "echo its arguments back",
		Parameters:  map[string]any{"type": "object"},
		Execute: func(_ context.Context, _ string, args json.RawMessage) (string, error) {
			mu.Lock()
			echoRan = true
			mu.Unlock()
			return "echo-result " + string(args), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The fake OAI endpoint streams: call 1 = a tool call (empty content),
	// call 2 = the final text.
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, string(b))
		n := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		write := func(s string) { _, _ = w.Write([]byte(s)); f.Flush() }
		if n == 1 {
			write("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"use the tool\"}}]}\n\n")
			write("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"fake_echo\",\"arguments\":\"{\\\"x\\\":1}\"}}]}}]}\n\n")
			write("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n")
		} else {
			write("data: {\"choices\":[{\"delta\":{\"content\":\"done: \"}}]}\n\n")
			write("data: {\"choices\":[{\"delta\":{\"content\":\"echo-result {\\\"x\\\":1}\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n")
		}
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	a.providers["m1"] = &provider.OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: &http.Client{}}

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	_, reply, err := a.TurnStream(context.Background(), "u1", convID, "use the tool", func(provider.Chunk) error { return nil })
	if err != nil {
		t.Fatalf("TurnStream: %v", err)
	}
	if reply != "done: echo-result {\"x\":1}" {
		t.Fatalf("reply = %q, want the final call's streamed text (a dropped tool call would end the turn with an empty reply)", reply)
	}
	mu.Lock()
	if !echoRan {
		t.Error("the streamed tool call was never executed (the dropped-tool-call bug)")
	}
	if len(calls) != 2 {
		t.Errorf("provider calls = %d, want 2 (tool call + final answer)", len(calls))
	}
	if len(calls) == 2 {
		// The SECOND call's wire must carry the executed result back to
		// the model (the loop fed it the tool result).
		if !strings.Contains(calls[1], "echo-result") {
			t.Errorf("second call wire lacks the tool result:\\n%s", calls[1])
		}
	}
	mu.Unlock()

	// The P7 row for the streamed call exists and is complete.
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	var p7 int
	for _, msg := range msgs {
		if msg.Role != roleAssistant {
			continue
		}
		rows, err := us.ToolCallsForMessage(msg.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Tool == "fake_echo" && row.Success {
				p7++
			}
		}
	}
	if p7 != 1 {
		t.Fatalf("completed fake_echo P7 rows = %d, want 1", p7)
	}
}

// TestParseElementID: the #N ids the model references (from the last read's
// element table) — the parse is strict (a bare number or a non-number is a
// model error the tool reports, not a crash).
func TestParseElementID(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"#1", 1, false},
		{"  #42 ", 42, false},
		{"#0", 0, true},
		{"5", 0, true},
		{"#abc", 0, true},
		{"#", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := parseElementID(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("parseElementID(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("parseElementID(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
}

// TestResolveTarget: click/type/select resolve their target from a #N id
// (server-side, against the LATEST read's table — a stale id fails with the
// read-again hint) or from a raw selector (the advanced fallback); neither
// is an argument error the model must fix.
func TestResolveTarget(t *testing.T) {
	inst := &browser.Instance{} // zero value: no read yet
	if _, err := resolveTarget(inst, "#3", ""); err == nil || !strings.Contains(err.Error(), "not in the last read") {
		t.Fatalf("stale #N err = %v, want the read-again hint", err)
	}
	if _, err := resolveTarget(inst, "", ""); err == nil {
		t.Fatal("no target must be an error")
	}
	if got, err := resolveTarget(inst, "", "body > a"); err != nil || got != "body > a" {
		t.Fatalf("raw selector = %q, %v", got, err)
	}
}
