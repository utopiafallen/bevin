package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Task is a P6 row (a user-level kanban item; owns one conversation).
type Task struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	// Owner is the task-ownership axis (v9; the design log's Refinement
	// B): "user" or "system". ALWAYS set explicitly at creation; the
	// user's board shows user-owned tasks only (system hidden). Every
	// v1 task is system (the agent's only task-creation tool is
	// interactive_exec; there is no user task-creation path yet).
	Owner     string `json:"owner"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// ConversationID is the task's owned conversation. A todo task may not
	// have one yet: the conversation is created at DISPATCH, with the
	// resolved model (the task model rule — no explicit target means any
	// available main model, which is only knowable then).
	ConversationID *string `json:"conversation_id,omitempty"`
	// OriginConversationID is the conversation the task was created from
	// (v2): its completion summary is posted there. NULL = no origin. The
	// origin never implies a model (the task model rule).
	OriginConversationID *string `json:"origin_conversation_id,omitempty"`
	// Model is an EXPLICIT model target (nullable): the task runs on it.
	// NULL = any available main model, resolved at dispatch (least-loaded
	// pool). Updatable while the task is todo (SetTaskModel) — an override
	// made later applies at the next dispatch.
	Model *string `json:"model,omitempty"`
	// MemoryStart/MemoryEnd: the curation task's CLOSED range of memory
	// ids (fixed at spawn; NULL on non-curation tasks). The watermark is
	// the max MemoryEnd of DONE curation tasks.
	MemoryStart  *int64   `json:"memory_start,omitempty"`
	MemoryEnd    *int64   `json:"memory_end,omitempty"`
	Dependencies []string `json:"depends_on,omitempty"`
}

// The task states. "canceled" (code-only; the columns are unconstrained
// TEXT) is a terminal state set by StopTask — a task the user/harness
// abandoned before it completed. done/blocked/canceled are terminal: no
// transitions out of them (see TransitionTask).
var taskStates = map[string]bool{"todo": true, "in-progress": true, "done": true, "blocked": true, "canceled": true}

var taskTerminal = map[string]bool{"done": true, "blocked": true, "canceled": true}

// TaskStateValid reports whether s is a known task state.
func TaskStateValid(s string) bool { return taskStates[s] }

const taskCols = `id, title, description, state, created_at, updated_at, conversation_id, origin_conversation_id, model, memory_start, memory_end, owner`

func scanTask(rs interface{ Scan(...any) error }, t *Task) error {
	return rs.Scan(&t.ID, &t.Title, &t.Description, &t.State, &t.CreatedAt, &t.UpdatedAt, &t.ConversationID, &t.OriginConversationID, &t.Model, &t.MemoryStart, &t.MemoryEnd, &t.Owner)
}

// taskOwners are the valid values of tasks.owner (Refinement B).
var taskOwners = map[string]bool{"user": true, "system": true}

func (s *UserStore) createTaskTx(tx *sql.Tx, title, description, state, owner string, deps []string, model *string, memStart, memEnd *int64) (*Task, error) {
	if !taskStates[state] {
		return nil, fmt.Errorf("invalid task state %q", state)
	}
	if !taskOwners[owner] {
		return nil, fmt.Errorf("invalid task owner %q (user|system)", owner)
	}
	id := newID()
	now := nowUTC()
	if _, err := tx.Exec(`INSERT INTO tasks (id, title, description, state, created_at, updated_at, model, memory_start, memory_end, owner) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, title, description, state, now, now, model, memStart, memEnd, owner); err != nil {
		return nil, err
	}
	for _, p := range deps {
		if err := s.addDependencyTx(tx, id, p); err != nil {
			return nil, err
		}
	}
	return &Task{ID: id, Title: title, Description: description, State: state, Owner: owner, CreatedAt: now, UpdatedAt: now, Model: model, MemoryStart: memStart, MemoryEnd: memEnd, Dependencies: deps}, nil
}

// CreateTask creates a task with (0..N) parent dependencies. A cycle-forming
// dependency is rejected. (The plain creation path — no model target, no
// curation range, no explicit owner: system-owned, the task model rule's
// "any available main model" applies.)
func (s *UserStore) CreateTask(title, description, state string, deps []string) (*Task, error) {
	return s.CreateTaskNew(TaskNew{Title: title, Description: description, State: state, Deps: deps, Owner: "system"})
}

// TaskNew is the full creation request (CreateTask's zero-valued
// convenience covers the plain case).
type TaskNew struct {
	Title       string
	Description string
	State       string // "" = todo
	// Owner is the task-ownership axis (Refinement B: user|system).
	// "" = system (the v1 default — the agent's interactive_exec is the
	// only task-creation tool, and agent-owned tasks are always system).
	// Always validated; an invalid value is a creation error.
	Owner    string
	Deps     []string
	Model    *string // explicit model target (nil = any available main model)
	MemStart *int64  // curation range, inclusive (nil on non-curation tasks)
	MemEnd   *int64
}

// CreateTaskNew creates a task with an optional explicit model target and
// curation range. A cycle-forming dependency is rejected.
func (s *UserStore) CreateTaskNew(n TaskNew) (*Task, error) {
	if n.State == "" {
		n.State = "todo"
	}
	if n.Owner == "" {
		n.Owner = "system"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := s.createTaskTx(tx, n.Title, n.Description, n.State, n.Owner, n.Deps, n.Model, n.MemStart, n.MemEnd)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *UserStore) GetTask(id string) (*Task, error) {
	t := &Task{}
	err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id = ?`, id), t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Dependencies, err = s.GetTaskDependencies(id)
	return t, err
}

func (s *UserStore) ListTasks() ([]Task, error) {
	rows, err := s.db.Query(`SELECT ` + taskCols + ` FROM tasks ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := scanTask(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTaskState changes a task's state. (The state machine + dependency gate
// are enforced by the scheduler; this is the durable write.)
func (s *UserStore) SetTaskState(id, state string) error {
	if !taskStates[state] {
		return fmt.Errorf("invalid task state %q", state)
	}
	res, err := s.db.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`, state, nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetTaskConversation links the task to the conversation it owns — and
// stamps the conversation's owner_task_id, the reverse of the 1:1 link
// (the conversation is KNOWN to be this task's; advance_task's scoping and
// the per-turn wire filter key on it). A clear un-stamps only this task's
// own conversation.
func (s *UserStore) SetTaskConversation(id, convID string) error {
	var v *string
	if convID != "" {
		v = &convID
	}
	var oldConv string
	if err := s.db.QueryRow(`SELECT COALESCE(conversation_id, '') FROM tasks WHERE id = ?`, id).Scan(&oldConv); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE tasks SET conversation_id = ?, updated_at = ? WHERE id = ?`, v, nowUTC(), id); err != nil {
		return err
	}
	if convID != "" {
		if _, err := s.db.Exec(`UPDATE conversations SET owner_task_id = ? WHERE id = ?`, id, convID); err != nil {
			return err
		}
	} else if oldConv != "" {
		if _, err := s.db.Exec(`UPDATE conversations SET owner_task_id = NULL WHERE id = ? AND owner_task_id = ?`, oldConv, id); err != nil {
			return err
		}
	}
	return nil
}

// SetTaskOrigin records the conversation a task was created from (the
// completion summary's destination). An empty convID clears it.
func (s *UserStore) SetTaskOrigin(id, convID string) error {
	var v *string
	if convID != "" {
		v = &convID
	}
	_, err := s.db.Exec(`UPDATE tasks SET origin_conversation_id = ?, updated_at = ? WHERE id = ?`, v, nowUTC(), id)
	return err
}

// SetTaskModel sets the task's EXPLICIT model target (the task model rule's
// override — "during its lifetime"): only while the task is todo, since the
// running turn already holds its model (an override made later applies at
// the next dispatch, e.g. after a boot reset). An empty model clears the
// target (back to any available main model).
func (s *UserStore) SetTaskModel(id, model string) error {
	var v *string
	if model != "" {
		v = &model
	}
	res, err := s.db.Exec(`UPDATE tasks SET model = ?, updated_at = ? WHERE id = ? AND state = 'todo'`, v, nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CurationWatermark returns the highest memory id a DONE curation task
// covered (0 when none has completed): the anchor of the growth trigger.
// Blocked/canceled runs do NOT advance it (their range is retried).
func (s *UserStore) CurationWatermark(title string) (int64, error) {
	var w int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(memory_end), 0) FROM tasks WHERE title = ? AND state = 'done' AND memory_end IS NOT NULL`, title).Scan(&w)
	return w, err
}

// HasActiveCuration reports whether a curation task is already queued or
// running (the spawn guard: one curation at a time; a BLOCKED one is
// terminal, so the next growth crossing spawns a fresh attempt).
func (s *UserStore) HasActiveCuration(title string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE title = ? AND state IN ('todo', 'in-progress')`, title).Scan(&n)
	return n > 0, err
}

// TransitionTask is the compare-and-swap state change: it flips the task
// from exactly `from` to `to`, and errors when the state moved in the
// meantime or when `from` is terminal (no transitions out of done/blocked/
// canceled). The task runner's transitions
// (todo→in-progress→done/blocked/canceled) use it so a StopTask racing the
// runner can never lose to a stale write.
func (s *UserStore) TransitionTask(id, from, to string) error {
	if !taskStates[from] || !taskStates[to] {
		return fmt.Errorf("invalid task state in transition %q -> %q", from, to)
	}
	if taskTerminal[from] {
		return fmt.Errorf("task state %q is terminal (no transitions out of it)", from)
	}
	res, err := s.db.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, to, nowUTC(), id, from)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	switch {
	case n == 0:
		return ErrNotFound
	}
	return nil
}

// SetTaskDependencies replaces a task's parent dependencies, rejecting any
// cycle-forming edge.
func (s *UserStore) SetTaskDependencies(id string, deps []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, id).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, nowUTC(), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM task_dependencies WHERE task_id = ?`, id); err != nil {
		return err
	}
	for _, p := range deps {
		if err := s.addDependencyTx(tx, id, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *UserStore) GetTaskDependencies(id string) ([]string, error) {
	rows, err := s.db.Query(`SELECT parent_id FROM task_dependencies WHERE task_id = ? ORDER BY parent_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		out = append(out, pid)
	}
	return out, rows.Err()
}

// TaskDepsDone reports whether all of a task's parents are done (i.e. the task
// is eligible to run on the dependency axis).
func (s *UserStore) TaskDepsDone(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM task_dependencies d JOIN tasks t ON t.id = d.parent_id
		WHERE d.task_id = ? AND t.state != 'done'`, id).Scan(&n)
	return n == 0, err
}

// addDependencyTx inserts a dependency edge in tx, rejecting cycles.
func (s *UserStore) addDependencyTx(tx *sql.Tx, taskID, parentID string) error {
	if taskID == parentID {
		return fmt.Errorf("a task cannot depend on itself")
	}
	// Reject if parentID isn't a real task.
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, parentID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("parent task %q not found", parentID)
	}
	// Cycle: taskID reachable from parentID via existing depends-on edges?
	if cycle, err := s.cycleReachableTx(tx, parentID, taskID); err != nil {
		return err
	} else if cycle {
		return fmt.Errorf("dependency would create a cycle: %s -> %s", taskID, parentID)
	}
	_, err := tx.Exec(`INSERT INTO task_dependencies (task_id, parent_id) VALUES (?,?)`, taskID, parentID)
	return err
}

// cycleReachableTx reports whether target is reachable from start by following
// depends-on (parent) edges within tx.
func (s *UserStore) cycleReachableTx(tx *sql.Tx, start, target string) (bool, error) {
	if start == target {
		return true, nil
	}
	visited := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		rows, err := tx.Query(`SELECT parent_id FROM task_dependencies WHERE task_id = ?`, cur)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var pid string
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return false, err
			}
			if pid == target {
				rows.Close()
				return true, nil
			}
			queue = append(queue, pid)
		}
		rows.Close()
	}
	return false, nil
}
