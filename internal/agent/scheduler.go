// Per-model execution-slot pools (the execution model, tier 1 + the tier 2
// cooperative yield).
//
// Every main model has its own pool of execution slots (its Slots config,
// minimum 1). A conversation turn is a unit of work: it is queued into the
// pool of its (the conversation's active) model and holds one slot for
// exactly the duration of the turn, then releases it — it never holds a slot
// while waiting for the user's next message. Aux perception requests are the
// same unit of work in a pool of their own: an aux model's perception requests
// submit to its pool id (config.AssignAuxPoolIDs) — its OWN pool (the aux's
// Slots, minimum 1) unless the aux EXACTLY matches an existing main model
// (same provider + base URL + model id), in which case they reuse THAT MAIN
// MODEL'S pool, so perception requests and that model's conversations share
// one execution bound. If there are more concurrent jobs on a pool than
// slots in it, the extras wait FIFO. There is NO server-wide cap on total
// concurrency; the per-model sum is the only bound.
//
// Tier 2 (cooperative yield, 2026-11-27): a RUNNING job may yield its slot
// between steps — the conversation turn does this after each completed tool
// batch (its transcript is durable through the batch) when other work is
// waiting in the pool. Yield releases the slot, re-queues the SAME job at
// the TAIL of the pool's queue (full FIFO), and parks the job's goroutine
// until a slot is granted again; the turn resumes where it left off (the
// loop state is the goroutine's locals, which parking preserves). The
// scheduler still sees plain jobs: a yielded job is a waiting job in the
// admin dashboard. A yielded job is EXEMPT from the frozen-user deferral:
// freezing gates NEW work (a frozen user's queued turns wait), never work
// already in flight — a turn that crosses the disk cap mid-run finishes
// (it re-acquires its slot at the next yield) exactly as it did before the
// yield existed. See the design log's TIER 2 section.
package agent

import (
	"context"
	"sync"
	"time"
)

// JobInfo identifies a queued job, for the admin dashboard.
type JobInfo struct {
	Handle  string // the user's handle
	ConvID  string // the conversation id
	ModelID string // the pool (the conversation's active model)
}

// QueuedJob is one waiting job in a pool (admin dashboard).
type QueuedJob struct {
	Handle   string `json:"handle"`
	ConvID   string `json:"conversation_id"`
	ModelID  string `json:"model_id"`
	WaitSecs int    `json:"wait_secs"`
}

// PoolLoad is the live load of one model's pool (admin dashboard).
type PoolLoad struct {
	Busy    int         `json:"busy"`
	Waiting []QueuedJob `json:"waiting"`
}

// Scheduler queues units of work into per-model slot pools and runs each one
// when a slot in its pool is free. Jobs within a pool run FIFO; a job holds
// its slot for the full duration of its run function.
type Scheduler struct {
	mu    sync.Mutex
	pools map[string]*pool
	// slotsFor returns a model's current slot count from live config (the
	// admin can change it while running), never below 1.
	slotsFor func(modelID string) int
	// frozen reports whether a user is over their disk cap (their queued work
	// is deferred until they're back under it). Nil-safe (a nil callback
	// defers no one) — installed by the host's disk monitor.
	frozen func(handle string) bool
}

type pool struct {
	busy  int
	queue []*job
}

type job struct {
	info     JobInfo
	run      func(context.Context) error
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool          // granted a slot, or canceled while queued — exactly one handling per queue epoch
	waitCh   chan struct{} // closed when a slot is granted; fresh per queue epoch (a yielded job gets a new one)
	held     bool          // currently holding a slot (granted and not yet released)
	yielded  bool          // has yielded at least once (exempt from the frozen deferral — in-flight work, see the package doc)
	queuedAt time.Time
}

// NewScheduler builds a scheduler. slotsFor must be non-nil.
func NewScheduler(slotsFor func(modelID string) int) *Scheduler {
	return &Scheduler{pools: map[string]*pool{}, slotsFor: slotsFor}
}

func (s *Scheduler) poolFor(modelID string) *pool {
	q, ok := s.pools[modelID]
	if !ok {
		q = &pool{}
		s.pools[modelID] = q
	}
	return q
}

// Submit queues a job into the pool of info.ModelID and blocks until the job
// completes (or ctx is canceled), returning the job's error. If a slot is free
// the job runs immediately. While waiting in the queue, canceling ctx removes
// the job from the queue; if a slot is granted at the same moment, the slot is
// returned without running. The job's context carries its cooperative-yield
// function (YieldFromContext) — see jobYielder.
func (s *Scheduler) Submit(ctx context.Context, info JobInfo, run func(context.Context) error) error {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	j := &job{info: info, run: run, ctx: jobCtx, cancel: cancel, waitCh: make(chan struct{}), queuedAt: time.Now()}
	runCtx := context.WithValue(jobCtx, yieldKey{}, &jobYielder{s: s, modelID: info.ModelID, j: j})
	s.mu.Lock()
	q := s.poolFor(info.ModelID)
	if q.busy < s.slotsFor(info.ModelID) && !s.isFrozen(info.Handle) {
		// A slot is free AND the user is not frozen: take it now.
		q.busy++
		j.held = true
		s.mu.Unlock()
		return s.runJob(info.ModelID, j, runCtx)
	}
	// The pool is full OR the user is frozen (over their disk cap — their
	// work stays queued until they're back under it): wait in the FIFO queue.
	q.queue = append(q.queue, j)
	s.mu.Unlock()
	select {
	case <-j.waitCh:
		// A slot was granted; run holding it.
		return s.runJob(info.ModelID, j, runCtx)
	case <-jobCtx.Done():
		return s.dropJob(info.ModelID, j)
	}
}

// runJob runs the job's function holding its slot, then releases the slot —
// unless the job yielded it and never got it back (canceled while parked:
// the yield already accounted for the slot; releasing again would
// double-decrement the pool's busy count).
func (s *Scheduler) runJob(modelID string, j *job, runCtx context.Context) error {
	err := j.run(runCtx)
	s.mu.Lock()
	if j.held {
		j.held = false
		s.pools[modelID].busy--
		s.startNextLocked(modelID)
	}
	s.mu.Unlock()
	return err
}

// dropJob is the canceled path: the job never ran (it was queued, or it was
// parked in the queue after a yield). If a slot was granted at the same
// moment as the cancel, the slot is returned without running so the next
// queued job can take it. Returns the job's context error.
func (s *Scheduler) dropJob(modelID string, j *job) error {
	s.mu.Lock()
	q := s.pools[modelID]
	if j.held {
		j.held = false
		q.busy--
		s.startNextLocked(modelID)
	} else {
		j.started = true
		for i, w := range q.queue {
			if w == j {
				q.queue = append(q.queue[:i], q.queue[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	return j.ctx.Err()
}

// startNextLocked starts queued jobs while slots are free. Callers hold s.mu.
// A frozen user's job is DEFERRED (re-queued to the end) rather than granted a
// slot — so their (rare) queued turn doesn't starve the pool's other users —
// EXCEPT a yielded job: that is in-flight work (it already ran on the slot),
// and freezing gates new work, never work in flight (the package doc). One
// full pass of deferrals caps the loop so an all-frozen queue can't spin.
func (s *Scheduler) startNextLocked(modelID string) {
	q := s.pools[modelID]
	initial := len(q.queue)
	deferred := 0
	for q.busy < s.slotsFor(modelID) && len(q.queue) > 0 {
		j := q.queue[0]
		q.queue = q.queue[1:]
		if j.started {
			continue // canceled while waiting
		}
		if s.isFrozen(j.info.Handle) && !j.yielded {
			// Defer the frozen job to the end (keep it queued until the user
			// is back under the cap). Cap deferrals to one full pass.
			if deferred >= initial {
				q.queue = append(q.queue, j) // all-frozen: put it back, stop
				break
			}
			q.queue = append(q.queue, j)
			deferred++
			continue
		}
		j.started = true
		j.held = true
		q.busy++
		close(j.waitCh)
	}
}

// Poke re-dispatches a model's queue after a config change (the admin raised
// the slot count, so capacity may have freed up). It is a no-op when the model
// has no pool yet or nothing is queued.
func (s *Scheduler) Poke(modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pools[modelID]; !ok {
		return
	}
	s.startNextLocked(modelID)
}

// PokeAll re-dispatches every pool (e.g. when a user unfreezes: their deferred
// queued work can now run in any pool it's queued in). A no-op for pools with
// nothing queued.
func (s *Scheduler) PokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.pools {
		s.startNextLocked(id)
	}
}

// isFrozen reports whether a user is over their disk cap (their queued work is
// deferred until they're back under it). Callers hold s.mu. Nil-safe.
func (s *Scheduler) isFrozen(handle string) bool {
	return s.frozen != nil && s.frozen(handle)
}

// SetFrozen installs the disk-cap callback (a user over their cap has their
// queued work deferred). A nil callback defers no one. Installed by the host's
// disk monitor; safe to call any time (read under s.mu on each dispatch).
func (s *Scheduler) SetFrozen(f func(handle string) bool) {
	s.mu.Lock()
	s.frozen = f
	s.mu.Unlock()
}

// Frozen reports whether the user is over their disk cap (the installed
// check; false when none is installed).
func (s *Scheduler) Frozen(handle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen == nil {
		return false
	}
	return s.frozen(handle)
}

// QueueLen reports how many jobs are waiting in a model's pool (non-blocking).
// A turn's yield point reads it: non-empty = yield the slot so the waiting
// work runs; empty = continue (yielding into an empty queue would just
// bounce the slot back).
func (s *Scheduler) QueueLen(modelID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q, ok := s.pools[modelID]; ok {
		return len(q.queue)
	}
	return 0
}

type yieldKey struct{}

// jobYielder is a running job's cooperative slot yield (tier 2). The
// Scheduler attaches one to every job's context; runs that never yield never
// see it (YieldFromContext returns nil outside a job context).
type jobYielder struct {
	s       *Scheduler
	modelID string
	j       *job
}

// Yield releases the job's slot, re-queues the job at the TAIL of its pool's
// queue (full FIFO — behind everything currently waiting), and blocks until
// a slot is granted again. It returns the job's context error if the job is
// canceled while parked (the job is dropped from the queue, and any slot
// granted at the cancel moment is returned without the job resuming). The
// job's goroutine — and with it the turn's loop state — survives the park.
func (y *jobYielder) Yield() error {
	y.s.mu.Lock()
	q := y.s.pools[y.modelID]
	if !y.j.held {
		// Not holding a slot (a yield point reached after a cancel already
		// dropped the job): nothing to yield.
		y.s.mu.Unlock()
		return y.j.ctx.Err()
	}
	// Release the slot and re-queue this job at the tail.
	y.j.held = false
	y.j.yielded = true
	y.j.queuedAt = time.Now() // this wait starts now (dashboard wait time)
	y.j.waitCh = make(chan struct{})
	q.busy--
	q.queue = append(q.queue, y.j)
	y.s.startNextLocked(y.modelID)
	y.s.mu.Unlock()
	select {
	case <-y.j.waitCh:
		return nil
	case <-y.j.ctx.Done():
		y.s.mu.Lock()
		if y.j.held {
			// Granted at the cancel moment: return the slot without
			// resuming and let the next queued job take it.
			y.j.held = false
			q.busy--
			y.s.startNextLocked(y.modelID)
		} else {
			y.j.started = true
			for i, w := range q.queue {
				if w == y.j {
					q.queue = append(q.queue[:i], q.queue[i+1:]...)
					break
				}
			}
		}
		y.s.mu.Unlock()
		return y.j.ctx.Err()
	}
}

// YieldFromContext returns the running job's yield function (the tier 2
// cooperative yield) — nil when the context is not a scheduler job context
// (direct calls, tests).
func YieldFromContext(ctx context.Context) func() error {
	y, _ := ctx.Value(yieldKey{}).(*jobYielder)
	if y == nil {
		return nil
	}
	return y.Yield
}

// Load returns the live load of every pool that has seen work (busy slots +
// waiting jobs). It never blocks. A yielded (parked) job shows up as a
// waiting job — its wait time restarted at the yield.
func (s *Scheduler) Load() map[string]PoolLoad {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]PoolLoad, len(s.pools))
	now := time.Now()
	for id, q := range s.pools {
		wait := []QueuedJob{}
		for _, j := range q.queue {
			if j.started {
				continue
			}
			wait = append(wait, QueuedJob{j.info.Handle, j.info.ConvID, j.info.ModelID, int(now.Sub(j.queuedAt).Seconds())})
		}
		out[id] = PoolLoad{Busy: q.busy, Waiting: wait}
	}
	return out
}
