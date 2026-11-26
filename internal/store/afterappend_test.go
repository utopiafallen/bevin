package store

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestAfterAppendHook: the row-persisted lifecycle hook fires exactly
// once per committed append, with the persisted seq, and a nil hook (the
// default for every store the agent did not wire) is a safe no-op.
func TestAfterAppendHook(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("hello", "m1")
	if err != nil {
		t.Fatal(err)
	}
	var fired []int
	s.AfterAppend = func(convID string, seq int) {
		if convID != c.ID {
			t.Fatalf("hook conv = %q, want %q", convID, c.ID)
		}
		fired = append(fired, seq)
	}
	m1, err := s.AppendMessage(c.ID, "user", "a", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := s.AppendMessage(c.ID, "assistant", "b", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fired) != 2 || fired[0] != m1.Seq || fired[1] != m2.Seq {
		t.Fatalf("fired = %v, want [%d %d]", fired, m1.Seq, m2.Seq)
	}
	// A nil hook is the store default — safe no-op.
	s.AfterAppend = nil
	if _, err := s.AppendMessage(c.ID, "user", "c", "", "", nil); err != nil {
		t.Fatalf("AppendMessage (nil hook): %v", err)
	}
}

// TestAfterAppendHookConcurrent: parallel appends to one store (the
// parallel-turns shape) each fire the hook exactly once — the hook is
// called with no store lock held, so the wiring must be race-free. Run
// under -race in the gate.
func TestAfterAppendHookConcurrent(t *testing.T) {
	s := openTestStore(t)
	c, err := s.CreateConversation("hello", "m1")
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	s.AfterAppend = func(convID string, seq int) { atomic.AddInt64(&count, 1) }
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AppendMessage(c.ID, "user", "x", "", "", nil); err != nil {
				t.Errorf("AppendMessage: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt64(&count); n != 25 {
		t.Fatalf("hook fired %d times, want 25", n)
	}
	if msgs, err := s.ListMessages(c.ID); err != nil || len(msgs) != 25 {
		t.Fatalf("msgs = %d, %v — want 25 (the hook never breaks the append)", len(msgs), err)
	}
}
