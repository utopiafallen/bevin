package store

import (
	"context"
	"testing"
	"time"
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