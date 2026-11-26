package store

import "testing"

// TestDisplaySeqTracking: MaxDisplaySeq + DisplaySeqsAfter — the
// multi-device done-event's seqs: the displayable rows (user/assistant/
// summary) persisted after a snapshot, in seq order. Checkin rows
// (UI-hidden) never count.
func TestDisplaySeqTracking(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("hello", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.MaxDisplaySeq(c.ID); err != nil || n != 0 {
		t.Fatalf("empty MaxDisplaySeq = %d, %v (want 0)", n, err)
	}

	u0, err := s.AppendMessage(c.ID, "user", "q1", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	a0, err := s.AppendMessage(c.ID, "assistant", "r1", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A checkin row (UI-hidden): it takes a seq but is not displayable.
	ck, err := s.AppendMessage(c.ID, "checkin", "note", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s0, err := s.AppendMessage(c.ID, "summary", "folded", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if n, err := s.MaxDisplaySeq(c.ID); err != nil || n != s0.Seq {
		t.Fatalf("MaxDisplaySeq = %d, %v (want %d, the summary)", n, err, s0.Seq)
	}
	// The pre-turn snapshot: everything after u0 — the checkin row takes a
	// seq but is not displayable, so it is excluded.
	seqs, err := s.DisplaySeqsAfter(c.ID, u0.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 2 || seqs[0] != a0.Seq || seqs[1] != s0.Seq {
		t.Fatalf("DisplaySeqsAfter(%d) = %v, want [%d %d] (the checkin %d excluded)",
			u0.Seq, seqs, a0.Seq, s0.Seq, ck.Seq)
	}
	// A snapshot at the head: the whole displayable history (the checkin
	// row is excluded).
	all, err := s.DisplaySeqsAfter(c.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0] != u0.Seq || all[1] != a0.Seq || all[2] != s0.Seq {
		t.Fatalf("DisplaySeqsAfter(-1) = %v, want the 3 displayable rows in order", all)
	}
	// Nothing past the tail.
	none, err := s.DisplaySeqsAfter(c.ID, s0.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("DisplaySeqsAfter(tail) = %v, want empty", none)
	}
}
