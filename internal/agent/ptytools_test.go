package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// ptyCall is one scripted pty tool call (the task agent's drive loop).
func ptyCall(name, args string) provider.Result {
	return provider.Result{ToolCalls: []provider.ToolCall{{ID: "c1", Name: name, Arguments: args}}}
}

// TestInteractiveExecSpawnsBackgroundTask is the design's WS2/WS3 contract
// end-to-end: the user's turn calls interactive_exec, which spawns a
// SYSTEM task (owner column, origin stamped) and ENDS the turn
// immediately; the task runs its own agent (the least-loaded OTHER model —
// the user's turn holds its model's slot for the turn's duration) that
// drives the pty session (start → wait → answer the prompt → wait → report
// done); the completion summary lands in the user's conversation; the task
// settle closes the session it started (the task-bound lifetime).
func TestInteractiveExecSpawnsBackgroundTask(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// The user's model (m1 — the default): one tool call (interactive_exec)
	// then the final text. The task lands on m2 (m1's slot is held by the
	// user's turn for its duration — the least-loaded rule). The user
	// provider's delay simulates a real model call's latency: it keeps the
	// user's turn holding its slot across the dispatch's model resolution
	// (an instant fake would let the whole user turn finish — and release
	// its slot — before the dispatch goroutine even starts, which no real
	// model call does).
	userP := &toolCallProvider{results: []provider.Result{
		{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "interactive_exec", Arguments: `{"command":"cat"}`}}},
		{Text: "Done — the command is running in the background."},
	}, delay: 50 * time.Millisecond}
	taskP := &toolCallProvider{results: []provider.Result{
		ptyCall("env_pty_start", `{"command":"cat"}`),
		ptyCall("env_pty_wait", `{"session":"s0","timeout_seconds":2}`),
		ptyCall("env_pty_input", `{"session":"s0","text":"hunter2\n"}`),
		ptyCall("env_pty_wait", `{"session":"s0","timeout_seconds":2}`),
		advanceCall(`{"state":"done","note":"password prompt answered; cat exited 0"}`),
		{Text: "cat ran: it asked for a password, I answered it, and it exited 0."},
	}}
	a.providers["m1"] = userP
	a.providers["m2"] = taskP

	bk := wireContainerBackend(t, a)
	bk.mu.Lock()
	bk.ptyInitialOutput = "Enter password: "
	bk.ptyInitialWaiting = true
	bk.ptyInputEcho = "Welcome!\n"
	bk.ptyAutoExitAfterInput = true
	bk.ptyExitCode = 0
	bk.mu.Unlock()

	origin, err := a.CreateConversation(context.Background(), "u1", "origin", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	// The user's turn: the tool spawns the task and the turn ends.
	_, reply, err := a.Turn(context.Background(), "u1", origin, "please run cat for me, it will ask for things", nil)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !strings.Contains(reply, "running in the background") {
		t.Fatalf("user reply = %q, want the turn's own final text (the task ran in the BACKGROUND)", reply)
	}
	// The user's turn never touched the pty layer directly (interactive_exec
	// only spawns the task — the environment work belongs to the task's
	// env_pty_start; by the time the task has settled, exactly one ensure
	// has happened, its).

	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	var task *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := us.ListTasks()
		if err == nil && len(tasks) == 1 {
			task = &tasks[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if task == nil {
		t.Fatal("no task row created")
	}
	done := waitTaskState(t, a, "u1", task.ID, "done")
	if done.Owner != "system" {
		t.Fatalf("task owner = %q, want system (Refinement B: agent-owned tasks are always system)", done.Owner)
	}
	if !strings.HasPrefix(done.Title, "interactive: cat") {
		t.Fatalf("task title = %q, want the interactive_exec title", done.Title)
	}
	if done.OriginConversationID == nil || *done.OriginConversationID != origin {
		t.Fatalf("task origin = %v, want the user's conversation", done.OriginConversationID)
	}

	// The pty drive: one session, the prompt answered, and the settle-path
	// close (the task-bound lifetime — the session was already exited, the
	// close is the cleanup).
	bk.mu.Lock()
	starts := len(bk.ptyStartCalls)
	inputs := append([]fakePtyInputCall(nil), bk.ptyInputCalls...)
	closes := append([]string(nil), bk.ptyCloseCalls...)
	ensures := bk.ensureCalls
	bk.mu.Unlock()
	if ensures != 1 {
		t.Fatalf("environment ensures = %d, want exactly 1 (the task's env_pty_start auto-ensure)", ensures)
	}
	if starts != 1 || bk.ptyStartCalls[0].Command != "cat" {
		t.Fatalf("pty starts = %+v, want one start of cat", bk.ptyStartCalls)
	}
	if len(inputs) != 1 || inputs[0].Session != "s0" || inputs[0].Text != "hunter2\n" {
		t.Fatalf("pty inputs = %+v, want the prompt answered on s0", inputs)
	}
	if len(closes) != 1 || closes[0] != "s0" {
		t.Fatalf("pty closes = %v, want the task-settle close of s0", closes)
	}

	// The completion summary landed in the user's conversation.
	omsgs, err := us.ListMessages(origin)
	if err != nil {
		t.Fatal(err)
	}
	var summary string
	for _, m := range omsgs {
		if strings.Contains(m.Content, `Task "interactive: cat" is complete:`) {
			summary = m.Content
		}
	}
	if summary == "" {
		t.Fatalf("origin messages = %+v, want the task completion summary", omsgs)
	}
	if !strings.Contains(summary, "password prompt answered") || !strings.Contains(summary, "cat ran") {
		t.Fatalf("summary = %q, want the note + the final reply", summary)
	}
	// The session tracker is empty (the settle path cleared it).
	a.ptySessMu.Lock()
	trackers := len(a.ptySessions)
	a.ptySessMu.Unlock()
	if trackers != 0 {
		t.Fatalf("pty session tracker has %d conversation entries after the task settled, want 0", trackers)
	}
}

// TestPtyToolsWireScoping: the wire offers interactive_exec only in
// non-task conversations and the env_pty_* tools only in task
// conversations; a stray direct call (the model imitating from history) is
// refused, structured, model-readable — either direction.
func TestPtyToolsWireScoping(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	wireNames := func(list []provider.Tool) map[string]bool {
		names := map[string]bool{}
		for _, t := range list {
			names[t.Name] = true
		}
		return names
	}

	// A plain chat conversation.
	chat, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	names := wireNames(a.wireTools(nil, nil))
	if !names["interactive_exec"] || names["env_pty_start"] || names["env_pty_wait"] || names["env_pty_input"] || names["env_pty_close"] {
		t.Fatalf("chat wire = %v, want interactive_exec WITHOUT the pty tools", names)
	}

	// A task-owned conversation (the reverse link, the advance_task test's
	// pattern).
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
	names = wireNames(a.wireTools(us, conv))
	if names["interactive_exec"] || !names["env_pty_start"] || !names["env_pty_wait"] || !names["env_pty_input"] || !names["env_pty_close"] {
		t.Fatalf("task wire = %v, want the pty tools WITHOUT interactive_exec", names)
	}

	// The direct-call refusals (defense in depth behind the wire filter).
	ptyStart, ok := a.tools.Get("env_pty_start")
	if !ok {
		t.Fatal("env_pty_start is not registered")
	}
	res, err := ptyStart.Execute(context.WithValue(context.Background(), ctxConversationKey, chat), "u1", []byte(`{"command":"cat"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "not a task conversation") {
		t.Fatalf("chat-conversation call = %q, want the not-a-task refusal", res)
	}
	ix, ok := a.tools.Get("interactive_exec")
	if !ok {
		t.Fatal("interactive_exec is not registered")
	}
	res, err = ix.Execute(context.WithValue(context.Background(), ctxConversationKey, taskConv), "u1", []byte(`{"command":"cat"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "cannot spawn another task") {
		t.Fatalf("task-conversation call = %q, want the no-nesting refusal", res)
	}
}

// TestPtySessionLifecycle: the task agent's direct session drive against the
// fake backend (start → wait → input → wait → close), the unknown-session
// result, and the tracker (start records, close untracks).
func TestPtySessionLifecycle(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	bk.mu.Lock()
	bk.ptyInitialOutput = "booting...\n"
	bk.mu.Unlock()

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
	ctx := context.WithValue(context.Background(), ctxConversationKey, taskConv)

	start, ok := a.tools.Get("env_pty_start")
	if !ok {
		t.Fatal("env_pty_start is not registered")
	}
	res, err := start.Execute(ctx, "u1", []byte(`{"command":"sleep 5"}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(res, "started pty session s0") || !strings.Contains(res, "booting...") {
		t.Fatalf("start result = %q, want the session id + the first output", res)
	}
	// Tracked under the task conversation.
	a.ptySessMu.Lock()
	_, tracked := a.ptySessions[taskConv]["s0"]
	a.ptySessMu.Unlock()
	if !tracked {
		t.Fatal("the started session is not tracked under the task conversation")
	}

	w, ok := a.tools.Get("env_pty_wait")
	if !ok {
		t.Fatal("env_pty_wait is not registered")
	}
	res, err = w.Execute(ctx, "u1", []byte(`{"session":"s0","timeout_seconds":1}`))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(res, "state: running") {
		t.Fatalf("wait result = %q, want running", res)
	}
	// Unknown session: the structured gone result (the session died with the
	// environment, or the model typed the id wrong).
	res, err = w.Execute(ctx, "u1", []byte(`{"session":"s9"}`))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(res, "it is gone") {
		t.Fatalf("unknown-session result = %q, want the gone result", res)
	}

	in, ok := a.tools.Get("env_pty_input")
	if !ok {
		t.Fatal("env_pty_input is not registered")
	}
	res, err = in.Execute(ctx, "u1", []byte(`{"session":"s0","text":"hi\n"}`))
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if !strings.Contains(res, "sent 3 bytes to session s0") {
		t.Fatalf("input result = %q", res)
	}

	cl, ok := a.tools.Get("env_pty_close")
	if !ok {
		t.Fatal("env_pty_close is not registered")
	}
	res, err = cl.Execute(ctx, "u1", []byte(`{"session":"s0"}`))
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !strings.Contains(res, "session s0 ended (exit code 0)") {
		t.Fatalf("close result = %q", res)
	}
	a.ptySessMu.Lock()
	_, tracked = a.ptySessions[taskConv]["s0"]
	a.ptySessMu.Unlock()
	if tracked {
		t.Fatal("the closed session is still tracked")
	}
	bk.mu.Lock()
	closes := append([]string(nil), bk.ptyCloseCalls...)
	bk.mu.Unlock()
	if len(closes) != 1 || closes[0] != "s0" {
		t.Fatalf("backend closes = %v, want s0", closes)
	}
}

// TestPtyToolsNoBackend: no container backend (an unsupported platform) —
// the structured unavailable result, and the env-not-startable result when
// the ensure fails (both model-facing, never a turn error).
func TestPtyToolsNoBackend(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// No SetContainerBackend at all: unavailable.
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
	ctx := context.WithValue(context.Background(), ctxConversationKey, taskConv)
	start, _ := a.tools.Get("env_pty_start")
	res, err := start.Execute(ctx, "u1", []byte(`{"command":"cat"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res, "unavailable") {
		t.Fatalf("no-backend result = %q, want the unavailable result", res)
	}

	// A backend whose ensure fails: the structured env result (the task
	// agent reports blocked on top of it).
	bk := wireContainerBackend(t, a)
	bk.mu.Lock()
	bk.ensureErr = context.DeadlineExceeded
	bk.mu.Unlock()
	res, err = start.Execute(ctx, "u1", []byte(`{"command":"cat"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res, "could not be started") {
		t.Fatalf("ensure-fail result = %q, want the env-not-startable result", res)
	}
}
