package agent

import (
	"context"
	"fmt"
	"testing"
)

// TestNotifyMessageViaStoreHook: the multi-device nudge rides on the user
// store's AfterAppend lifecycle hook (set when the agent opens the store),
// so a RAW us.AppendMessage — the ordinary call site now — fires it with
// (handle, conversation, the persisted seq). The handle comes from the
// store's wiring, not the call site. A nil hook (unit-test default) is
// safe.
func TestNotifyMessageViaStoreHook(t *testing.T) {
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
	// Open the store the agent way (the wiring happens there) and append
	// directly on it — the way every persistence site calls.
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	m, err := us.AppendMessage(convID, roleUser, "hello", "", "", nil)
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("hook calls = %d, want 1: %v", len(calls), calls)
	}
	want := fmt.Sprintf("u1 %s %d", convID, m.Seq)
	if calls[0] != want {
		t.Fatalf("hook call = %q, want %q", calls[0], want)
	}
	// The cached store (second lookup) keeps the wiring — and the hook is
	// still nil-safe once the observer is unset.
	us2, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore (cached): %v", err)
	}
	if us2 != us {
		t.Fatal("second userStore lookup must return the cached store (the wiring lives on it)")
	}
	a.NotifyMessage = nil
	if _, err := us.AppendMessage(convID, roleAssistant, "hi", "", "", nil); err != nil {
		t.Fatalf("AppendMessage (nil hook): %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("nil hook must not fire: %v", calls)
	}
}
