package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateV1AddsTaskOrigin replicates a user db written by a v1 build
// (the tasks table WITHOUT origin_conversation_id, version 1 recorded) and
// verifies the numbered migrations add the column (the v4 step), keep the
// data, and leave the column usable — then that a re-open is idempotent.
func TestMigrateV1AddsTaskOrigin(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dir, "bevin.db")))
	if err != nil {
		t.Fatal(err)
	}
	v1 := `
CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE conversations (
	id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
	last_activity_at TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'active',
	frozen_system_prompt TEXT NOT NULL DEFAULT '', prompt_input_versions TEXT NOT NULL DEFAULT '',
	owner_task_id TEXT, active_model_id TEXT NOT NULL, reasoning_effort TEXT NOT NULL DEFAULT ''
);
CREATE TABLE tasks (
	id TEXT PRIMARY KEY, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'todo', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
	conversation_id TEXT
);
INSERT INTO schema_version (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z');
INSERT INTO conversations (id, title, created_at, last_activity_at, active_model_id)
	VALUES ('c1', 'old conv', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'm1');
INSERT INTO tasks (id, title, description, state, created_at, updated_at, conversation_id)
	VALUES ('t1', 'old task', 'a v1 task', 'todo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'c1');
`
	if _, err := db.Exec(v1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Open with the current build: the v1->v2 step must run.
	us, err := OpenUser(dir)
	if err != nil {
		t.Fatalf("OpenUser (v1 file): %v", err)
	}
	tasks, err := us.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t1" || tasks[0].Title != "old task" || tasks[0].Description != "a v1 task" {
		t.Fatalf("v1 data did not survive the migration: %+v", tasks)
	}
	if tasks[0].OriginConversationID != nil {
		t.Fatalf("migrated task origin = %v, want NULL", *tasks[0].OriginConversationID)
	}
	// The new column is usable (an FK-valid reference round-trips).
	if err := us.SetTaskOrigin("t1", "c1"); err != nil {
		t.Fatalf("SetTaskOrigin: %v", err)
	}
	tk, err := us.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.OriginConversationID == nil || *tk.OriginConversationID != "c1" {
		t.Fatalf("origin round-trip failed: %+v", tk.OriginConversationID)
	}
	// A re-open (now a v2 file) is idempotent.
	if err := us.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUser(dir); err != nil {
		t.Fatalf("re-open (v2 file): %v", err)
	}
}

// TestTaskCanceledStateAndTransition pins the code-only "canceled" state:
// it is valid, TransitionTask moves a task into it, and a second transition
// from a stale state loses (the CAS the runner and StopTask rely on).
func TestTaskCanceledStateAndTransition(t *testing.T) {
	s := openTestStore(t)
	task, err := s.CreateTask("cancel me", "the harness abandoned it", "todo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !TaskStateValid("canceled") {
		t.Fatal("canceled is not a valid state")
	}
	// A stale transition (from a state the task is not in) must fail...
	if err := s.TransitionTask(task.ID, "in-progress", "canceled"); err == nil {
		t.Fatal("transition from a stale state succeeded")
	}
	// ...while the real one succeeds.
	if err := s.TransitionTask(task.ID, "todo", "in-progress"); err != nil {
		t.Fatalf("todo->in-progress: %v", err)
	}
	if err := s.TransitionTask(task.ID, "in-progress", "canceled"); err != nil {
		t.Fatalf("in-progress->canceled: %v", err)
	}
	tk, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != "canceled" {
		t.Fatalf("state = %q, want canceled", tk.State)
	}
	// A terminal state is a dead end: no further transition applies.
	if err := s.TransitionTask(task.ID, "canceled", "todo"); err == nil {
		t.Fatal("transition out of a terminal state succeeded")
	}
}
