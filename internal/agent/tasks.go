// The minimal task system (the design log's TASKS section; tier 2 of the
// execution model, built on the same per-model slot pools as interactive
// turns).
//
// A task is a user-level unit of work the AGENT works done: it owns a
// normal conversation (regular system prompt, full tool surface) and runs
// through the model's slot pool like any other turn. The P6 row (state +
// the owned conversation + the origin) is the durable record; runtime
// state (the live run contexts) is in-memory by design — a restart resets
// in-progress tasks to todo and works them again.
//
// Spawn is a harness method (v1 has no model-facing tool and no task API —
// the first caller is the memory curation task). A completed task's final
// reply is its completion summary, posted to the origin conversation when
// one was given (the audit trail: what the agent did and learned).
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/utopiafallen/bevin/internal/store"
)

// TaskSpec is the harness's task request.
type TaskSpec struct {
	Title       string // the task's title (the owned conversation is titled "task: <Title>")
	Description string // the unit of work the agent works done
	// OriginConvID is the conversation the task was created from (optional):
	// the completion summary is posted there (the audit trail). It never
	// implies a model (the task model rule).
	OriginConvID string
	// Model is an EXPLICIT model target (the task model rule's override at
	// creation). Empty = ANY AVAILABLE MAIN MODEL, resolved at dispatch
	// (the least-loaded pool — availability is only meaningful then).
	Model string
	// MemStart/MemEnd: the curation task's CLOSED range of memory ids
	// (inclusive; 0 = unset, the non-curation case). Fixed at spawn —
	// memories arriving mid-run wait for the next pass.
	MemStart int64
	MemEnd   int64
}

// taskSystem is the Agent's tier-2 dispatcher: one goroutine per task, each
// waiting on (and holding) a slot in the task's model's pool for exactly the
// duration of its turn — interactive turns and tasks share the pools FIFO.
type taskSystem struct {
	a *Agent

	mu      sync.Mutex
	running map[string]context.CancelFunc // taskID -> its live run's cancel (StopTask)

	// terminal: the model's own outcome report (the advance_task tool),
	// keyed by the task's owned conversation. Set during the run's turn,
	// consumed ONCE by the settling run (last call wins — the model's
	// latest judgment). A turn that errors before settling consumes it
	// too (an intent from a failed run must not leak into a re-run).
	termMu   sync.Mutex
	terminal map[string]terminalIntent
}

func newTaskSystem(a *Agent) *taskSystem {
	return &taskSystem{a: a, running: map[string]context.CancelFunc{}, terminal: map[string]terminalIntent{}}
}

// terminalIntent is what advance_task reports: the state the model picked
// (done | blocked) + its optional note (the reason, posted ahead of the
// completion summary).
type terminalIntent struct {
	state string
	note  string
}

func (ts *taskSystem) setTerminalIntent(convID, state, note string) {
	ts.termMu.Lock()
	defer ts.termMu.Unlock()
	ts.terminal[convID] = terminalIntent{state: state, note: note}
}

// takeTerminalIntent returns the model's outcome report for the task
// conversation (clearing it — it applies to this run only).
func (ts *taskSystem) takeTerminalIntent(convID string) (terminalIntent, bool) {
	ts.termMu.Lock()
	defer ts.termMu.Unlock()
	in, ok := ts.terminal[convID]
	delete(ts.terminal, convID)
	return in, ok
}

// SpawnTask creates the task row and dispatches it (non-blocking: the run
// happens on its own goroutine through the model's slot pool). It returns
// the created task — already todo, possibly already flipped by the time the
// caller reads it back.
//
// The owned conversation is NOT created here: model resolution happens at
// DISPATCH (the task model rule — no explicit target means any available
// main model, which is only knowable when the task actually starts), and the
// conversation is created with the resolved model at that point. A todo
// task is therefore a row in tasks, not yet a conversation.
func (a *Agent) SpawnTask(ctx context.Context, handle string, spec TaskSpec) (*store.Task, error) {
	if spec.Title == "" || spec.Description == "" {
		return nil, fmt.Errorf("a task needs a title and a description")
	}
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	// Validate the origin (the summary's destination — not a model source).
	if spec.OriginConvID != "" {
		if _, err := us.GetConversation(spec.OriginConvID); err != nil {
			return nil, fmt.Errorf("origin conversation: %w", err)
		}
	}
	// An explicit model target must name an available main model (a typo
	// here would surface as a blocked task at dispatch — reject it at
	// spawn instead).
	if spec.Model != "" && !a.modelAvailable(spec.Model) {
		return nil, fmt.Errorf("unknown or unavailable model %q", spec.Model)
	}
	var model *string
	var memStart, memEnd *int64
	if spec.Model != "" {
		m := spec.Model
		model = &m
	}
	if spec.MemStart > 0 && spec.MemEnd >= spec.MemStart {
		s, e := spec.MemStart, spec.MemEnd
		memStart, memEnd = &s, &e
	}
	task, err := us.CreateTaskNew(store.TaskNew{
		Title: spec.Title, Description: spec.Description,
		Model: model, MemStart: memStart, MemEnd: memEnd,
	})
	if err != nil {
		return nil, err
	}
	if spec.OriginConvID != "" {
		if err := us.SetTaskOrigin(task.ID, spec.OriginConvID); err != nil {
			return nil, err
		}
	}
	a.tasks.dispatch(handle, task)
	return task, nil
}

// StopTask cancels a task: a queued (todo) one is dropped before it runs; a
// running (in-progress) one is canceled at its next step boundary. The CAS
// transitions make the cancel safe against the runner's own writes (see
// TransitionTask). Terminal tasks refuse.
func (a *Agent) StopTask(ctx context.Context, handle, taskID string) error {
	us, err := a.userStore(handle)
	if err != nil {
		return err
	}
	// Two passes: a state can flip between the read and the CAS (the
	// runner starting, settling); the retry then handles the new state.
	for range 2 {
		task, err := us.GetTask(taskID)
		if err != nil {
			return err
		}
		switch task.State {
		case "todo":
			if err := us.TransitionTask(taskID, "todo", "canceled"); err == nil {
				return nil
			}
		case "in-progress":
			a.tasks.mu.Lock()
			cancel, ok := a.tasks.running[taskID]
			a.tasks.mu.Unlock()
			if ok {
				cancel() // the run settles to canceled as it unwinds
			}
			if err := us.TransitionTask(taskID, "in-progress", "canceled"); err == nil {
				return nil
			}
		default:
			return fmt.Errorf("task %s is %s (already terminal)", taskID, task.State)
		}
	}
	return fmt.Errorf("task %s: cancel raced a state change — retry", taskID)
}

// dispatch runs one task on its own goroutine (SpawnTask stays non-blocking).
func (ts *taskSystem) dispatch(handle string, task *store.Task) {
	go ts.run(handle, task.ID)
}

// modelAvailable reports whether modelID is one of the user's available
// main models (admin-enabled and not auto-disabled).
func (a *Agent) modelAvailable(modelID string) bool {
	for _, m := range a.Models() {
		if m.ID == modelID {
			return true
		}
	}
	return false
}

// pickAnyMainModel is the task model rule's default: no explicit target →
// ANY available main model (the task is background work; any main model's
// idle slot is fine). The LEAST-LOADED pool wins (busy slots + queued jobs;
// a model with no pool yet is load 0), ties broken by config order. "" when
// no model is available (the caller blocks the task).
func (ts *taskSystem) pickAnyMainModel() string {
	a := ts.a
	models := a.Models()
	if len(models) == 0 {
		return ""
	}
	load := a.sched.Load()
	bestID := ""
	bestLoad := 0
	for _, m := range models {
		l := load[m.ID] // absent pool = no work seen = load 0
		cur := l.Busy + len(l.Waiting)
		if bestID == "" || cur < bestLoad {
			bestID, bestLoad = m.ID, cur
		}
	}
	return bestID
}

// resolveTaskModel applies the task model rule at dispatch: the explicit
// target when the task carries one (the origin never implies a model), else
// any available main model.
func (ts *taskSystem) resolveTaskModel(task *store.Task) string {
	if task.Model != nil && *task.Model != "" {
		return *task.Model
	}
	return ts.pickAnyMainModel()
}

// run is the task's lifecycle: CAS the start, resolve the model + create
// the owned conversation (dispatch time — see the task model rule), run the
// work as a normal agent turn, and settle the terminal state (done /
// blocked / canceled) + post the completion summary to the origin when one
// exists.
func (ts *taskSystem) run(handle, taskID string) {
	a := ts.a
	us, err := a.userStore(handle)
	if err != nil {
		return // the user's store is gone: nothing to run or record
	}
	task, err := us.GetTask(taskID)
	if err != nil {
		return // deleted in the meantime
	}
	if task.State != "todo" {
		return // canceled while queued
	}
	if ok, err := us.TaskDepsDone(taskID); err != nil || !ok {
		return // not eligible yet (v1 tasks have no parents — future-proofing)
	}
	// CAS the start: a StopTask that landed between the re-read and this
	// point already set canceled, and this flip simply loses (the task is
	// gone from todo — it never runs).
	if err := us.TransitionTask(taskID, "todo", "in-progress"); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts.mu.Lock()
	ts.running[taskID] = cancel
	ts.mu.Unlock()
	defer func() {
		ts.mu.Lock()
		delete(ts.running, taskID)
		ts.mu.Unlock()
		cancel()
	}()
	// The owned conversation: kept when a re-run (boot reset) already has
	// one; otherwise created NOW with the dispatch-time resolved model
	// (the task model rule — see resolveTaskModel). A creation failure
	// settles the task blocked (the in-progress→blocked CAS loses cleanly
	// to a racing cancel).
	fail := func() {
		_ = us.TransitionTask(taskID, "in-progress", "blocked")
	}
	convID := ""
	switch {
	case task.ConversationID != nil && *task.ConversationID != "":
		convID = *task.ConversationID
	default:
		modelID := ts.resolveTaskModel(task)
		if modelID == "" {
			fail() // no available model: it can't run
			return
		}
		id, err := a.CreateConversation(ctx, handle, "task: "+task.Title, modelID, "")
		if err != nil {
			fail()
			return
		}
		if err := us.SetTaskConversation(taskID, id); err != nil {
			fail()
			return
		}
		convID = id
	}
	_, reply, terr := a.Turn(ctx, handle, convID, taskWorkMessage(task))
	// The model's own outcome report (advance_task) — consumed once here
	// (even on the error/cancel paths, so a failed run's report never
	// leaks into a later re-run).
	intent, hasIntent := ts.takeTerminalIntent(convID)

	switch {
	case ctx.Err() != nil:
		// Canceled (StopTask): whatever ran stays in the task
		// conversation (the audit trail); no summary is posted.
		_ = us.TransitionTask(taskID, "in-progress", "canceled")
	case terr != nil:
		// Infra/model failure: the task is blocked, and its card status
		// area (the conversation's last message) says why. An infra
		// failure beats any report the model managed to make (it is
		// objective).
		_, _ = us.AppendMessage(convID, roleAssistant, "Task failed: "+terr.Error(), "", "", nil)
		_ = us.TransitionTask(taskID, "in-progress", "blocked")
	default:
		// The terminal state: the model's report when it made one, the
		// harness default (done) otherwise.
		state := "done"
		if hasIntent {
			state = intent.state
		}
		_ = us.TransitionTask(taskID, "in-progress", state)
		// The completion summary (audit trail + "what the agent learned"):
		// the task's final reply (its advance_task note, when given,
		// posted ahead of it), posted to the origin conversation when one
		// was given (a deleted origin loses it silently — the task
		// conversation keeps the reply either way). A blocked task posts
		// too — it is exactly the outcome the origin should see.
		if origin := task.OriginConversationID; origin != nil && *origin != "" {
			if _, err := us.GetConversation(*origin); err == nil {
				summary := reply
				if intent.note != "" {
					summary = intent.note + "\n\n" + reply
				}
				framing := "Task \"" + task.Title + "\" is complete:"
				if state == "blocked" {
					framing = "Task \"" + task.Title + "\" is blocked:"
				}
				_, _ = us.AppendMessage(*origin, roleAssistant, framing+"\n\n"+summary, "", "", nil)
			}
		}
	}
}

// taskWorkMessage frames the task's first turn: the task is a unit worked
// done by the agent — the normal system prompt and tool surface apply, only
// the framing (and the completion-summary contract) is task-specific.
func taskWorkMessage(task *store.Task) string {
	var b strings.Builder
	b.WriteString("You are working a background task to completion.\n\n")
	b.WriteString("Task: " + task.Title + "\n" + task.Description + "\n\n")
	b.WriteString("Work it with your normal tools. When you know how it ended, call advance_task to report it")
	b.WriteString(" (state \"done\", or \"blocked\" with a note on what is missing or stuck) — the task settles on that report. ")
	b.WriteString("Your final reply is recorded as the task's completion summary")
	if task.OriginConversationID != nil && *task.OriginConversationID != "" {
		b.WriteString(" and posted to the conversation the task was created from, for the user's audit trail")
	}
	b.WriteString(" — make it a concise summary of what you did, what you found or learned, and anything the user should know.")
	return b.String()
}

// resetStaleOnBoot (called from NewAgent): a task that was in-progress when
// the process died or restarted cannot resume mid-turn — runtime state is
// in-memory by design — so it resets to todo and is worked again (re-runs
// are safe: the agent's work supersede-merges on top of its earlier half).
// Terminal states are untouched.
func (ts *taskSystem) resetStaleOnBoot() {
	entries, err := os.ReadDir(filepath.Join(ts.a.dataDir, "users"))
	if err != nil {
		return // no users dir yet (a fresh install)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		handle := e.Name()
		us, err := ts.a.userStore(handle)
		if err != nil {
			continue
		}
		tasks, err := us.ListTasks()
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.State != "in-progress" {
				continue
			}
			if err := us.SetTaskState(t.ID, "todo"); err != nil {
				continue
			}
			if fresh, err := us.GetTask(t.ID); err == nil {
				ts.dispatch(handle, fresh)
			}
		}
	}
}
