// Per-model execution-slot pools (the execution model, tier 1).
//
// Every main model has its own pool of execution slots (its Slots config,
// minimum 1). A conversation turn is a unit of work: it is queued into the
// pool of its (the conversation's active) model and holds one slot for exactly
// the duration of the turn, then releases it — it never holds a slot while
// waiting for the user's next message. If there are more concurrent turns on a
// model than slots in its pool, the extras wait FIFO. There is NO server-wide
// cap on total concurrency; the per-model sum is the only bound.
//
// This implements tier 1 of the execution model (interactive user
// conversations). Tier 2 (autonomous task conversations, preemption,
// dependency gating) builds on the same pools in a later revision.
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
	started  bool // a slot was granted, or it was canceled while queued — exactly one handling
	start    chan struct{} // closed when a slot is granted
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
// returned without running.
func (s *Scheduler) Submit(ctx context.Context, info JobInfo, run func(context.Context) error) error {
	jobCtx, cancel := context.WithCancel(ctx)
	j := &job{info: info, run: run, ctx: jobCtx, cancel: cancel, start: make(chan struct{}), queuedAt: time.Now()}
	s.mu.Lock()
	q := s.poolFor(info.ModelID)
	if q.busy < s.slotsFor(info.ModelID) {
		// A slot is free: take it now.
		q.busy++
		s.mu.Unlock()
		defer s.release(info.ModelID)
		defer cancel()
		return run(jobCtx)
	}
	// The pool is full: wait in the FIFO queue.
	q.queue = append(q.queue, j)
	s.mu.Unlock()
	select {
	case <-j.start:
		// A slot was granted; run holding it.
	case <-jobCtx.Done():
		s.mu.Lock()
		if !j.started {
			// Not yet granted: drop out of the queue.
			j.started = true
			for i, w := range q.queue {
				if w == j {
					q.queue = append(q.queue[:i], q.queue[i+1:]...)
					break
				}
			}
		} else {
			// Granted while we were canceling: return the slot without running
			// and let the next queued job take it.
			q.busy--
			s.startNextLocked(info.ModelID)
		}
		s.mu.Unlock()
		cancel()
		return jobCtx.Err()
	}
	defer s.release(info.ModelID)
	defer cancel()
	return run(jobCtx)
}

// release returns one slot and starts queued jobs while slots are free.
func (s *Scheduler) release(modelID string) {
	s.mu.Lock()
	q := s.poolFor(modelID)
	q.busy--
	s.startNextLocked(modelID)
	s.mu.Unlock()
}

// startNextLocked starts queued jobs while slots are free. Callers hold s.mu.
func (s *Scheduler) startNextLocked(modelID string) {
	q := s.pools[modelID]
	for q.busy < s.slotsFor(modelID) && len(q.queue) > 0 {
		j := q.queue[0]
		q.queue = q.queue[1:]
		if j.started {
			continue // canceled while waiting
		}
		j.started = true
		q.busy++
		close(j.start)
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

// Load returns the live load of every pool that has seen work (busy slots +
// waiting jobs). It never blocks.
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
