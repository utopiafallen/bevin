package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
)

// TestSchedulerYieldReleasesSlotAndResumes verifies the tier-2 yield
// mechanism itself: on a 1-slot pool, job A yields its slot, the queued job B
// runs to completion in the MEANTIME (the slot actually freed), and A resumes
// on the re-granted slot and completes. The pool is empty again at the end
// (the release accounting survived the park).
func TestSchedulerYieldReleasesSlotAndResumes(t *testing.T) {
	s := NewScheduler(func(string) int { return 1 })
	aEntered, proceedA := make(chan struct{}), make(chan struct{})
	bStarted := make(chan struct{})
	errCh := make(chan error, 2)

	go func() {
		errCh <- s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "c1", ModelID: "m"}, func(ctx context.Context) error {
			yield := YieldFromContext(ctx)
			if yield == nil {
				return errors.New("no yield function on the job context")
			}
			close(aEntered)
			<-proceedA     // yield only once B is queued (an empty-queue yield just bounces)
			return yield() // park until the slot comes back
		})
	}()
	<-aEntered
	go func() {
		errCh <- s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "c2", ModelID: "m"}, func(ctx context.Context) error {
			close(bStarted)
			return nil
		})
	}()
	// Wait until B is actually queued behind A.
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Load()["m"].Waiting) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.Load()["m"].Waiting) < 1 {
		t.Fatal("B never queued behind A")
	}
	close(proceedA)
	select {
	case <-bStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("B never ran while A was parked — the yield did not release the slot")
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("job error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a job never finished")
		}
	}
	if load := s.Load()["m"]; load.Busy != 0 || len(load.Waiting) != 0 {
		t.Fatalf("pool not empty after both jobs: %+v (busy count leaked through the park)", load)
	}
}

// TestSchedulerYieldRequeuesAtTail verifies the FIFO policy: a yielding job
// re-queues at the TAIL, behind everything already waiting — B and C run
// before A resumes.
func TestSchedulerYieldRequeuesAtTail(t *testing.T) {
	s := NewScheduler(func(string) int { return 1 })
	order := make(chan string, 8)
	aEntered, proceedA := make(chan struct{}), make(chan struct{})

	run := func(id string, yield func(context.Context) error, wait <-chan struct{}) func(context.Context) error {
		return func(ctx context.Context) error {
			order <- id
			if wait != nil {
				<-wait
			}
			if yield != nil {
				return yield(ctx)
			}
			return nil
		}
	}
	// A runs, parks at a gate, then yields (only after B + C are queued).
	go func() {
		if err := s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "c1", ModelID: "m"},
			run("A", func(ctx context.Context) error {
				close(aEntered)
				<-proceedA
				if err := YieldFromContext(ctx)(); err != nil {
					return err
				}
				order <- "A" // resumed
				return nil
			}, nil)); err != nil {
			t.Errorf("A: %v", err)
		}
	}()
	<-aEntered
	// Queue B, then C — serializing the ENQUEUE order (goroutine start order
	// alone would let C race ahead of B in the queue). Each Submit goroutine
	// parks in the queue; the Load() poll confirms it is in before the next
	// one is launched.
	launch := func(id string) {
		go func() {
			if err := s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: id, ModelID: "m"}, run(id, nil, nil)); err != nil {
				t.Errorf("%s: %v", id, err)
			}
		}()
	}
	waitQueued := func(n int) {
		deadline := time.Now().Add(5 * time.Second)
		for len(s.Load()["m"].Waiting) < n && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(s.Load()["m"].Waiting) < n {
			t.Fatalf("only %d jobs queued, want %d", len(s.Load()["m"].Waiting), n)
		}
	}
	launch("B")
	waitQueued(1)
	launch("C")
	waitQueued(2)
	close(proceedA)
	var got []string
	for i := 0; i < 4; i++ {
		select {
		case v := <-order:
			got = append(got, v)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for run order: got %v", got)
		}
	}
	want := []string{"A", "B", "C", "A"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("run order = %v, want %v (a yielded job re-queues at the tail)", got, want)
		}
	}
}

// TestSchedulerYieldCancelWhileParked verifies the cancel path: a job parked
// after a yield is dropped from the queue when its context is canceled, the
// slot is freed for the next job, and the pool ends empty (no double release).
func TestSchedulerYieldCancelWhileParked(t *testing.T) {
	s := NewScheduler(func(string) int { return 1 })
	ctxA, cancelA := context.WithCancel(context.Background())
	aEntered, proceedA := make(chan struct{}), make(chan struct{})
	bDone := make(chan struct{})
	bRelease := make(chan struct{})
	errA := make(chan error, 1)
	errB := make(chan error, 1)

	go func() {
		errA <- s.Submit(ctxA, JobInfo{Handle: "u", ConvID: "c1", ModelID: "m"}, func(ctx context.Context) error {
			close(aEntered)
			<-proceedA                     // yield only once B is queued (an empty-queue yield just bounces)
			return YieldFromContext(ctx)() // parks behind B
		})
	}()
	<-aEntered
	go func() {
		errB <- s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "c2", ModelID: "m"}, func(ctx context.Context) error {
			<-bRelease
			close(bDone)
			return nil
		})
	}()
	// Wait for B to be queued, then let A yield so it parks behind B.
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Load()["m"].Waiting) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(proceedA)
	// Wait for B to hold the slot (A is parked behind it).
	for s.Load()["m"].Busy != 1 && len(s.Load()["m"].Waiting) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancelA()
	select {
	case err := <-errA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled park: err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("A never returned after the cancel")
	}
	close(bRelease)
	<-bDone
	<-errB
	if load := s.Load()["m"]; load.Busy != 0 || len(load.Waiting) != 0 {
		t.Fatalf("pool not empty after the cancel: %+v (the parked job leaked a slot or stayed queued)", load)
	}
}

// TestSchedulerYieldFrozenExempt verifies the frozen-user exemption: a turn
// that yields while its user is frozen (they crossed the disk cap mid-run)
// re-acquires its slot — in-flight work is never frozen — while the user's
// NEW work stays deferred until they unfreeze.
func TestSchedulerYieldFrozenExempt(t *testing.T) {
	s := NewScheduler(func(string) int { return 1 })
	var frozen int32
	s.SetFrozen(func(string) bool { return atomic.LoadInt32(&frozen) == 1 })
	aEntered, proceedA := make(chan struct{}), make(chan struct{})
	bDone := make(chan struct{})
	aOrder := make(chan string, 2)
	bRelease := make(chan struct{})

	go func() {
		if err := s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "c1", ModelID: "m"}, func(ctx context.Context) error {
			aOrder <- "A"
			close(aEntered)
			<-proceedA
			if err := YieldFromContext(ctx)(); err != nil {
				return err
			}
			aOrder <- "A2"
			return nil
		}); err != nil {
			t.Errorf("A: %v", err)
		}
	}()
	<-aEntered
	go func() {
		if err := s.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "c2", ModelID: "m"}, func(ctx context.Context) error {
			<-bRelease
			close(bDone)
			return nil
		}); err != nil {
			t.Errorf("B: %v", err)
		}
	}()
	// Wait for B to actually queue behind A, THEN freeze the user while A is
	// in flight and let A yield. A must re-acquire the slot (the exemption)
	// even though B (new work, same user) is deferred by the frozen check.
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Load()["m"].Waiting) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.Load()["m"].Waiting) < 1 {
		t.Fatal("B never queued behind A")
	}
	atomic.StoreInt32(&frozen, 1)
	close(proceedA)
	select {
	case <-aOrder: // A resumes
	case <-time.After(5 * time.Second):
		t.Fatal("A never resumed — a yielded (in-flight) job must not be deferred by the frozen check")
	}
	// B is still waiting (frozen, not yielded).
	if load := s.Load()["m"]; len(load.Waiting) != 1 {
		t.Fatalf("B should still be waiting while frozen: %+v", load)
	}
	atomic.StoreInt32(&frozen, 0)
	s.Poke("m")
	close(bRelease) // let B (unfrozen now) finish
	<-bDone
	<-aOrder // A2
	if load := s.Load()["m"]; load.Busy != 0 || len(load.Waiting) != 0 {
		t.Fatalf("pool not empty at the end: %+v", load)
	}
}

// TestTurnYieldsSlotBetweenToolBatches is the end-to-end pin for tier 2: a
// 1-slot pool, a long turn (a tool batch) and a queued second turn on the
// same model — after the first turn's tool batch settles, it YIELDS its slot
// and the second turn's model call runs in the meantime; the first turn
// resumes only after the slot comes back. The model-call order (by
// conversation id on the wire) proves the interleave: T1-call-1, T2-call-1,
// T1-call-2.
func TestTurnYieldsSlotBetweenToolBatches(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// m1 has a single slot (newTestAgent's default).
	var gateMu sync.Mutex
	gateEntered := false
	entered := make(chan struct{})
	block := make(chan struct{})
	if err := a.tools.Register(Tool{
		Name:        "gate",
		Description: "test gate tool",
		Parameters:  map[string]any{},
		Execute: func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
			gateMu.Lock()
			if !gateEntered {
				gateEntered = true
				gateMu.Unlock()
				close(entered)
			} else {
				gateMu.Unlock()
			}
			<-block
			return "ok", nil
		},
	}); err != nil {
		t.Fatalf("Register gate: %v", err)
	}
	fp := &fakeProvider{
		replies:   []string{"r1", "r2", "r3"},
		toolCalls: []provider.ToolCall{{Name: "gate", Arguments: "{}"}},
	}
	a.providers["m1"] = fp

	ctx := context.Background()
	conv1, err := a.EnsureConversation(ctx, "u1", "", "t1")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	conv2, err := a.EnsureConversation(ctx, "u1", "", "t2")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	done1, done2 := make(chan struct{}), make(chan struct{})
	errCh := make(chan error, 2)
	go func() {
		defer close(done1)
		_, _, e := a.Turn(ctx, "u1", conv1, "hi", nil)
		errCh <- e
	}()
	<-entered // T1 is executing its tool batch (holding m1's only slot)
	go func() {
		defer close(done2)
		_, _, e := a.Turn(ctx, "u1", conv2, "hi", nil)
		errCh <- e
	}()
	// Wait until T2 is actually queued behind T1 (the yield fires on a
	// non-empty queue).
	deadline := time.Now().Add(5 * time.Second)
	for len(a.sched.Load()["m1"].Waiting) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(a.sched.Load()["m1"].Waiting) < 1 {
		t.Fatal("T2 never queued behind T1")
	}
	close(block) // T1's batch settles -> yield point: the slot goes to T2.
	<-done1
	<-done2
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("turn error: %v", err)
		}
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.calls) != 3 {
		t.Fatalf("model calls = %d, want 3 (T1 call 1, T2 call 1, T1 call 2)", len(fp.calls))
	}
	if fp.calls[0].ConversationID != conv1 {
		t.Fatalf("call 1 = conversation %q, want %q (T1's first call)", fp.calls[0].ConversationID, conv1)
	}
	if fp.calls[1].ConversationID != conv2 {
		t.Fatalf("call 2 = conversation %q, want %q (T2's call ran in T1's yield window)", fp.calls[1].ConversationID, conv2)
	}
	if fp.calls[2].ConversationID != conv1 {
		t.Fatalf("call 3 = conversation %q, want %q (T1 resumed after the slot came back)", fp.calls[2].ConversationID, conv1)
	}
}

// TestTurnNoYieldOnEmptyQueue pins the yield policy's other branch: with an
// empty pool queue the turn does NOT yield between tool batches (yielding
// into an empty queue would just bounce the slot back). The model calls of a
// tool-looping turn are contiguous — no second conversation can interleave.
func TestTurnNoYieldOnEmptyQueue(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	var mu sync.Mutex
	block := make(chan struct{})
	if err := a.tools.Register(Tool{
		Name:        "gate",
		Description: "test gate tool",
		Parameters:  map[string]any{},
		Execute: func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			<-block
			return "ok", nil
		},
	}); err != nil {
		t.Fatalf("Register gate: %v", err)
	}
	fp := &fakeProvider{
		replies:   []string{"r1", "r2"},
		toolCalls: []provider.ToolCall{{Name: "gate", Arguments: "{}"}},
	}
	a.providers["m1"] = fp
	ctx := context.Background()
	conv, err := a.EnsureConversation(ctx, "u1", "", "t1")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	var released int32
	go func() {
		// While T1's batch is in flight, the pool's queue must stay EMPTY —
		// T1 holds the slot and nothing is waiting, so it never yields and
		// never releases it.
		time.Sleep(50 * time.Millisecond)
		atomic.StoreInt32(&released, 1)
		close(block)
	}()
	if _, _, err := a.Turn(ctx, "u1", conv, "hi", nil); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if atomic.LoadInt32(&released) != 1 {
		t.Fatal("block never released")
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	// Both model calls belong to the one conversation (no interleave — and
	// the second happened while the queue was empty, i.e. WITHOUT a yield).
	if len(fp.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(fp.calls))
	}
	for i, c := range fp.calls {
		if c.ConversationID != conv {
			t.Fatalf("call %d = conversation %q, want %q", i, c.ConversationID, conv)
		}
	}
}
