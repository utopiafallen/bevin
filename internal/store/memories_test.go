package store

import (
	"testing"
)

// TestSearchMemoriesInConversation — the search scope: FTS hits restricted
// to memories whose provenance touches the named conversation. A memory
// without provenance (or provenance only in other conversations) does not
// match; an unknown conversation returns empty (not an error).
func TestSearchMemoriesInConversation(t *testing.T) {
	us, err := OpenUser(t.TempDir())
	if err != nil {
		t.Fatalf("OpenUser: %v", err)
	}
	defer us.Close()

	convA, err := us.CreateConversation("A", "m1")
	if err != nil {
		t.Fatalf("CreateConversation A: %v", err)
	}
	convB, err := us.CreateConversation("B", "m1")
	if err != nil {
		t.Fatalf("CreateConversation B: %v", err)
	}
	msgA, err := us.AppendMessage(convA.ID, "user", "about the zephyr plan", "", "", nil)
	if err != nil {
		t.Fatalf("AppendMessage A: %v", err)
	}
	msgB, err := us.AppendMessage(convB.ID, "user", "zephyr again", "", "", nil)
	if err != nil {
		t.Fatalf("AppendMessage B: %v", err)
	}

	mA, err := us.CreateMemory("work", "the zephyr project kickoff", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory A: %v", err)
	}
	if err := us.SetMemoryProvenance(mA.ID, []int64{msgA.ID}); err != nil {
		t.Fatalf("SetMemoryProvenance A: %v", err)
	}
	mB, err := us.CreateMemory("work", "a zephyr follow-up", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory B: %v", err)
	}
	if err := us.SetMemoryProvenance(mB.ID, []int64{msgB.ID}); err != nil {
		t.Fatalf("SetMemoryProvenance B: %v", err)
	}
	// No provenance at all — never conversation-scoped.
	mNone, err := us.CreateMemory("work", "another zephyr fact", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory none: %v", err)
	}

	all, err := us.SearchMemories("zephyr", 10)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unscoped search = %d memories, want 3", len(all))
	}

	inA, err := us.SearchMemoriesInConversation("zephyr", convA.ID, 10)
	if err != nil {
		t.Fatalf("SearchMemoriesInConversation A: %v", err)
	}
	if len(inA) != 1 || inA[0].ID != mA.ID {
		t.Fatalf("scope A = %v, want only memory %d", ids(inA), mA.ID)
	}

	inB, err := us.SearchMemoriesInConversation("zephyr", convB.ID, 10)
	if err != nil {
		t.Fatalf("SearchMemoriesInConversation B: %v", err)
	}
	if len(inB) != 1 || inB[0].ID != mB.ID {
		t.Fatalf("scope B = %v, want only memory %d", ids(inB), mB.ID)
	}

	none, err := us.SearchMemoriesInConversation("zephyr", "no-such-conversation", 10)
	if err != nil {
		t.Fatalf("SearchMemoriesInConversation unknown: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown conversation scope = %v, want empty (the no-provenance memory %d must never match)", ids(none), mNone.ID)
	}
}

func TestListActiveMemoriesInConversation(t *testing.T) {
	us, err := OpenUser(t.TempDir())
	if err != nil {
		t.Fatalf("OpenUser: %v", err)
	}
	defer us.Close()

	convA, err := us.CreateConversation("A", "m1")
	if err != nil {
		t.Fatalf("CreateConversation A: %v", err)
	}
	convB, err := us.CreateConversation("B", "m1")
	if err != nil {
		t.Fatalf("CreateConversation B: %v", err)
	}
	msgA, err := us.AppendMessage(convA.ID, "user", "about the zephyr plan", "", "", nil)
	if err != nil {
		t.Fatalf("AppendMessage A: %v", err)
	}
	msgB, err := us.AppendMessage(convB.ID, "user", "zephyr again", "", "", nil)
	if err != nil {
		t.Fatalf("AppendMessage B: %v", err)
	}

	mA, err := us.CreateMemory("work", "the zephyr project kickoff", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory A: %v", err)
	}
	if err := us.SetMemoryProvenance(mA.ID, []int64{msgA.ID}); err != nil {
		t.Fatalf("SetMemoryProvenance A: %v", err)
	}
	mB, err := us.CreateMemory("work", "a zephyr follow-up", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory B: %v", err)
	}
	if err := us.SetMemoryProvenance(mB.ID, []int64{msgB.ID}); err != nil {
		t.Fatalf("SetMemoryProvenance B: %v", err)
	}
	// No provenance at all — never a conversation-scoped candidate.
	if _, err := us.CreateMemory("work", "another zephyr fact", nil, 0.9, "test", nil); err != nil {
		t.Fatalf("CreateMemory none: %v", err)
	}
	// Superseded + provenance in A — excluded (active only).
	mDead, err := us.CreateMemory("work", "an old zephyr fact", nil, 0.9, "test", nil)
	if err != nil {
		t.Fatalf("CreateMemory dead: %v", err)
	}
	if err := us.SetMemoryProvenance(mDead.ID, []int64{msgA.ID}); err != nil {
		t.Fatalf("SetMemoryProvenance dead: %v", err)
	}
	if err := us.ForgetMemory(mDead.ID); err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}

	// FTS plays no role: the statement need not share a keyword with any
	// query — the candidate set is the provenance set itself.
	gotA, err := us.ListActiveMemoriesInConversation(convA.ID)
	if err != nil {
		t.Fatalf("ListActiveMemoriesInConversation A: %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID != mA.ID {
		t.Fatalf("scope A candidates = %v, want only memory %d (the superseded %d excluded)", ids(gotA), mA.ID, mDead.ID)
	}
	gotB, err := us.ListActiveMemoriesInConversation(convB.ID)
	if err != nil {
		t.Fatalf("ListActiveMemoriesInConversation B: %v", err)
	}
	if len(gotB) != 1 || gotB[0].ID != mB.ID {
		t.Fatalf("scope B candidates = %v, want only memory %d", ids(gotB), mB.ID)
	}
	gotNone, err := us.ListActiveMemoriesInConversation("no-such-conversation")
	if err != nil {
		t.Fatalf("ListActiveMemoriesInConversation unknown: %v", err)
	}
	if len(gotNone) != 0 {
		t.Fatalf("unknown conversation = %v, want empty", ids(gotNone))
	}
}

func ids(ms []Memory) []int64 {
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
