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
// task's owned conversation (which takes the origin's model); the task
// settles done and the completion summary (its final reply) is posted to
// the origin — the audit trail.
func TestTaskSpawnRunsAndCompletes(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// m2 only: the task must reach the model through the ORIGIN's model
	// (m2), not the user default (m1, which has no provider here).
	fp := &fakeProvider{replies: []string{"I reviewed the day and learned that the user ships on Fridays."}}
	a.providers["m2"] = fp

	origin, err := a.CreateConversation(context.Background(), "u1", "origin", "m2", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "nightly review", Description: "Review the day's conversations.", OriginConvID: origin,
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
	// The owned conversation took the origin's model.
	conv, err := us.GetConversation(*done.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ActiveModelID != "m2" {
		t.Fatalf("task conversation model = %q, want the origin's m2", conv.ActiveModelID)
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

// TestTaskNoOriginStaysLocal: a harness task with no origin runs on the
// user's default model, and its summary stays in the task conversation
// (nothing is posted anywhere else).
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
// turn, a spawned task is accepted (in-progress — its turn is queued in the
// pool) but not yet at the model. StopTask cancels it; when the slot frees,
// the interactive turn finishes and the task never runs (no provider call,
// no work message).
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
		a.Turn(context.Background(), "u1", conv, "a long interactive turn")
	}()
	waitFor(t, func() bool { return gp.callCount() >= 1 }, "the interactive turn to hold m1's only slot")

	task, err := a.SpawnTask(context.Background(), "u1", TaskSpec{
		Title: "queued behind the busy slot", Description: "never reaches the model",
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

// TestTaskSpawnValidation: spawn refuses empty title/description and an
// unknown origin conversation.
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
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
