package agent

import (
	"context"
	"fmt"
	"testing"
)

// TestAppendMessageNotifies: every row persisted through appendMessage
// fires the multi-device nudge hook with (handle, conversation, the
// persisted seq), and a nil hook (the unit-test default) is safe.
func TestAppendMessageNotifies(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	var calls []string
	a.NotifyMessage = func(handle, convID string, seq int) {
		calls = append(calls, fmt.Sprintf("%s %s %d", handle, convID, seq))
	}
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	m, err := a.appendMessage("u1", us, convID, roleUser, "hello", "", "", nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("hook calls = %d, want 1: %v", len(calls), calls)
	}
	want := fmt.Sprintf("u1 %s %d", convID, m.Seq)
	if calls[0] != want {
		t.Fatalf("hook call = %q, want %q", calls[0], want)
	}
	// A nil hook must be safe (agents built by tests wire none).
	a.NotifyMessage = nil
	if _, err := a.appendMessage("u1", us, convID, roleAssistant, "hi", "", "", nil); err != nil {
		t.Fatalf("appendMessage (nil hook): %v", err)
	}
}
