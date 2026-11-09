package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Task is a P6 row (a user-level kanban item; owns one conversation).
type Task struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	State          string   `json:"state"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	ConversationID *string  `json:"conversation_id,omitempty"`
	Dependencies   []string `json:"depends_on,omitempty"`
}

var taskStates = map[string]bool{"todo": true, "in-progress": true, "done": true, "blocked": true}

// TaskStateValid reports whether s is a known task state.
func TaskStateValid(s string) bool { return taskStates[s] }

const taskCols = `id, title, description, state, created_at, updated_at, conversation_id`

func scanTask(rs interface{ Scan(...any) error }, t *Task) error {
	return rs.Scan(&t.ID, &t.Title, &t.Description, &t.State, &t.CreatedAt, &t.UpdatedAt, &t.ConversationID)
}

func (s *UserStore) createTaskTx(tx *sql.Tx, title, description, state string, deps []string) (*Task, error) {
	if !taskStates[state] {
		return nil, fmt.Errorf("invalid task state %q", state)
	}
	id := newID()
	now := nowUTC()
	if _, err := tx.Exec(`INSERT INTO tasks (id, title, description, state, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		id, title, description, state, now, now); err != nil {
		return nil, err
	}
	for _, p := range deps {
		if err := s.addDependencyTx(tx, id, p); err != nil {
			return nil, err
		}
	}
	return &Task{ID: id, Title: title, Description: description, State: state, CreatedAt: now, UpdatedAt: now, Dependencies: deps}, nil
}

// CreateTask creates a task with (0..N) parent dependencies. A cycle-forming
// dependency is rejected.
func (s *UserStore) CreateTask(title, description, state string, deps []string) (*Task, error) {
	if state == "" {
		state = "todo"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := s.createTaskTx(tx, title, description, state, deps)
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

// SetTaskConversation links the task to the conversation it owns.
func (s *UserStore) SetTaskConversation(id, convID string) error {
	var v *string
	if convID != "" {
		v = &convID
	}
	_, err := s.db.Exec(`UPDATE tasks SET conversation_id = ?, updated_at = ? WHERE id = ?`, v, nowUTC(), id)
	return err
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