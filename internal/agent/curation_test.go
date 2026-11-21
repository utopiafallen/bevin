// Tests for the memory-curation task: the growth trigger, the closed range,
// the spawn guard, the watermark, the curator's own writes, and the
// set_profile single-writer surface.
package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// curationTasks lists this user's curation tasks (by the fixed title).
func curationTasks(t *testing.T, a *Agent, handle string) []store.Task {
	t.Helper()
	us, err := a.userStore(handle)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := us.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Task
	for _, tk := range tasks {
		if tk.Title == CurationTaskTitle {
			out = append(out, tk)
		}
	}
	return out
}

// remember calls the remember tool N times in a conversation (the mid-turn
// capture path — the only live memory growth).
func remember(t *testing.T, a *Agent, handle, convID string, from, to int) {
	t.Helper()
	tool, ok := a.tools.Get("remember")
	if !ok {
		t.Fatal("remember is not registered")
	}
	for i := from; i <= to; i++ {
		args := fmt.Sprintf(`{"facet":"test","statement":"test memory %d: the user has a distinct fact number %d"}`, i, i)
		ctx := context.WithValue(context.Background(), ctxConversationKey, convID)
		if _, err := tool.Execute(ctx, handle, []byte(args)); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}
}

// waitForCuration waits until exactly one curation task exists.
func waitForCuration(t *testing.T, a *Agent, handle string) store.Task {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if list := curationTasks(t, a, handle); len(list) == 1 {
			return list[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no curation task appeared (got %d)", len(curationTasks(t, a, handle)))
	return store.Task{}
}

// TestCurationSpawnsAtThreshold: 9 new memories do not earn a pass; the 10th
// does. The task carries the CLOSED [1,10] range (the first run backfills
// from watermark 0), no explicit model (any available main model), and —
// once done — its end becomes the watermark: 5 more memories (growth 5 < 10)
// do not spawn a second pass.
func TestCurationSpawnsAtThreshold(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	a.providers["m1"] = &fakeProvider{replies: []string{"curated the range."}}
	conv, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	remember(t, a, "u1", conv, 1, 9)
	if list := curationTasks(t, a, "u1"); len(list) != 0 {
		t.Fatalf("%d curation tasks after 9 memories, want 0 (below the meaty threshold)", len(list))
	}

	remember(t, a, "u1", conv, 10, 10) // the 10th: threshold crossed
	task := waitForCuration(t, a, "u1")
	if task.MemoryStart == nil || *task.MemoryStart != 1 {
		t.Fatalf("curation memory_start = %v, want 1 (watermark 0 + 1, the first run backfills)", task.MemoryStart)
	}
	if task.MemoryEnd == nil || *task.MemoryEnd != 10 {
		t.Fatalf("curation memory_end = %v, want 10 (the closed snapshot range)", task.MemoryEnd)
	}
	if task.Model != nil {
		t.Fatalf("curation model = %v, want nil (any available main model — no explicit target)", task.Model)
	}
	done := waitTaskState(t, a, "u1", task.ID, "done")
	if done.ConversationID == nil || *done.ConversationID == "" {
		t.Fatal("the done curation task has no owned conversation")
	}

	// Watermark advanced to 10: growth of 5 must not spawn a second pass.
	remember(t, a, "u1", conv, 11, 15)
	time.Sleep(300 * time.Millisecond) // give a (wrong) spawn time to land
	if list := curationTasks(t, a, "u1"); len(list) != 1 {
		t.Fatalf("%d curation tasks after 5 more memories, want 1 (growth 5 < threshold 10)", len(list))
	}
}

// TestCurationSpamGuard: with a curation task already queued/running, more
// growth does not spawn a second one (one curation at a time).
func TestCurationSpamGuard(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	gp := &gateProvider{gate: make(chan struct{})} // holds the turn: the task stays in-progress
	a.providers["m1"] = gp
	conv, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	remember(t, a, "u1", conv, 1, 10)
	waitForCuration(t, a, "u1")

	// More growth while the first pass is still running: no second task.
	remember(t, a, "u1", conv, 11, 15)
	time.Sleep(300 * time.Millisecond)
	if list := curationTasks(t, a, "u1"); len(list) != 1 {
		t.Fatalf("%d curation tasks with one already running, want 1 (the spawn guard)", len(list))
	}

	close(gp.gate)
	waitTaskState(t, a, "u1", curationTasks(t, a, "u1")[0].ID, "done")
}

// TestCurationBlockedSpawnsAgain: a BLOCKED curation run is terminal — it
// does not advance the watermark — so the next threshold crossing spawns a
// fresh attempt over the new range.
func TestCurationBlockedSpawnsAgain(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	a.providers["m1"] = errProvider{err: fmt.Errorf("model exploded")}
	conv, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	remember(t, a, "u1", conv, 1, 10)
	first := waitForCuration(t, a, "u1")
	waitTaskState(t, a, "u1", first.ID, "blocked")

	// The watermark never moved (blocked is not done): one more memory
	// (11 >= 10 past watermark 0) spawns a fresh attempt.
	remember(t, a, "u1", conv, 11, 11)
	var second store.Task
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if list := curationTasks(t, a, "u1"); len(list) == 2 {
			second = list[1]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if second.ID == "" {
		t.Fatal("no second curation task after the blocked run (the watermark must not have advanced)")
	}
	if second.MemoryEnd == nil || *second.MemoryEnd != 11 {
		t.Fatalf("second run memory_end = %v, want 11 (the new closed range)", second.MemoryEnd)
	}
	// Settle the second run (the provider is still broken → blocked) so the
	// test ends with no task run in flight.
	waitTaskState(t, a, "u1", second.ID, "blocked")
}

// TestCurationCuratorWritesDoNotRetrigger: memories written FROM the
// curation conversation (the curator's own supersede-creates) are curated
// output, not user growth — they must not re-trigger the very pass they
// belong to (even when the threshold is crossed again).
func TestCurationCuratorWritesDoNotRetrigger(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	gp := &gateProvider{gate: make(chan struct{})}
	a.providers["m1"] = gp
	conv, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	remember(t, a, "u1", conv, 1, 10)
	task := waitForCuration(t, a, "u1")
	// Wait for the owned conversation to exist (dispatch creates it).
	var taskConv string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		tk, err := a.userStore("u1")
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := tk.GetTask(task.ID)
		if err == nil && fresh.ConversationID != nil && *fresh.ConversationID != "" {
			taskConv = *fresh.ConversationID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if taskConv == "" {
		t.Fatal("the curation task never got its owned conversation")
	}

	// The curator (in its own conversation) writes 10 memories — crossing
	// the threshold again past watermark 0 — yet no second task appears.
	remember(t, a, "u1", taskConv, 11, 20)
	time.Sleep(300 * time.Millisecond)
	if list := curationTasks(t, a, "u1"); len(list) != 1 {
		t.Fatalf("%d curation tasks after the curator's own writes, want 1 (curator writes are not growth)", len(list))
	}

	close(gp.gate)
	waitTaskState(t, a, "u1", task.ID, "done")
}

// makeCurationConv builds a curation task + its owned conversation directly
// (the wire-filter unit does not need a live run).
func makeCurationConv(t *testing.T, a *Agent, us *store.UserStore, handle string) (string, *store.Conversation) {
	t.Helper()
	start, end := int64(1), int64(10)
	task, err := us.CreateTaskNew(store.TaskNew{
		Title: CurationTaskTitle, Description: "d", MemStart: &start, MemEnd: &end,
	})
	if err != nil {
		t.Fatal(err)
	}
	convID, err := a.CreateConversation(context.Background(), handle, "task: "+CurationTaskTitle, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetTaskConversation(task.ID, convID); err != nil {
		t.Fatal(err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatal(err)
	}
	return convID, conv
}

func wireOffers(list []provider.Tool, name string) bool {
	for _, wt := range list {
		if wt.Name == name {
			return true
		}
	}
	return false
}

// TestSetProfileScoping: set_profile is offered ONLY in the curation task's
// conversation (a plain chat conv and another task's conv do not get it),
// and the executor refuses direct calls from anywhere but the curation
// conversation (defense in depth — the model imitating the tool from
// history cannot bypass curation's single-writer rule).
func TestSetProfileScoping(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := a.tools.Get("set_profile")
	if !ok {
		t.Fatal("set_profile is not registered")
	}

	// A plain chat conversation: withheld from the wire.
	chat, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	chatConv, err := us.GetConversation(chat)
	if err != nil {
		t.Fatal(err)
	}
	if wireOffers(a.wireTools(us, chatConv), "set_profile") {
		t.Fatal("set_profile offered to a plain chat conversation")
	}

	// Another (non-curation) task's conversation: withheld from the wire.
	other, err := us.CreateTask("chore", "d", "todo", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherConvID, err := a.CreateConversation(context.Background(), "u1", "task: chore", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetTaskConversation(other.ID, otherConvID); err != nil {
		t.Fatal(err)
	}
	otherConv, err := us.GetConversation(otherConvID)
	if err != nil {
		t.Fatal(err)
	}
	if wireOffers(a.wireTools(us, otherConv), "set_profile") {
		t.Fatal("set_profile offered to a non-curation task conversation")
	}
	if !wireOffers(a.wireTools(us, otherConv), "advance_task") {
		t.Fatal("advance_task must still be offered to any task conversation")
	}

	// The curation conversation: offered.
	taskConvID, taskConv := makeCurationConv(t, a, us, "u1")
	if !wireOffers(a.wireTools(us, taskConv), "set_profile") {
		t.Fatal("set_profile not offered to the curation conversation")
	}

	// Executor: refused outside the curation conversation...
	args := []byte(`{"key":"preferences","value":"the user ships on Fridays"}`)
	res, err := tool.Execute(context.WithValue(context.Background(), ctxConversationKey, chat), "u1", args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "only available in the memory-curation task") {
		t.Fatalf("chat-conversation call result = %q, want the single-writer refusal", res)
	}
	res, err = tool.Execute(context.WithValue(context.Background(), ctxConversationKey, otherConvID), "u1", args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "only available in the memory-curation task") {
		t.Fatalf("other-task call result = %q, want the single-writer refusal", res)
	}
	if p, err := us.GetProfile(); err != nil || len(p) != 0 {
		t.Fatalf("profile after refused writes = %v (err %v), want empty", p, err)
	}
	// ...and accepted in the curation conversation (the sole writer).
	res, err = tool.Execute(context.WithValue(context.Background(), ctxConversationKey, taskConvID), "u1", args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "profile updated") {
		t.Fatalf("curation call result = %q, want the update confirmation", res)
	}
	p, err := us.GetProfile()
	if err != nil || p["preferences"] != "the user ships on Fridays" {
		t.Fatalf("profile = %v (err %v), want the curated entry", p, err)
	}
}
