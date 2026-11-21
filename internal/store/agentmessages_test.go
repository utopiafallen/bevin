package store

import "testing"

// TestAgentMessageQueue is the store-level half of the sibling queue: send
// queues (undelivered), pending lists oldest-first, the per-pair count
// feeds the cap, and marking delivered clears both.
func TestAgentMessageQueue(t *testing.T) {
	us, err := OpenUser(t.TempDir())
	if err != nil {
		t.Fatalf("OpenUser: %v", err)
	}
	defer us.Close()

	m1, err := us.SendAgentMessage("a", "b", "first")
	if err != nil {
		t.Fatalf("SendAgentMessage: %v", err)
	}
	if m1.DeliveredAt != "" {
		t.Fatalf("a fresh message must be queued: %+v", m1)
	}
	if _, err := us.SendAgentMessage("a", "b", "second"); err != nil {
		t.Fatalf("SendAgentMessage: %v", err)
	}
	if _, err := us.SendAgentMessage("c", "b", "other sender"); err != nil {
		t.Fatalf("SendAgentMessage: %v", err)
	}

	pending, err := us.PendingAgentMessages("b")
	if err != nil {
		t.Fatalf("PendingAgentMessages: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3: %+v", len(pending), pending)
	}
	if pending[0].Text != "first" || pending[0].ID != m1.ID {
		t.Fatalf("pending is not oldest-first: %+v", pending)
	}
	// The per-pair count (the cap): only a->b counts for pair (a, b).
	if n, err := us.CountPendingAgentMessages("a", "b"); err != nil || n != 2 {
		t.Fatalf("count(a,b) = %d, %v — want 2", n, err)
	}
	if n, err := us.CountPendingAgentMessages("b", "a"); err != nil || n != 0 {
		t.Fatalf("count(b,a) = %d, %v — want 0 (direction matters)", n, err)
	}
	// Delivery to b marks every queued message to b (both senders).
	if err := us.MarkAgentMessagesDelivered("b"); err != nil {
		t.Fatalf("MarkAgentMessagesDelivered: %v", err)
	}
	pending, _ = us.PendingAgentMessages("b")
	if len(pending) != 0 {
		t.Fatalf("still pending after delivery: %+v", pending)
	}
	if n, _ := us.CountPendingAgentMessages("a", "b"); n != 0 {
		t.Fatalf("count after delivery = %d, want 0", n)
	}
}
