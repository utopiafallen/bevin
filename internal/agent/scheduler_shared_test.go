package agent

import (
	"context"
	"testing"
	"time"
)

// TestSharedPoolBoundsBothWorkloads — the actual sharing: main-model turns
// and the shared aux's perception jobs submit to the SAME pool, and the
// MAIN MODEL'S slot count bounds their combined concurrency. The shared aux
// deliberately carries a LARGER own slot count (5 > the main's 3): if the
// scheduler ever honored the aux's count for the shared pool, the fourth
// job below would start while the three holders run — it must not.
func TestSharedPoolBoundsBothWorkloads(t *testing.T) {
	cfg := auxSlotTestConfig()
	cfg.Aux[0].Slots = 5 // larger than the main's 3 — the trap
	a, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	pool := cfg.Aux[0].PoolID // == "m1" (exact match)
	if pool != "m1" {
		t.Fatalf("shared aux pool = %q, want m1", pool)
	}

	// Three "conversation turn" jobs hold all 3 slots of the shared pool.
	// Each holds until its OWN release channel closes (so exactly one slot
	// frees at a time).
	releases := [3]chan struct{}{}
	for i := range releases {
		releases[i] = make(chan struct{})
	}
	convNames := []string{"conv-a", "conv-b", "conv-c"}
	for i, name := range convNames {
		conv := name
		rel := releases[i]
		go func() {
			_ = a.sched.Submit(context.Background(), JobInfo{Handle: "u", ConvID: conv, ModelID: "m1"}, func(context.Context) error {
				<-rel
				return nil
			})
		}()
	}
	// Wait until all three holders actually hold slots (an earlier-submitting
	// job could otherwise still be starting).
	deadline := time.After(2 * time.Second)
	for {
		if l := a.sched.Load()[pool]; l.Busy == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the three main-model holders never all started (load: %+v)", a.sched.Load()[pool])
		case <-time.After(5 * time.Millisecond):
		}
	}

	// The aux's perception job goes to the SAME pool (its resolved pool id).
	// The pool is full (3/3) — it must queue, even though the aux's own
	// slot count is 5. It holds its slot (once granted) until the test
	// releases it, so the refilled 3/3 state is stable and observable.
	perceptionRan := make(chan struct{})
	perceptionHold := make(chan struct{})
	go func() {
		_ = a.sched.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "perception", ModelID: cfg.Aux[0].PoolID}, func(context.Context) error {
			close(perceptionRan)
			<-perceptionHold
			return nil
		})
	}()
	deadline = time.After(2 * time.Second)
	for {
		if l := a.sched.Load()[pool]; len(l.Waiting) == 1 {
			break
		}
		select {
		case <-perceptionRan:
			t.Fatalf("the perception job started while the pool was full (3/3) — the shared aux's own slot count (%d) leaked into the shared pool", cfg.Aux[0].Slots)
		case <-deadline:
			t.Fatalf("the perception job never queued (load: %+v)", a.sched.Load()[pool])
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Releasing ONE main-model slot lets the perception job in — it shares
	// the pool, it does not have one of its own. The pool refills to 3/3
	// (two holders + the perception job).
	close(releases[0])
	select {
	case <-perceptionRan:
	case <-time.After(2 * time.Second):
		t.Fatalf("the perception job did not start after a shared slot freed (load: %+v)", a.sched.Load()[pool])
	}
	deadline = time.After(2 * time.Second)
	for {
		if l := a.sched.Load()[pool]; l.Busy == 3 && len(l.Waiting) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the pool never refilled to 3/3 after the hand-off (load: %+v)", a.sched.Load()[pool])
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Drain: release the perception job and the remaining holders.
	close(perceptionHold)
	close(releases[1])
	close(releases[2])
}
