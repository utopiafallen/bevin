package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestSchedulerDefersFrozenUser verifies the two-way link to the disk cap: a
// user who is frozen (over their disk cap) has their queued work DEFERRED even
// when a slot is free (it stays queued, not dropped), and it runs once they're
// back under the cap + the host pokes the scheduler (PokeAll, fired by the disk
// monitor's unfreeze hook).
func TestSchedulerDefersFrozenUser(t *testing.T) {
	var frozen atomic.Bool
	frozen.Store(true) // alice is over her disk cap
	s := NewScheduler(func(string) int { return 1 })
	s.SetFrozen(func(h string) bool { return h == "alice" && frozen.Load() })

	started := make(chan struct{})
	done := make(chan error, 1)
	// The pool is idle (a free slot), but alice is frozen: her job must queue,
	// not run.
	go func() {
		done <- s.Submit(context.Background(), JobInfo{Handle: "alice", ConvID: "c", ModelID: "m"}, func(ctx context.Context) error {
			close(started)
			return nil
		})
	}()

	// Poll until the job settles in the queue (it queued, not started). If it
	// took the free slot (Busy) the deferral is broken.
	deadline := time.Now().Add(2 * time.Second)
	var l PoolLoad
	for {
		l = s.Load()["m"]
		if l.Busy == 0 && len(l.Waiting) == 1 && l.Waiting[0].Handle == "alice" {
			break
		}
		if l.Busy == 1 {
			t.Fatal("a frozen user's job took the free slot (it must be deferred, not run)")
		}
		if time.Now().After(deadline) {
			t.Fatalf("frozen job did not settle in the queue (load: %+v)", l)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Confirmed: the slot is free but her job is deferred in the queue.
	select {
	case <-started:
		t.Fatal("a frozen user's job started while they're over the cap")
	default:
	}

	// Back under the cap → the deferred job runs (PokeAll re-dispatches).
	frozen.Store(false)
	s.PokeAll()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred job did not run after unfreeze + PokeAll")
	}
	if err := <-done; err != nil {
		t.Fatalf("submit returned: %v", err)
	}
}
