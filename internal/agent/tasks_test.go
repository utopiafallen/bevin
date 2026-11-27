package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// gateProvider blocks its Chat call until its gate closes (a long model
// turn) and counts entries, so tests can observe the call before releasing.
type gateProvider struct {
	mu    sync.Mutex
	calls int
	gate  chan struct{}
}

func (p *gateProvider) Name() string { return "gate" }

func (p *gateProvider) Chat(_ context.Context, _ string, _ provider.ChatRequest) (*provider.Result, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-p.gate
	return &provider.Result{Text: "late answer"}, nil
}

func (p *gateProvider) Stream(_ context.Context, _ string, _ provider.ChatRequest, _ func(provider.Chunk) error) (*provider.Result, error) {
	return p.Chat(context.Background(), "", provider.ChatRequest{})
}

func (p *gateProvider) Models(_ context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p *gateProvider) Embed(_ context.Context, _ string, _ []string) ([][]float32, error) {
	return nil, nil
}

func (p *gateProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// errProvider fails every call (an infra/model failure).
type errProvider struct{ err error }

func (p errProvider) Name() string { return "err" }

func (p errProvider) Chat(context.Context, string, provider.ChatRequest) (*provider.Result, error) {
	return nil, p.err
}

func (p errProvider) Stream(context.Context, string, provider.ChatRequest, func(provider.Chunk) error) (*provider.Result, error) {
	return nil, p.err
}

func (p errProvider) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p errProvider) Embed(context.Context, string, []string) ([][]float32, error) { return nil, nil }

// waitTaskState polls the task's durable state until it is `want` (the
// runner settles asynchronously on its own goroutine).
func waitTaskState(t *testing.T, a *Agent, handle, taskID, want string) *store.Task {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		us, err := a.userStore(handle)
		if err != nil {
			t.Fatalf("userStore: %v", err)
		}
		task, err := us.GetTask(taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.State == want {
			return task
		}
		last = task.State
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s never reached %q (last state %q)", taskID, want, last)
	return nil
}

// waitFor polls cond until true (or fails the test).
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestTaskSpawnRunsAndCompletes is the core path: the harness spawns a task
// from an origin conversation; the agent works it to completion in the
// task's owned conversation (on the task's EXPLICIT model target — the
// origin's model is not a target by itself); the task settles done and the
// completion summary (its final reply) is posted to the origin — the audit
// trail.
func TestTaskSpawnRunsAndCompletes(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// The explicit target is m2 (the only model with a provider here);
	// the origin happens to be on m2 too — the point is that the target
	// is the task's, not the origin's.
	fp := &fakeProvider{replies: []string{"I reviewed the day and learned that the user ships on Fridays."}}
	a.providers["m2"] = fp

	origin, err := a.CreateConversation(context.Background(), "u1", "origin", "m2", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "nightly review", Description: "Review the day's conversations.", OriginConvID: origin,
		Model: "m2",
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	done := waitTaskState(t, a, "u1", task.ID, "done")
	if done.ConversationID == nil || *done.ConversationID == "" {
		t.Fatal("the done task has no owned conversation")
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	// The owned conversation took the task's explicit model target.
	conv, err := us.GetConversation(*done.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ActiveModelID != "m2" {
		t.Fatalf("task conversation model = %q, want the explicit target m2", conv.ActiveModelID)
	}
	// The work ran in the task conversation: the framed work message first,
	// the reply as the assistant message (a no-progress checkin row may
	// trail it — the roleCheckin turn machinery, unrelated to the task).
	msgs, err := us.ListMessages(*done.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 2 {
		t.Fatalf("task conversation has %d messages, want >= 2 (work + reply)", len(msgs))
	}
	if msgs[0].Role != roleUser || !contains(msgs[0].Content, "nightly review") || !contains(msgs[0].Content, "Review the day's conversations.") {
		t.Fatalf("first task-conversation message = %+v, want the framed work (title + description)", msgs[0])
	}
	var reply *store.Message
	for i := range msgs {
		if msgs[i].Role == roleAssistant {
			reply = &msgs[i]
			break
		}
	}
	if reply == nil || !contains(reply.Content, "ships on Fridays") {
		t.Fatalf("task conversation has no assistant reply: %+v", msgs)
	}
	// The completion summary was posted to the origin (an assistant message
	// — the audit trail the user reads in their own conversation).
	omsgs, err := us.ListMessages(origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(omsgs) != 1 {
		t.Fatalf("origin has %d messages, want 1 (the completion summary)", len(omsgs))
	}
	s := omsgs[0]
	if s.Role != roleAssistant || !contains(s.Content, "Task \"nightly review\" is complete:") || !contains(s.Content, "ships on Fridays") {
		t.Fatalf("origin message = %+v, want the framed completion summary", s)
	}
}

// TestTaskNoOriginStaysLocal: a harness task with no origin runs on any
// available main model (m1 here — the config-order tie-break of two
// idle pools), and its summary stays in the task conversation (nothing is
// posted anywhere else).
func TestTaskNoOriginStaysLocal(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fp := &fakeProvider{replies: []string{"done the local work"}}
	a.providers["m1"] = fp

	// A bystander conversation that must receive nothing.
	bystander, err := a.CreateConversation(context.Background(), "u1", "bystander", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "local chore", Description: "Do the local chore.",
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	done := waitTaskState(t, a, "u1", task.ID, "done")
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	// The task conversation ran on the user default (m1).
	conv, err := us.GetConversation(*done.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ActiveModelID != "m1" {
		t.Fatalf("task conversation model = %q, want the user default m1", conv.ActiveModelID)
	}
	// The bystander received nothing.
	bmsgs, err := us.ListMessages(bystander)
	if err != nil {
		t.Fatal(err)
	}
	if len(bmsgs) != 0 {
		t.Fatalf("bystander conversation has %d messages, want 0 (no origin, nothing posted)", len(bmsgs))
	}
}

// TestTaskQueuedCancel: with the model's only slot held by an interactive
// turn, a spawned task that EXPLICITLY targets that model is accepted
// (in-progress — its turn is queued in the pool) but not yet at the model.
// StopTask cancels it; when the slot frees, the interactive turn finishes
// and the task never runs (no provider call for it, no work message).
func TestTaskQueuedCancel(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	gp := &gateProvider{gate: make(chan struct{})}
	a.providers["m1"] = gp

	conv, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		a.Turn(context.Background(), "u1", conv, "a long interactive turn", nil)
	}()
	waitFor(t, func() bool { return gp.callCount() >= 1 }, "the interactive turn to hold m1's only slot")

	// Explicit target m1: with the default any-model rule the task would
	// pick the idle m2 pool and never queue behind the interactive turn.
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "queued behind the busy slot", Description: "never reaches the model",
		Model: "m1",
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	// Accepted: the start flip happened; the turn is queued for the slot.
	waitTaskState(t, a, "u1", task.ID, "in-progress")
	if err := a.StopTask(context.Background(), "u1", task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	waitTaskState(t, a, "u1", task.ID, "canceled")

	close(gp.gate)
	<-turnDone
	if n := gp.callCount(); n != 1 {
		t.Fatalf("provider calls = %d, want 1 (the canceled task must never reach the model)", n)
	}
	// Canceled while queued: the turn never ran, so no work message.
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := us.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := us.ListMessages(*tk.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("canceled-while-queued task conversation has %d messages, want 0 (the turn never ran)", len(msgs))
	}
}

// TestTaskRunningCancel: a task already at the model is canceled mid-turn.
// It settles canceled, its partial transcript stays in the task
// conversation (the audit trail), and no summary is posted.
func TestTaskRunningCancel(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	gp := &gateProvider{gate: make(chan struct{})}
	a.providers["m1"] = gp

	origin, err := a.CreateConversation(context.Background(), "u1", "origin", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "the long one", Description: "a long job", OriginConvID: origin,
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	waitTaskState(t, a, "u1", task.ID, "in-progress")
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	// The task row now carries its owned conversation (the spawn returns
	// the pre-link row; the runner works the re-read).
	tk, err := us.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.ConversationID == nil || *tk.ConversationID == "" {
		t.Fatal("task has no owned conversation")
	}
	// The turn reached the model (the work message is persisted).
	waitFor(t, func() bool {
		msgs, err := us.ListMessages(*tk.ConversationID)
		return err == nil && len(msgs) >= 1
	}, "the task turn to reach the model")

	if err := a.StopTask(context.Background(), "u1", task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	waitTaskState(t, a, "u1", task.ID, "canceled")
	close(gp.gate) // release the blocked call; the turn unwinds

	// The partial transcript is the audit trail (the work message survived).
	msgs, err := us.ListMessages(*tk.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 1 {
		t.Fatal("canceled task conversation is empty — the partial transcript (audit trail) was lost")
	}
	// No summary was posted to the origin.
	omsgs, err := us.ListMessages(origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(omsgs) != 0 {
		t.Fatalf("origin has %d messages, want 0 (a canceled task posts no summary)", len(omsgs))
	}
}

// TestTaskFailureBlocked: a model/infra failure settles the task blocked,
// and its card status area (the conversation's last message) says why.
func TestTaskFailureBlocked(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	a.providers["m1"] = errProvider{err: errors.New("model exploded")}

	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "doomed", Description: "the model is down",
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	blocked := waitTaskState(t, a, "u1", task.ID, "blocked")
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := us.ListMessages(*blocked.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != roleAssistant || !contains(last.Content, "Task failed:") || !contains(last.Content, "model exploded") {
		t.Fatalf("card status (last message) = %+v, want the failure recorded", last)
	}
}

// TestTaskBootReset: a task in-progress when the process died (simulated by
// writing the row directly) resets to todo on boot and is worked again —
// the re-dispatched run reaches the model and settles done.
func TestTaskBootReset(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	gp := &gateProvider{gate: make(chan struct{})}
	a.providers["m1"] = gp

	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	task, err := us.CreateTask("crashed", "was in-progress when the process died", "todo", nil)
	if err != nil {
		t.Fatal(err)
	}
	convID, err := a.CreateConversation(context.Background(), "u1", "task: crashed", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetTaskConversation(task.ID, convID); err != nil {
		t.Fatal(err)
	}
	if err := us.SetTaskState(task.ID, "in-progress"); err != nil {
		t.Fatal(err)
	}

	a.tasks.resetStaleOnBoot()

	// The reset re-dispatched the task: its run reached the model.
	waitFor(t, func() bool { return gp.callCount() >= 1 }, "the re-dispatched task to run")
	close(gp.gate)
	waitTaskState(t, a, "u1", task.ID, "done")
}

// TestTaskModelDefaultIgnoresOrigin: the task model rule — an untargeted
// task does NOT take its origin conversation's model (the origin is the
// audit destination, not a target): it runs on any available main model
// (m1 here — both pools idle, config-order tie-break), while the origin
// sits on m2.
func TestTaskModelDefaultIgnoresOrigin(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	a.providers["m1"] = &fakeProvider{replies: []string{"ran on the any-model pick"}}
	a.providers["m2"] = &fakeProvider{replies: []string{"never used"}}

	origin, err := a.CreateConversation(context.Background(), "u1", "origin", "m2", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "no target", Description: "runs on any available model.", OriginConvID: origin,
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	done := waitTaskState(t, a, "u1", task.ID, "done")
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := us.GetConversation(*done.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ActiveModelID != "m1" {
		t.Fatalf("task conversation model = %q, want the any-model pick m1 (the origin's m2 must not be implied)", conv.ActiveModelID)
	}
}

// TestTaskSpawnValidation: spawn refuses empty title/description, an
// unknown origin conversation, and an unknown/unavailable model target.
func TestTaskSpawnValidation(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	if _, err := a.SpawnTask(context.Background(), "u1", TaskSpec{Title: "", Description: "x"}); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := a.SpawnTask(context.Background(), "u1", TaskSpec{Title: "t", Description: ""}); err == nil {
		t.Fatal("empty description accepted")
	}
	if _, err := a.SpawnTask(context.Background(), "u1", TaskSpec{Title: "t", Description: "x", OriginConvID: "no-such-conversation"}); err == nil {
		t.Fatal("unknown origin conversation accepted")
	}
	if _, err := a.SpawnTask(context.Background(), "u1", TaskSpec{Title: "t", Description: "x", Model: "no-such-model"}); err == nil {
		t.Fatal("unknown model target accepted")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// toolCallProvider queues whole Results per call — a tool call first, then
// the final text — so tests can drive the advance_task report path (the
// plain fakeProvider only answers with text).
type toolCallProvider struct {
	mu      sync.Mutex
	calls   int
	results []provider.Result
	errs    []error // per-call index; a non-nil entry makes that call fail
}

func (p *toolCallProvider) Name() string { return "tcall" }

func (p *toolCallProvider) Chat(_ context.Context, _ string, _ provider.ChatRequest) (*provider.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.calls
	p.calls++
	if i < len(p.errs) && p.errs[i] != nil {
		return nil, p.errs[i]
	}
	if i < len(p.results) {
		return &p.results[i], nil
	}
	return &provider.Result{Text: "ok"}, nil
}

func (p *toolCallProvider) Stream(_ context.Context, _ string, _ provider.ChatRequest, _ func(provider.Chunk) error) (*provider.Result, error) {
	return p.Chat(context.Background(), "", provider.ChatRequest{})
}

func (p *toolCallProvider) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p *toolCallProvider) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func advanceCall(args string) provider.Result {
	return provider.Result{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "advance_task", Arguments: args}}}
}

// TestTaskAdvanceBlocked: the model reports its task blocked (with a note);
// the task settles BLOCKED — not done — and the origin receives the blocked
// summary (note + final reply), the audit trail of why it needs attention.
func TestTaskAdvanceBlocked(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	p := &toolCallProvider{results: []provider.Result{
		advanceCall(`{"state":"blocked","note":"the archive API key is not configured"}`),
		{Text: "I could not finish: the archive API key is missing."},
	}}
	a.providers["m1"] = p

	origin, err := a.CreateConversation(context.Background(), "u1", "origin", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "fetch the archive", Description: "Pull the archive export.", OriginConvID: origin,
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	waitTaskState(t, a, "u1", task.ID, "blocked")

	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	omsgs, err := us.ListMessages(origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(omsgs) != 1 {
		t.Fatalf("origin has %d messages, want 1 (the blocked summary)", len(omsgs))
	}
	s := omsgs[0]
	if s.Role != roleAssistant || !contains(s.Content, `Task "fetch the archive" is blocked:`) ||
		!contains(s.Content, "the archive API key is not configured") ||
		!contains(s.Content, "I could not finish") {
		t.Fatalf("origin message = %+v, want the blocked summary (framing + note + reply)", s)
	}
}

// TestTaskAdvanceDone: the model reports done (with a note) — the task
// settles done and the origin gets the complete summary (note ahead of the
// final reply).
func TestTaskAdvanceDone(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	p := &toolCallProvider{results: []provider.Result{
		advanceCall(`{"state":"done","note":"archived 12 items"}`),
		{Text: "Archive pull finished; 12 items stored."},
	}}
	a.providers["m1"] = p

	origin, err := a.CreateConversation(context.Background(), "u1", "origin", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "pull the archive", Description: "Pull the archive export.", OriginConvID: origin,
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	waitTaskState(t, a, "u1", task.ID, "done")

	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	omsgs, err := us.ListMessages(origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(omsgs) != 1 {
		t.Fatalf("origin has %d messages, want 1", len(omsgs))
	}
	s := omsgs[0]
	if !contains(s.Content, `Task "pull the archive" is complete:`) || !contains(s.Content, "archived 12 items") || !contains(s.Content, "12 items stored") {
		t.Fatalf("origin message = %+v, want the complete summary (note + reply)", s)
	}
}

// TestTaskAdvanceIgnoredOnInfraError: the model reports done, then the model
// fails — the objective infra failure wins: the task settles blocked, and
// the (failed) run's report does not leak into anything.
func TestTaskAdvanceIgnoredOnInfraError(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	p := &toolCallProvider{
		results: []provider.Result{advanceCall(`{"state":"done","note":"almost"}`)},
		errs:    []error{nil, errors.New("model exploded")},
	}
	a.providers["m1"] = p

	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "half done", Description: "the model dies on the second call",
	})
	if err != nil {
		t.Fatalf("SpawnTask: %v", err)
	}
	blocked := waitTaskState(t, a, "u1", task.ID, "blocked")
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := us.ListMessages(*blocked.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != roleAssistant || !contains(last.Content, "Task failed:") {
		t.Fatalf("card status (last message) = %+v, want the failure recorded (the done report must not win)", last)
	}
}

// TestAdvanceTaskScoping: outside a task conversation the tool refuses
// (structured, model-readable) and the wire never offers it; in a task's
// owned conversation the wire offers it and the report is accepted.
func TestAdvanceTaskScoping(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	tool, ok := a.tools.Get("advance_task")
	if !ok {
		t.Fatal("advance_task is not registered")
	}
	// A plain chat conversation: the wire filters the tool out, and a
	// direct call (the model imitating it from history) is refused.
	chat, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	var offered bool
	for _, wt := range a.wireTools(nil, nil) {
		if wt.Name == "advance_task" {
			offered = true
		}
	}
	if offered {
		t.Fatal("advance_task offered to a non-task conversation")
	}
	res, err := tool.Execute(context.WithValue(context.Background(), ctxConversationKey, chat), "u1", []byte(`{"state":"done"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "not a task") {
		t.Fatalf("chat-conversation call result = %q, want the not-a-task refusal", res)
	}
	// A task-owned conversation: the wire offers it (the reverse link is
	// stamped by SetTaskConversation).
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	task, err := us.CreateTask("t", "d", "todo", nil)
	if err != nil {
		t.Fatal(err)
	}
	taskConv, err := a.CreateConversation(context.Background(), "u1", "task: t", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetTaskConversation(task.ID, taskConv); err != nil {
		t.Fatal(err)
	}
	conv, err := us.GetConversation(taskConv)
	if err != nil {
		t.Fatal(err)
	}
	if conv.OwnerTaskID == nil || *conv.OwnerTaskID != task.ID {
		t.Fatalf("owned conversation owner_task_id = %v, want %s (the reverse link)", conv.OwnerTaskID, task.ID)
	}
	offered = false
	for _, wt := range a.wireTools(us, conv) {
		if wt.Name == "advance_task" {
			offered = true
		}
	}
	if !offered {
		t.Fatal("advance_task not offered to a task-owned conversation")
	}
	res, err = tool.Execute(context.WithValue(context.Background(), ctxConversationKey, taskConv), "u1", []byte(`{"state":"blocked","note":"stuck"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "settles as blocked") {
		t.Fatalf("task-conversation call result = %q, want the recorded report", res)
	}
	// A bad state is refused, not recorded.
	res, _ = tool.Execute(context.WithValue(context.Background(), ctxConversationKey, taskConv), "u1", []byte(`{"state":"canceled"}`))
	if !contains(res, `"done" or "blocked"`) {
		t.Fatalf("bad-state result = %q, want the structured refusal", res)
	}
}
