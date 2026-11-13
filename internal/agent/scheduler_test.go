package agent

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func itoa(i int) string { return strconv.Itoa(i) }

// TestSchedulerEnforcesSlotLimit runs more jobs than slots on one pool and
// verifies concurrency never exceeds the slot count, jobs complete FIFO, and
// no slot leaks. One slot: each grant holds the only slot until the job
// finishes, so the observed start order equals the (FIFO) grant order.
func TestSchedulerEnforcesSlotLimit(t *testing.T) {
	var cur, maxCur int32
	gate := make(chan struct{}) // jobs block here (simulating a model call)
	const slots, jobs = 1, 5
	var slotsFn atomic.Int32
	slotsFn.Store(slots)
	s := NewScheduler(func(string) int { return int(slotsFn.Load()) })

	var wg sync.WaitGroup
	starts := make(chan int, jobs)
	launch := func(i int) {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Submit(context.Background(), JobInfo{Handle: "u" + itoa(i), ConvID: "c" + itoa(i), ModelID: "m"}, func(ctx context.Context) error {
				curNow := atomic.AddInt32(&cur, 1)
				for {
					m := atomic.LoadInt32(&maxCur)
					if curNow <= m || atomic.CompareAndSwapInt32(&maxCur, m, curNow) {
						break
					}
				}
				starts <- i
				<-gate
				atomic.AddInt32(&cur, -1)
				return nil
			})
			if err != nil {
				t.Errorf("job %d: %v", i, err)
			}
		}(i)
	}
	// waitWaiting polls Load until the pool shows n waiting jobs (all settled
	// in the queue, in submission order).
	waitWaiting := func(n int) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if l := s.Load()["m"]; len(l.Waiting) == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %d queued jobs (load: %+v)", n, s.Load()["m"])
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// Hold the only slot with job 0 before anything else is submitted.
	launch(0)
	<-starts
	// Submit the rest one at a time, each settled in the queue before the next,
	// so the queue order is exactly 1, 2, 3, 4.
	for i := 1; i < jobs; i++ {
		launch(i)
		waitWaiting(i)
	}
	// Release all at once; the queued jobs must start in FIFO order.
	close(gate)
	var got []int
	for i := 0; i < jobs-slots; i++ {
		got = append(got, <-starts)
	}
	wg.Wait()
	want := []int{1, 2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FIFO order of the queued batch: got %v, want %v", got, want)
		}
	}
	if m := int(atomic.LoadInt32(&maxCur)); m != slots {
		t.Fatalf("max concurrency = %d, want %d (the slot count)", m, slots)
	}
	if l := s.Load()["m"]; l.Busy != 0 || len(l.Waiting) != 0 {
		t.Fatalf("load after completion = %+v, want idle", l)
	}
}

// TestSchedulerCancelWhileQueued cancels a queued job and verifies it drops
// out of the queue without leaking its (never-granted) slot, and the pool
// still serves the next job.
func TestSchedulerCancelWhileQueued(t *testing.T) {
	s := NewScheduler(func(string) int { return 1 })
	holder := make(chan struct{})
	releaseHolder := make(chan struct{})
	doneHolder := make(chan struct{})
	go func() {
		defer close(doneHolder)
		_ = s.Submit(context.Background(), JobInfo{Handle: "u0", ConvID: "c0", ModelID: "m"}, func(ctx context.Context) error {
			close(holder)
			<-releaseHolder
			return nil
		})
	}()
	<-holder // the holder now owns the single slot

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() { queued <- s.Submit(ctx, JobInfo{Handle: "u1", ConvID: "c1", ModelID: "m"}, func(context.Context) error { return nil }) }()
	time.Sleep(50 * time.Millisecond) // let it queue
	cancel()
	select {
	case err := <-queued:
		if err == nil {
			t.Fatal("canceled queued job should return an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled queued job did not return")
	}
	if l := s.Load()["m"]; l.Busy != 1 || len(l.Waiting) != 0 {
		t.Fatalf("after cancel: busy=%d waiting=%d, want busy=1 waiting=0", l.Busy, len(l.Waiting))
	}
	// The next job must run once the holder releases its slot.
	close(releaseHolder)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Submit(context.Background(), JobInfo{Handle: "u2", ConvID: "c2", ModelID: "m"}, func(context.Context) error { return nil }) }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("next job after canceled one: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("next job did not run after the holder released")
	}
	<-doneHolder
}

// TestSchedulerLiveSlotCount changes a model's slot count while a job is
// queued and verifies Poke (called by the agent after a config edit) starts
// the queued job immediately — no restart, no waiting for a release.
func TestSchedulerLiveSlotCount(t *testing.T) {
	var slotsFn atomic.Int32
	slotsFn.Store(1)
	s := NewScheduler(func(string) int { return int(slotsFn.Load()) })
	gate := make(chan struct{})
	var started int32
	var wg sync.WaitGroup
	launch := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "c", ModelID: "m"}, func(ctx context.Context) error {
				atomic.AddInt32(&started, 1)
				<-gate
				return nil
			})
		}()
	}
	launch() // takes the single slot
	launch() // queues
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&started); got != 1 {
		t.Fatalf("started = %d, want 1 (1 slot, one job queued)", got)
	}
	slotsFn.Store(2) // the admin adds a slot mid-session
	s.Poke("m")      // the agent pokes the pool after the config edit
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&started) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&started); got != 2 {
		t.Fatalf("started = %d, want 2 (Poke started the queued job on the new slot)", got)
	}
	close(gate)
	wg.Wait()
}

// TestSchedulerLoadReportsQueue verifies the admin-facing snapshot shows the
// busy slots + the waiting jobs (with identity + wait time).
func TestSchedulerLoadReportsQueue(t *testing.T) {
	s := NewScheduler(func(string) int { return 1 })
	holder := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = s.Submit(context.Background(), JobInfo{Handle: "admin", ConvID: "conv-1", ModelID: "m"}, func(context.Context) error {
			close(holder)
			<-release
			return nil
		})
	}()
	<-holder
	go func() {
		_ = s.Submit(context.Background(), JobInfo{Handle: "carol", ConvID: "conv-2", ModelID: "m"}, func(context.Context) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	l := s.Load()["m"]
	if l.Busy != 1 || len(l.Waiting) != 1 {
		t.Fatalf("load = %+v, want busy=1, 1 waiting", l)
	}
	w := l.Waiting[0]
	if w.Handle != "carol" || w.ConvID != "conv-2" || w.ModelID != "m" {
		t.Fatalf("waiting job = %+v, want carol/conv-2/m", w)
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	if l := s.Load()["m"]; l.Busy != 0 || len(l.Waiting) != 0 {
		t.Fatalf("after drain: %+v, want idle", l)
	}
}
