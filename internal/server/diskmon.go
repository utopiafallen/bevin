package server

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

// diskScanInterval is how often the DiskMonitor re-walks every user's
// storage. It is a background hygiene check, NOT a hard real-time limit —
// a user can exceed the cap by up to one interval of work before the freeze
// catches it. The in-flight turn always finishes; the NEXT turn (or the next
// upload/workspace create) is the one that gets blocked.
const diskScanInterval = 45 * time.Second

// diskReleaseFraction is the hysteresis: a frozen user is released only once
// usage drops back below this fraction of the cap, so a user hovering right
// at the cap does not flap frozen/unfrozen every scan.
const diskReleaseFraction = 0.9

// diskStatus is one user's disk usage + frozen state, for the disk page.
type diskStatus struct {
	Used     int64 `json:"used_bytes"`
	Limit    int64 `json:"limit_bytes"` // 0 = no cap
	Baseline int64 `json:"baseline_bytes"`
	Frozen   bool  `json:"frozen"`
}

// userDisk is the cached per-user state (usage + frozen, with hysteresis).
type userDisk struct {
	used     int64
	baseline int64
	frozen   bool
}

// diskMonitor tracks per-user DISK usage (the user's data dir, counted in
// full, plus the container-storage GROWTH over its creation-time baseline)
// against the per-user cap, and a frozen state with hysteresis.
//
// It is a HOST-SIDE BACKGROUND WALKER: it never runs in the request path.
// The gate + the scheduler read the CACHED frozen state (cheap); the disk
// page and the cleanup "done" button trigger an immediate re-check (a full
// walk). The cap is read LIVE from config each scan, so an admin edit
// applies from the next scan without a restart.
type diskMonitor struct {
	dataDir string
	capMB   func() int             // live per-user disk cap in MiB (0 = no cap)
	users   func() ([]string, bool) // live user handles (ok=false on a transient store error)
	// onUnfreeze fires (outside the monitor's lock) when a user transitions
	// frozen→unfrozen — the host re-dispatches the scheduler (their deferred
	// queued work can now run). Nil-safe.
	onUnfreeze func(handle string)
	mu         sync.RWMutex
	st         map[string]*userDisk
}

func newDiskMonitor(dataDir string, capMB func() int, users func() ([]string, bool)) *diskMonitor {
	return &diskMonitor{dataDir: dataDir, capMB: capMB, users: users, st: make(map[string]*userDisk)}
}

// SetOnUnfreeze installs the callback fired (outside the monitor's lock) when
// a user transitions frozen→unfrozen — the host re-dispatches the scheduler
// (the user's deferred queued work can now run). Nil-safe.
func (m *diskMonitor) SetOnUnfreeze(f func(handle string)) {
	m.mu.Lock()
	m.onUnfreeze = f
	m.mu.Unlock()
}

// Start runs the periodic walk in a goroutine until ctx is cancelled.
func (m *diskMonitor) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(diskScanInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.scan()
			}
		}
	}()
}

// Prime does a synchronous initial scan (called at boot before serving, so
// the gate has data for existing users on the very first request).
func (m *diskMonitor) Prime() { m.scan() }

// scan re-walks every user's storage and applies the hysteresis. A transient
// user-list error keeps the previous state (it must not clear frozen users).
func (m *diskMonitor) scan() {
	handles, ok := m.users()
	if !ok {
		return
	}
	limit := int64(m.capMB()) * 1024 * 1024
	live := make(map[string]bool, len(handles))
	for _, h := range handles {
		live[h] = true
	}
	var unfroze []string
	m.mu.Lock()
	for _, h := range handles {
		if m.updateLocked(h, limit) {
			unfroze = append(unfroze, h)
		}
	}
	for h := range m.st {
		if !live[h] { // an admin delete — drop the stale row
			delete(m.st, h)
		}
	}
	m.mu.Unlock()
	for _, h := range unfroze {
		m.notifyUnfreeze(h)
	}
}

// notifyUnfreeze fires the onUnfreeze callback (outside the monitor's lock, so
// it may re-dispatch the scheduler without a lock-ordering concern). The
// callback is read under the RLock (no race with SetOnUnfreeze) and called
// with the lock released. Nil-safe.
func (m *diskMonitor) notifyUnfreeze(h string) {
	m.mu.RLock()
	f := m.onUnfreeze
	m.mu.RUnlock()
	if f != nil {
		f(h)
	}
}

// updateLocked recomputes one user's usage and applies the hysteresis. The
// caller holds m.mu (write). It reports whether the user transitioned
// frozen→unfrozen (so the caller can re-dispatch the scheduler after releasing
// the lock).
func (m *diskMonitor) updateLocked(h string, limit int64) (unfroze bool) {
	ud := m.st[h]
	if ud == nil {
		ud = &userDisk{}
		m.st[h] = ud
	}
	ud.used = m.usage(h)
	ud.baseline = sandbox.ContainerBaseBytes(m.dataDir, h)
	switch {
	case limit <= 0:
		if ud.frozen {
			ud.frozen = false // the cap was removed (set to 0) → release
			return true
		}
		return false
	case !ud.frozen && ud.used > limit:
		ud.frozen = true // crossed the cap → freeze (the next turn is blocked)
		return false
	case ud.frozen && ud.used < int64(float64(limit)*diskReleaseFraction):
		ud.frozen = false // back under 90% → release
		return true
	}
	// otherwise keep the current state (the hysteresis band)
	return false
}

// usage = the user's data dir (counted in full) + the container-storage
// growth over its creation-time baseline. The base OS image (the linux rootfs
// copy) / the empty rwlayer do not count — only what the user added does.
func (m *diskMonitor) usage(h string) int64 {
	data := sandbox.DirSize(sandbox.UserDataDir(m.dataDir, h))
	storage := sandbox.DirSize(sandbox.UserSandboxDir(m.dataDir, h))
	growth := storage - sandbox.ContainerBaseBytes(m.dataDir, h)
	if growth < 0 {
		growth = 0
	}
	return data + growth
}

// Frozen reports whether the user is currently over their disk cap. It reads
// the CACHED state (no walk) — the gate calls it on every request, so it
// must stay cheap. A user not yet scanned (brand-new, before the first tick)
// is not frozen: a new user starts at ~0 usage and the next scan picks them
// up.
func (m *diskMonitor) Frozen(h string) bool {
	if m.capMB() == 0 {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	ud := m.st[h]
	return ud != nil && ud.frozen
}

// Recheck immediately re-walks one user's storage and applies the hysteresis
// (a FULL walk — for the disk page + the cleanup "done" button, never the hot
// path). It returns the fresh status.
func (m *diskMonitor) Recheck(h string) diskStatus {
	limit := int64(m.capMB()) * 1024 * 1024
	var unfroze bool
	m.mu.Lock()
	unfroze = m.updateLocked(h, limit)
	m.mu.Unlock()
	if unfroze {
		m.notifyUnfreeze(h)
	}
	return m.Status(h)
}

// Status returns the user's cached disk usage + frozen state + cap (for the
// disk page). Call Recheck first for fresh numbers.
func (m *diskMonitor) Status(h string) diskStatus {
	limit := int64(m.capMB()) * 1024 * 1024
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := diskStatus{Limit: limit}
	if ud := m.st[h]; ud != nil {
		s.Used = ud.used
		s.Baseline = ud.baseline
		s.Frozen = ud.frozen
	}
	return s
}

// userHandles lists the user handles for the disk monitor. ok=false on a
// transient store error (the monitor then keeps its previous state rather
// than clearing frozen users).
func (a *App) userHandles() ([]string, bool) {
	users, err := a.srv.ListUsers()
	if err != nil {
		log.Printf("bevin: disk monitor: listing users: %v", err)
		return nil, false
	}
	handles := make([]string, 0, len(users))
	for _, u := range users {
		handles = append(handles, u.Handle)
	}
	return handles, true
}
