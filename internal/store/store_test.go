package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/prompt"
)

func openTestStore(t *testing.T) *UserStore {
	t.Helper()
	s, err := OpenUser(t.TempDir())
	if err != nil {
		t.Fatalf("OpenUser: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestConversationAndMessages(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("hello", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "active" || c.ActiveModelID != "m1" {
		t.Fatalf("conv = %+v", c)
	}

	m1, err := s.AppendMessage(c.ID, "user", "what is a quasar?", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Seq != 0 {
		t.Fatalf("seq = %d", m1.Seq)
	}
	m2, err := s.AppendMessage(c.ID, "assistant", "a quasar is a luminous active galactic nucleus", ptrStr("m1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Seq != 1 || *m2.ModelID != "m1" {
		t.Fatalf("m2 = %+v", m2)
	}

	msgs, err := s.ListMessages(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %d", len(msgs))
	}

	// FTS5 search over conversation history.
	hits, err := s.SearchMessages("galactic", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != m2.ID {
		t.Fatalf("search = %+v", hits)
	}

	if err := s.SetConversationModel(c.ID, "m2"); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.GetConversation(c.ID)
	if c2.ActiveModelID != "m2" {
		t.Fatalf("model = %q", c2.ActiveModelID)
	}
}

func TestConcurrentWritersNoBusy(t *testing.T) {
	// Two concurrent turns for one user share a single bevin.db. Without a
	// per-connection busy_timeout the second writer fails with SQLITE_BUSY
	// (seen in e2e: the 2nd of 2 parallel streaming turns died on persisting
	// its reply). Every goroutine gets its own conversation (like distinct
	// live turns) and appends a message; none may error.
	s := openTestStore(t)
	const writers = 4
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		go func(i int) {
			<-start
			c, err := s.CreateConversation("conv", "m1")
			if err != nil {
				errs[i] = err
				return
			}
			if _, err := s.AppendMessage(c.ID, "user", "concurrent write test", nil, nil); err != nil {
				errs[i] = err
			}
		}(i)
	}
	close(start)
	for i := 0; i < writers; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent writer %d: %v (want no SQLITE_BUSY)", i, errs[i])
		}
	}
}

func TestConcurrentBurstShape(t *testing.T) {
	// The full e2e burst shape: at t=0 three turns each do
	// CreateConversation + SetFrozenSystemPrompt + AppendMessage(user)
	// concurrently (a write storm); at t≈delay each appends its reply
	// concurrently. Pre-fix (deferred txs) this failed the reply/user appends
	// with SQLITE_LOCKED(517): a deferred transaction reads first (MAX(seq)),
	// takes a read snapshot, then fails to UPGRADE to a writer while another
	// connection is writing — an error busy_timeout does not retry. The DB is
	// seeded with a handful of conversations + messages first so the FTS
	// triggers and indexes are non-trivial, as in a real data dir.
	s := openTestStore(t)
	for i := 0; i < 8; i++ {
		c, err := s.CreateConversation("seed", "m1")
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 2; j++ {
			if _, err := s.AppendMessage(c.ID, "user", "seed message", nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendMessage(c.ID, "assistant", "seed reply", ptrStr("m1"), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	const turns = 3
	for round := 0; round < 10; round++ {
		// Creation phase: the t=0 write storm.
		start := make(chan struct{})
		ids := make([]string, turns)
		errs := make([]error, turns)
		done := make(chan struct{}, turns)
		for i := 0; i < turns; i++ {
			go func(i int) {
				defer func() { done <- struct{}{} }()
				<-start
				c, err := s.CreateConversation("burst", "m1")
				if err != nil {
					errs[i] = fmt.Errorf("create: %w", err)
					return
				}
				ids[i] = c.ID
				if err := s.SetFrozenSystemPrompt(c.ID, "system prompt text"); err != nil {
					errs[i] = fmt.Errorf("frozen: %w", err)
					return
				}
				if _, err := s.AppendMessage(c.ID, "user", "ping", nil, nil); err != nil {
					errs[i] = fmt.Errorf("user append: %w", err)
					return
				}
			}(i)
		}
		close(start)
		for i := 0; i < turns; i++ {
			<-done
		}
		for i := 0; i < turns; i++ {
			if errs[i] != nil {
				t.Fatalf("round %d turn %d (creation phase): %v", round, i, errs[i])
			}
		}
		// The reply phase: all turns finish their model call at once.
		time.Sleep(200 * time.Millisecond)
		errs2 := make([]error, turns)
		start2 := make(chan struct{})
		done2 := make(chan struct{}, turns)
		for i := 0; i < turns; i++ {
			go func(i int) {
				defer func() { done2 <- struct{}{} }()
				<-start2
				_, err := s.AppendMessage(ids[i], "assistant", "fake reply text", ptrStr("m1"), nil)
				errs2[i] = err
			}(i)
		}
		close(start2)
		for i := 0; i < turns; i++ {
			<-done2
		}
		for i := 0; i < turns; i++ {
			if errs2[i] != nil {
				t.Fatalf("round %d turn %d (reply phase): %v", round, i, errs2[i])
			}
		}
	}
}

func TestConcurrentReplyAppends(t *testing.T) {
	// The e2e shape: several conversations each already hold a user message
	// (so the FTS triggers + indexes are warm), then all reply appends land
	// in the same instant (as when two parallel turns finish their model call
	// together). This is what produced SQLITE_LOCKED(517) in the browser e2e.
	s := openTestStore(t)
	const writers = 3
	ids := make([]string, writers)
	for i := 0; i < writers; i++ {
		c, err := s.CreateConversation("conv", "m1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendMessage(c.ID, "user", "existing user message", nil, nil); err != nil {
			t.Fatal(err)
		}
		ids[i] = c.ID
	}
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		go func(i int) {
			<-start
			_, err := s.AppendMessage(ids[i], "assistant", "the model reply text", ptrStr("m1"), nil)
			errs[i] = err
		}(i)
	}
	close(start)
	for i := 0; i < writers; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent reply append %d: %v", i, errs[i])
		}
	}
}

func TestMemoryFTSAndSupersession(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateMemory("diet", "user prefers a vegan diet", nil, 1.0, "", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	m2, err := s.CreateMemory("diet", "user switched to a flexitarian diet", nil, 1.0, "", []float32{0, 1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeMemory(1, m2.ID); err != nil {
		t.Fatal(err)
	}
	active, err := s.ListActiveMemories()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != m2.ID {
		t.Fatalf("active = %+v", active)
	}

	// FTS5 search only surfaces active memories.
	hits, err := s.SearchMemories("diet", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != m2.ID {
		t.Fatalf("fts = %+v", hits)
	}
}

func TestRetrieverCosineRanking(t *testing.T) {
	s := openTestStore(t)
	r := NewMemoryRetriever(s)
	if _, err := s.CreateMemory("travel", "user lives in Lisbon", nil, 1.0, "", []float32{0.9, 0.1, 0.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMemory("travel", "user prefers trains over flying", nil, 1.0, "", []float32{0.0, 0.1, 0.9}); err != nil {
		t.Fatal(err)
	}
	recs, err := r.RecallMemories(context.Background(), "lisbon", []float32{0.85, 0.2, 0.0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("no recalls")
	}
	if recs[0].Memory.Statement != "user lives in Lisbon" {
		t.Fatalf("top recall = %+v", recs[0].Memory)
	}
}

func TestTaskDependenciesAcyclic(t *testing.T) {
	s := openTestStore(t)
	a, err := s.CreateTask("a", "", "todo", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTask("b", "", "todo", []string{a.ID}) // b depends on a
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateTask("c", "", "todo", []string{b.ID}) // c depends on b
	if err != nil {
		t.Fatal(err)
	}
	// a -> c would close the cycle (c -> b -> a).
	if err := s.SetTaskDependencies(a.ID, []string{c.ID}); err == nil {
		t.Fatal("expected cycle error")
	}
	// a's deps must be unchanged (tx rolled back).
	deps, _ := s.GetTaskDependencies(a.ID)
	if len(deps) != 0 {
		t.Fatalf("a deps after rollback = %v", deps)
	}

	// Eligibility: b gated until a is done.
	if ok, _ := s.TaskDepsDone(b.ID); ok {
		t.Fatal("b should not be eligible (a not done)")
	}
	if err := s.SetTaskState(a.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.TaskDepsDone(b.ID); !ok {
		t.Fatal("b should be eligible now (a done)")
	}
}

func TestTaskSelfDependencyRejected(t *testing.T) {
	s := openTestStore(t)
	a, err := s.CreateTask("a", "", "todo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskDependencies(a.ID, []string{a.ID}); err == nil {
		t.Fatal("expected self-dependency to be rejected")
	}
	// dependency on a non-existent task is rejected.
	if _, err := s.CreateTask("b", "", "todo", []string{"nope"}); err == nil {
		t.Fatal("expected missing-parent error")
	}
}

func TestWorkspaceAndFiles(t *testing.T) {
	s := openTestStore(t)
	ws, err := s.CreateWorkspace("work", "my project", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddWorkspaceFile(ws.ID, "notes.md", "notes.md", "text/markdown", 123, "abc", "user", "doc"); err != nil {
		t.Fatal(err)
	}
	files, err := s.ListWorkspaceFiles(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Kind != "doc" {
		t.Fatalf("files = %+v", files)
	}
	if err := s.UpdateWorkspaceNotes(ws.ID, "use when working on the project"); err != nil {
		t.Fatal(err)
	}
	w, _ := s.GetWorkspace(ws.ID)
	if w.AgentNotes == "" {
		t.Fatal("notes not saved")
	}
}

func TestProfile(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetProfile("name", "Ada", "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProfile("name", "Ada Lovelace", "user"); err != nil { // upsert
		t.Fatal(err)
	}
	kv, err := s.GetProfile()
	if err != nil {
		t.Fatal(err)
	}
	if kv["name"] != "Ada Lovelace" {
		t.Fatalf("name = %q", kv["name"])
	}
	txt, err := s.ProfileText()
	if err != nil {
		t.Fatal(err)
	}
	if txt != "name: Ada Lovelace" {
		t.Fatalf("profile text = %q", txt)
	}
}

func TestUserStoreMigrationV2RefreezesLegacyPrompt(t *testing.T) {
	// A v1 user DB: the conversations table exists, one conversation still
	// carries the exact pre-upgrade prompt, one carries a custom (never
	// legacy) prompt. Opening the store must re-freeze ONLY the legacy one
	// onto the current prompt.Base.
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bevin.db"))
	if err != nil {
		t.Fatal(err)
	}
	v1 := `
CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS conversations (
	id                    TEXT PRIMARY KEY,
	title                 TEXT NOT NULL DEFAULT '',
	created_at            TEXT NOT NULL,
	last_activity_at      TEXT NOT NULL,
	state                 TEXT NOT NULL DEFAULT 'active',
	frozen_system_prompt  TEXT NOT NULL DEFAULT '',
	prompt_input_versions TEXT NOT NULL DEFAULT '',
	owner_task_id         TEXT,
	active_model_id       TEXT NOT NULL
);
INSERT INTO schema_version (version, applied_at) VALUES (1, 'x');
INSERT INTO conversations (id, title, created_at, last_activity_at, active_model_id, frozen_system_prompt)
VALUES
	('legacy', 'legacy', 'x', 'x', 'm1', 'You are Bevin, a personal assistant. Be direct and concise.'),
	('custom', 'custom', 'x', 'x', 'm1', 'a custom frozen prompt');
`
	if _, err := raw.Exec(v1); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	legacy, err := s.GetConversation("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.FrozenSystemPrompt != prompt.Base {
		t.Fatalf("legacy conv prompt = %q, want prompt.Base", legacy.FrozenSystemPrompt)
	}
	custom, err := s.GetConversation("custom")
	if err != nil {
		t.Fatal(err)
	}
	if custom.FrozenSystemPrompt != "a custom frozen prompt" {
		t.Fatalf("custom conv prompt = %q (must be untouched)", custom.FrozenSystemPrompt)
	}
	// The version row was recorded.
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v < currentSchemaVersion {
		t.Fatalf("schema version = %d, want >= %d", v, currentSchemaVersion)
	}
}

func TestServerStoreUserCredSession(t *testing.T) {
	ss, err := OpenServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	u, err := ss.CreateUser("ada", "Ada", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != "admin" {
		t.Fatalf("role = %q", u.Role)
	}
	if _, err := ss.CreateUser("ada", "dup", "user"); err == nil {
		t.Fatal("expected duplicate-user error")
	}

	if err := ss.SetCredential("ada", "$argon2id$hash"); err != nil {
		t.Fatal(err)
	}
	if h, _ := ss.PasswordHash("ada"); h != "$argon2id$hash" {
		t.Fatalf("hash = %q", h)
	}

	if err := ss.CreateSession("tok123", "ada", time.Hour, "test-ua"); err != nil {
		t.Fatal(err)
	}
	if sess, err := ss.GetSession("tok123"); err != nil || sess.Handle != "ada" {
		t.Fatalf("session = %+v err=%v", sess, err)
	}
	if _, err := ss.GetSession("missing"); err != ErrSessionNotFound {
		t.Fatalf("want ErrSessionNotFound, got %v", err)
	}
	// An already-expired session is not found.
	if err := ss.CreateSession("tok0", "ada", -time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.GetSession("tok0"); err != ErrSessionNotFound {
		t.Fatalf("expired should be not found, got %v", err)
	}

	if err := ss.SetUserStatus("ada", "disabled"); err != nil {
		t.Fatal(err)
	}
	if u2, _ := ss.GetUser("ada"); u2.Status != "disabled" {
		t.Fatalf("status = %q", u2.Status)
	}
}

func ptrStr(s string) *string { return &s }