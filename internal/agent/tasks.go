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
// the first caller is the learning re-design). A completed task's final
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
	// the task conversation takes its model, and the completion summary is
	// posted there. Empty = a harness task with no origin (the summary
	// stays in the task conversation; the user's default model applies).
	OriginConvID string
}

// taskSystem is the Agent's tier-2 dispatcher: one goroutine per task, each
// waiting on (and holding) a slot in the task's model's pool for exactly the
// duration of its turn — interactive turns and tasks share the pools FIFO.
type taskSystem struct {
	a *Agent

	mu      sync.Mutex
	running map[string]context.CancelFunc // taskID -> its live run's cancel (StopTask)
}

func newTaskSystem(a *Agent) *taskSystem {
	return &taskSystem{a: a, running: map[string]context.CancelFunc{}}
}

// SpawnTask creates a task + its owned conversation and dispatches it
// (non-blocking: the run happens on its own goroutine through the model's
// slot pool). It returns the created task — already todo, possibly already
// flipped by the time the caller reads it back.
func (a *Agent) SpawnTask(ctx context.Context, handle string, spec TaskSpec) (*store.Task, error) {
	if spec.Title == "" || spec.Description == "" {
		return nil, fmt.Errorf("a task needs a title and a description")
	}
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	// Validate the origin (and take its model for the task conversation).
	originModel := ""
	if spec.OriginConvID != "" {
		conv, err := us.GetConversation(spec.OriginConvID)
		if err != nil {
			return nil, fmt.Errorf("origin conversation: %w", err)
		}
		originModel = conv.ActiveModelID
	}
	task, err := us.CreateTask(spec.Title, spec.Description, "todo", nil)
	if err != nil {
		return nil, err
	}
	convID, err := a.CreateConversation(ctx, handle, "task: "+spec.Title, originModel, "")
	if err != nil {
		_ = us.SetTaskState(task.ID, "blocked") // no owned conversation: it can never run
		return nil, fmt.Errorf("create the task conversation: %w", err)
	}
	if err := us.SetTaskConversation(task.ID, convID); err != nil {
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

// run is the task's lifecycle: wait for a slot in the task's model's pool,
// run the work as a normal agent turn in the owned conversation, and settle
// the terminal state (done / blocked / canceled) + post the completion
// summary to the origin when one exists.
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
	if task.ConversationID == nil || *task.ConversationID == "" {
		_ = us.SetTaskState(taskID, "blocked") // no owned conversation: it can't run
		return
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

	convID := *task.ConversationID
	_, reply, terr := a.Turn(ctx, handle, convID, taskWorkMessage(task))

	switch {
	case ctx.Err() != nil:
		// Canceled (StopTask): whatever ran stays in the task
		// conversation (the audit trail); no summary is posted.
		_ = us.TransitionTask(taskID, "in-progress", "canceled")
	case terr != nil:
		// Infra/model failure: the task is blocked, and its card status
		// area (the conversation's last message) says why.
		_, _ = us.AppendMessage(convID, roleAssistant, "Task failed: "+terr.Error(), "", "", nil)
		_ = us.TransitionTask(taskID, "in-progress", "blocked")
	default:
		_ = us.TransitionTask(taskID, "in-progress", "done")
		// The completion summary (audit trail + "what the agent learned"):
		// the task's final reply, posted to the origin conversation when
		// one was given (a deleted origin loses it silently — the task
		// conversation keeps the reply either way).
		if origin := task.OriginConversationID; origin != nil && *origin != "" {
			if _, err := us.GetConversation(*origin); err == nil {
				_, _ = us.AppendMessage(*origin, roleAssistant,
					"Task \""+task.Title+"\" is complete:\n\n"+reply, "", "", nil)
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
	b.WriteString("Work it with your normal tools. Your final reply is recorded as the task's completion summary")
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
