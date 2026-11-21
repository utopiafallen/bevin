package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// The idle learning loop + memory tools.

// fastLearning wires the test agent for fast learning passes: a fake model
// that returns the canned pass reply, and sub-second idle/deferral timers.
func fastLearning(t *testing.T, a *Agent, passReply string) {
	t.Helper()
	fake := &fakeProvider{replies: []string{passReply}}
	a.mu.Lock()
	a.providers["m1"] = fake
	a.mu.Unlock()
	a.learn.delay = 20 * time.Millisecond
	a.learn.busyDeferral = 20 * time.Millisecond
}

// enableLearning opts a test into the idle learning loop (disabled by
// default — see learningEnabled). Restored after the test.
func enableLearning(t *testing.T) {
	t.Helper()
	learningEnabled = true
	t.Cleanup(func() { learningEnabled = false })
}

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

// appendTurns adds user/assistant turns to the conversation.
func appendTurns(t *testing.T, us *store.UserStore, convID string, pairs ...[2]string) {
	t.Helper()
	for _, p := range pairs {
		if _, err := us.AppendMessage(convID, "user", p[0], "", "", nil); err != nil {
			t.Fatalf("AppendMessage user: %v", err)
		}
		if _, err := us.AppendMessage(convID, "assistant", p[1], "", "", nil); err != nil {
			t.Fatalf("AppendMessage assistant: %v", err)
		}
	}
}

const passJSON = `{"memories":[{"facet":"preferences","statement":"The user prefers tea over coffee","subject":"user","confidence":0.9}]}`

func TestIdleLearningPass(t *testing.T) {
	a, _, _ := newTestAgent(t)
	enableLearning(t)
	convID, us := newTestConversation(t, a, "u1")
	appendTurns(t, us, convID,
		[2]string{"I'd like to set up a project.", "What kind of project?"},
		[2]string{"By the way, I prefer tea over coffee.", "Noted."},
	)
	fastLearning(t, a, passJSON)

	a.learn.onTurnComplete("u1", convID)

	deadline := time.Now().Add(3 * time.Second)
	for {
		mems, err := us.ListActiveMemories()
		if err != nil {
			t.Fatalf("ListActiveMemories: %v", err)
		}
		if len(mems) == 1 {
			if mems[0].Facet != "preferences" || mems[0].Statement != "The user prefers tea over coffee" {
				t.Fatalf("unexpected memory: %+v", mems[0])
			}
			if mems[0].Provenance != "learn:"+convID {
				t.Fatalf("provenance = %q", mems[0].Provenance)
			}
			if mems[0].Confidence != 0.9 {
				t.Fatalf("confidence = %v", mems[0].Confidence)
			}
			ids, err := us.MemoryProvenance(mems[0].ID)
			if err != nil || len(ids) != 4 {
				t.Fatalf("structured provenance = %v, err %v (want the 4 new message ids)", ids, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the learning pass did not run (memories: %d)", len(mems))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The meta call: no conversation_id on the wire (the KV-cache rule) and
	// the pass's own system prompt.
	fake := a.providers["m1"].(*fakeProvider)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 1 {
		t.Fatalf("want exactly 1 meta call, got %d", len(fake.calls))
	}
	req := fake.calls[0]
	if req.ConversationID != "" {
		t.Fatalf("the learning meta call must carry NO conversation_id, got %q", req.ConversationID)
	}
	if len(req.System) == 0 || !strings.Contains(req.System[0].Text, "memory-learning pass") {
		t.Fatalf("want the learning system prompt, got %v", req.System)
	}
}

// TestIdleLearningMinUserTurns: one new user turn is not enough (the
// assumed threshold) — the window closes without a pass.
func TestIdleLearningMinUserTurns(t *testing.T) {
	a, _, _ := newTestAgent(t)
	enableLearning(t)
	convID, us := newTestConversation(t, a, "u1")
	appendTurns(t, us, convID, [2]string{"One quick question.", "Here is the answer."})
	fastLearning(t, a, passJSON)

	a.learn.onTurnComplete("u1", convID)
	time.Sleep(150 * time.Millisecond) // well past the fast delay

	mems, err := us.ListActiveMemories()
	if err != nil {
		t.Fatalf("ListActiveMemories: %v", err)
	}
	if len(mems) != 0 {
		t.Fatalf("want no memories (only 1 new user turn), got %+v", mems)
	}
}

// TestIdleLearningBusyGate: a user with running LLM work is not learned on —
// the pass defers, then runs once the work finishes.
func TestIdleLearningBusyGate(t *testing.T) {
	a, _, _ := newTestAgent(t)
	enableLearning(t)
	convID, us := newTestConversation(t, a, "u1")
	appendTurns(t, us, convID,
		[2]string{"Turn one.", "Reply one."},
		[2]string{"Turn two.", "Reply two."},
	)
	fastLearning(t, a, passJSON)

	// The user is busy: arm while the counter is up.
	a.llmWork.enter("u1")
	a.learn.onTurnComplete("u1", convID)
	time.Sleep(50 * time.Millisecond) // first fire hits the busy gate + deferral
	mems, _ := us.ListActiveMemories()
	if len(mems) != 0 {
		t.Fatalf("the busy gate failed — learned on a working user: %+v", mems)
	}
	// The work finishes: the deferred pass runs.
	a.llmWork.leave("u1")
	time.Sleep(100 * time.Millisecond)
	mems, _ = us.ListActiveMemories()
	if len(mems) != 1 {
		t.Fatalf("the deferred pass did not run after the user went idle: %d memories", len(mems))
	}
}

// TestIdleLearningNoModel: the active model unavailable → the pass skips
// (no crash), and the window is NOT advanced (a later idle window retries).
func TestIdleLearningNoModel(t *testing.T) {
	a, _, _ := newTestAgent(t)
	enableLearning(t)
	convID, us := newTestConversation(t, a, "u1")
	appendTurns(t, us, convID,
		[2]string{"One.", "a."},
		[2]string{"Two.", "b."},
	)
	a.learn.delay = 20 * time.Millisecond
	// Replace the provider with one that fails (the model is unavailable to
	// the pass).
	a.mu.Lock()
	a.providers["m1"] = &failProvider{}
	a.mu.Unlock()

	a.learn.onTurnComplete("u1", convID)
	time.Sleep(100 * time.Millisecond)
	mems, _ := us.ListActiveMemories()
	if len(mems) != 0 {
		t.Fatalf("a failing model must not produce memories: %+v", mems)
	}
}

// TestIdleLearningDeletedConversation: the conversation deleted before the
// pass fires → the pass drops the state cleanly.
func TestIdleLearningDeletedConversation(t *testing.T) {
	a, _, _ := newTestAgent(t)
	enableLearning(t)
	convID, us := newTestConversation(t, a, "u1")
	appendTurns(t, us, convID,
		[2]string{"One.", "a."},
		[2]string{"Two.", "b."},
	)
	fastLearning(t, a, passJSON)
	a.learn.delay = 50 * time.Millisecond
	a.learn.onTurnComplete("u1", convID)
	if err := us.DeleteConversation(convID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	a.learn.mu.Lock()
	_, still := a.learn.st[learningKey("u1", convID)]
	a.learn.mu.Unlock()
	if still {
		t.Fatalf("the deleted conversation's state should be dropped")
	}
}

// TestIdleLearningRetryOnBadJSON: an unparseable pass reply skips the pass
// WITHOUT advancing the window — the next idle window retries the same turns.
func TestIdleLearningRetryOnBadJSON(t *testing.T) {
	a, _, _ := newTestAgent(t)
	enableLearning(t)
	convID, us := newTestConversation(t, a, "u1")
	appendTurns(t, us, convID,
		[2]string{"One.", "a."},
		[2]string{"Two.", "b."},
	)
	fake := &fakeProvider{replies: []string{"sure, happy to help!"}}
	a.mu.Lock()
	a.providers["m1"] = fake
	a.mu.Unlock()
	a.learn.delay = 20 * time.Millisecond
	a.learn.busyDeferral = 20 * time.Millisecond

	a.learn.onTurnComplete("u1", convID)
	time.Sleep(100 * time.Millisecond)
	mems, _ := us.ListActiveMemories()
	if len(mems) != 0 {
		t.Fatalf("bad JSON must not produce memories: %+v", mems)
	}
	// The window did not advance: re-arming with a GOOD reply now learns the
	// same turns.
	fake.mu.Lock()
	fake.replies = []string{passJSON}
	fake.mu.Unlock()
	a.learn.onTurnComplete("u1", convID)
	time.Sleep(100 * time.Millisecond)
	mems, _ = us.ListActiveMemories()
	if len(mems) != 1 {
		t.Fatalf("the retry should have learned the turns: %d memories", len(mems))
	}
}

// ---- memory tools ----

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

// TestIdleLearningDisabledByDefault pins the 2026-11-20 user decision: the
// idle learning loop is OFF by default (its passes are being redesigned as
// background TASKS). The arming hook is a no-op until enabled, so a clean
// idle conversation learns nothing.
func TestIdleLearningDisabledByDefault(t *testing.T) {
	if learningEnabled {
		t.Fatal("the idle learning loop must be disabled by default (task-system redesign pending)")
	}
	a, _, _ := newTestAgent(t)
	convID, us := newTestConversation(t, a, "u1")
	appendTurns(t, us, convID,
		[2]string{"One.", "a."},
		[2]string{"Two.", "b."},
	)
	fastLearning(t, a, passJSON)
	a.learn.onTurnComplete("u1", convID)
	time.Sleep(100 * time.Millisecond) // well past the fast delay
	mems, _ := us.ListActiveMemories()
	if len(mems) != 0 {
		t.Fatalf("a disabled loop must not learn: %+v", mems)
	}
}

// TestParseLearningReply covers the reply parsing (fenced + bare + garbage).
func TestParseLearningReply(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{`{"memories":[]}`, 0, false},
		{passJSON, 1, false},
		{"```json\n" + passJSON + "\n```", 1, false},
		{"```" + passJSON + "```", 1, false},
		{"here you go:\n" + passJSON, 0, true},
		{"not json at all", 0, true},
	}
	for _, c := range cases {
		got, err := parseLearningReply(c.in)
		if c.wantErr != (err != nil) {
			t.Fatalf("parse(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if !c.wantErr && len(got) != c.want {
			t.Fatalf("parse(%q): %d memories, want %d", c.in, len(got), c.want)
		}
	}
}

// failProvider is a provider whose calls always fail (an unavailable model).
type failProvider struct{}

func (f *failProvider) Name() string { return "fail" }
func (f *failProvider) Chat(_ context.Context, _ string, _ provider.ChatRequest) (*provider.Result, error) {
	return nil, errUnavailable
}
func (f *failProvider) Stream(_ context.Context, _ string, _ provider.ChatRequest, _ func(provider.Chunk) error) (*provider.Result, error) {
	return nil, errUnavailable
}
func (f *failProvider) Models(_ context.Context) ([]provider.ModelInfo, error) {
	return nil, errUnavailable
}
func (f *failProvider) Embed(_ context.Context, _ string, _ []string) ([][]float32, error) {
	return nil, errUnavailable
}

var errUnavailable = &modelUnavailableErr{}

type modelUnavailableErr struct{}

func (e *modelUnavailableErr) Error() string { return "model unavailable" }
