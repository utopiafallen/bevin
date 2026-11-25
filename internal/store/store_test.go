package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
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

	m1, err := s.AppendMessage(c.ID, "user", "what is a quasar?", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Seq != 0 {
		t.Fatalf("seq = %d", m1.Seq)
	}
	m2, err := s.AppendMessage(c.ID, "assistant", "a quasar is a luminous active galactic nucleus", "", "", ptrStr("m1"))
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
			if _, err := s.AppendMessage(c.ID, "user", "concurrent write test", "", "", nil); err != nil {
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
			if _, err := s.AppendMessage(c.ID, "user", "seed message", "", "", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendMessage(c.ID, "assistant", "seed reply", "", "", ptrStr("m1")); err != nil {
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
				if _, err := s.AppendMessage(c.ID, "user", "ping", "", "", nil); err != nil {
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
				_, err := s.AppendMessage(ids[i], "assistant", "fake reply text", "", "", ptrStr("m1"))
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
		if _, err := s.AppendMessage(c.ID, "user", "existing user message", "", "", nil); err != nil {
			t.Fatal(err)
		}
		ids[i] = c.ID
	}
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		go func(i int) {
			<-start
			_, err := s.AppendMessage(ids[i], "assistant", "the model reply text", "", "", ptrStr("m1"))
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

// TestUserStoreRefusesNewerSchema pins the version-reset invariant (user
// directive: no migration paths — no live data worth migrating): a bevin.db
// stamped with a schema version NEWER than this build is refused loudly at
// open, while a fresh DB is created at the current version.
func TestUserStoreRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bevin.db"))
	if err != nil {
		t.Fatal(err)
	}
	dml := `
CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);
INSERT INTO schema_version (version, applied_at) VALUES (99, 'x');`
	if _, err := raw.Exec(dml); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUser(dir); err == nil {
		t.Fatal("a DB stamped with a newer schema version must be refused, got nil error")
	}

	s, err := OpenUser(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("fresh DB schema version = %d, want %d", v, currentSchemaVersion)
	}
}

func TestMessageReasoningRoundTrip(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("thinking", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(c.ID, "user", "solve this", "", "", nil); err != nil {
		t.Fatal(err)
	}
	reply, err := s.AppendMessage(c.ID, "assistant", "the answer is 42", "let me reason about this step by step", "EqQCsig-abc", ptrStr("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Reasoning != "let me reason about this step by step" {
		t.Fatalf("reply.Reasoning = %q (not returned from append)", reply.Reasoning)
	}
	if reply.ReasoningSignature != "EqQCsig-abc" {
		t.Fatalf("reply.ReasoningSignature = %q (not returned from append)", reply.ReasoningSignature)
	}
	msgs, err := s.ListMessages(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %d, want 2", len(msgs))
	}
	if msgs[0].Reasoning != "" || msgs[0].ReasoningSignature != "" {
		t.Fatalf("user msg reasoning/sig = %q/%q, want empty/empty", msgs[0].Reasoning, msgs[0].ReasoningSignature)
	}
	if msgs[1].Reasoning != "let me reason about this step by step" {
		t.Fatalf("assistant reasoning not round-tripped: %q", msgs[1].Reasoning)
	}
	if msgs[1].ReasoningSignature != "EqQCsig-abc" {
		t.Fatalf("assistant signature not round-tripped: %q", msgs[1].ReasoningSignature)
	}
}

// TestConversationReasoningEffortRoundTrip — the conversation's stored
// reasoning-effort choice round-trips through the store (set, read back,
// clear) and is included in the list rows (the UI reads it from the
// conversation list, not a per-row fetch).
func TestConversationReasoningEffortRoundTrip(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("effort", "m1")
	if err != nil {
		t.Fatal(err)
	}
	// No choice yet: empty.
	c0, err := s.GetConversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c0.ReasoningEffort != "" {
		t.Fatalf("new conversation ReasoningEffort = %q, want empty", c0.ReasoningEffort)
	}
	// Set it; the choice round-trips.
	if err := s.SetConversationReasoningEffort(c.ID, "xhigh"); err != nil {
		t.Fatal(err)
	}
	c1, err := s.GetConversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c1.ReasoningEffort != "xhigh" {
		t.Fatalf("ReasoningEffort = %q, want %q", c1.ReasoningEffort, "xhigh")
	}
	// The list rows carry it too.
	convList, err := s.ListConversations()
	if err != nil {
		t.Fatal(err)
	}
	if len(convList) != 1 || convList[0].ReasoningEffort != "xhigh" {
		t.Fatalf("list = %+v, want one conversation with effort xhigh", convList)
	}
	// Clearing it stores empty again (the provider's default applies).
	if err := s.SetConversationReasoningEffort(c.ID, ""); err != nil {
		t.Fatal(err)
	}
	c2, err := s.GetConversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c2.ReasoningEffort != "" {
		t.Fatalf("cleared ReasoningEffort = %q, want empty", c2.ReasoningEffort)
	}
}

// TestDeleteConversation — a delete removes the conversation row and every
// one of its message rows (the FTS index follows via its triggers, so a
// search no longer surfaces the deleted content); deleting an unknown or
// already-deleted conversation is ErrNotFound.
func TestDeleteConversation(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("doomed", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(c.ID, "user", "zebra fact one", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(c.ID, "assistant", "zebra fact two", "", "", ptrStr("m1")); err != nil {
		t.Fatal(err)
	}
	// A second conversation must be untouched.
	other, err := s.CreateConversation("keeper", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(other.ID, "user", "stay put", "", "", nil); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteConversation(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation(c.ID); err != ErrNotFound {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	// The messages are gone (ListMessages reports an empty history for a
	// conversation with no rows).
	msgs, err := s.ListMessages(c.ID)
	if err != nil {
		t.Fatalf("list after delete = %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages after delete = %+v, want none", msgs)
	}
	// FTS no longer matches the deleted content...
	hits, err := s.SearchMessages("zebra", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("fts hits after delete = %+v, want none", hits)
	}
	// ...and the other conversation is intact.
	if _, err := s.GetConversation(other.ID); err != nil {
		t.Fatalf("sibling conversation damaged: %v", err)
	}
	// Deleting again is ErrNotFound (no double-delete surprise).
	if err := s.DeleteConversation(c.ID); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	// Unknown id is ErrNotFound.
	if err := s.DeleteConversation("nope"); err != ErrNotFound {
		t.Fatalf("delete unknown = %v, want ErrNotFound", err)
	}
}

// TestListMessagesPaged — the lazy-load pagination (issue 3): returns the
// most recent limit DISPLAYABLE messages (user/assistant/summary) in
// seq-ascending order, optionally restricted to seq < beforeSeq (the
// scroll-up cursor). The page size counts displayable rows only — check-in /
// system rows (model-facing, dropped by the display path) never count, even
// when they occupy the low seqs. A full walk reconstructs every displayable
// message in order with no gaps or overlaps.
func TestListMessagesPaged(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("paged", "m1")
	if err != nil {
		t.Fatal(err)
	}
	// 10 check-in rows first (seq 0..9) — non-displayable, and they occupy the
	// low seqs (the case that would break a naive seq-0 "head" check).
	for i := 0; i < 10; i++ {
		if _, err := s.AppendMessage(c.ID, "checkin", "check-in", "", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// 30 displayable messages (seq 10..39), alternating user/assistant, plus a
	// summary (compaction) row carrying token counts (seq 40).
	for i := 0; i < 30; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := s.AppendMessage(c.ID, role, fmt.Sprintf("message %d", i), "", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := s.AppendMessage(c.ID, "summary", "context summary", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMessageCompaction(sum.ID, 1000, 400); err != nil {
		t.Fatal(err)
	}
	// Total: 41 raw rows, 31 displayable (seq 10..40).

	const limit = 25
	// Page 1: the 25 most recent displayable rows (seq 16..40). The check-in
	// rows (seq 0..9) do not count, so the oldest here is seq 16, NOT seq 0.
	p1, err := s.ListMessagesPaged(c.ID, limit, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 25 {
		t.Fatalf("page1 = %d, want 25 (displayable, not raw rows)", len(p1))
	}
	for i := range p1 {
		if p1[i].Role == "checkin" || p1[i].Role == "system" {
			t.Fatalf("page1[%d] = %q role, want only displayable roles", i, p1[i].Role)
		}
		if want := 16 + i; p1[i].Seq != want {
			t.Fatalf("page1[%d].Seq = %d, want %d", i, p1[i].Seq, want)
		}
	}
	// The summary is the newest displayable (seq 40) with its token counts.
	if p1[24].Role != "summary" || p1[24].CompactedFrom != 1000 || p1[24].CompactedTo != 400 {
		t.Fatalf("page1[24] = %+v, want summary 1000->400", p1[24])
	}

	// Page 2: displayable rows strictly before seq 16 -> seq 10..15 (6 rows,
	// a short page => the head is reached).
	p2, err := s.ListMessagesPaged(c.ID, limit, p1[0].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2) != 6 {
		t.Fatalf("page2 = %d, want 6", len(p2))
	}
	if p2[0].Seq != 10 || p2[len(p2)-1].Seq != 15 {
		t.Fatalf("page2 span = %d..%d, want 10..15", p2[0].Seq, p2[len(p2)-1].Seq)
	}

	// Full walk: 31 displayable messages, seq strictly increasing, no gaps.
	collected := append(append([]Message{}, p2...), p1...)
	if len(collected) != 31 {
		t.Fatalf("walk = %d, want 31", len(collected))
	}
	for i := 1; i < len(collected); i++ {
		if collected[i].Seq <= collected[i-1].Seq {
			t.Fatalf("walk not strictly increasing at %d: %d -> %d", i, collected[i-1].Seq, collected[i].Seq)
		}
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

// TestWorkspaceDeleteCascade — workspace + file metadata lifecycle:
// DeleteWorkspaceFilesAt prunes the file at a path (and everything
// beneath a directory); DeleteWorkspace removes the row and its file
// rows cascade.
func TestWorkspaceDeleteCascade(t *testing.T) {
	s, err := OpenUser(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace("Notes", "d", "/tmp/x")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "docs/one.txt", "docs/two.txt"} {
		if _, err := s.AddWorkspaceFile(ws.ID, p, p, "text/plain", 10, "", "user", "doc"); err != nil {
			t.Fatal(err)
		}
	}
	// Deleting the "docs" directory prunes both files beneath it.
	if err := s.DeleteWorkspaceFilesAt(ws.ID, "docs"); err != nil {
		t.Fatal(err)
	}
	files, err := s.ListWorkspaceFiles(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].RelativePath != "a.txt" {
		t.Fatalf("files = %+v, want only a.txt", files)
	}
	// Deleting the workspace removes the row + the rest cascades.
	if err := s.DeleteWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorkspace(ws.ID); err != ErrNotFound {
		t.Fatalf("GetWorkspace after delete = %v, want ErrNotFound", err)
	}
	if files, err := s.ListWorkspaceFiles(ws.ID); err != nil || len(files) != 0 {
		t.Fatalf("files after workspace delete = %+v (err %v), want none", files, err)
	}
}
