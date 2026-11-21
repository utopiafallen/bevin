package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// The settle parameters (named constants — the harness's stability model,
// not a per-deployment dial): a page is STABLE when nothing is in flight
// (the Network-domain counter) AND no DOM mutation has landed for the
// quiet window. The hard cap bounds a page that never quiets (a live
// dashboard, a hung request) — it then reports settled=false and the
// caller proceeds with a snapshot and says so.
const (
	settleQuiet = 300 * time.Millisecond
	settleCap   = 5 * time.Second
	settlePoll  = 100 * time.Millisecond
)

// settleProbeJS installs the mutation observer ONCE PER DOCUMENT (a fresh
// document has no window.__bevinSettle and reinstalls it — no document-
// identity plumbing needed) and reports {t: the last mutation's
// performance.now, now: the current performance.now}. The window global
// carries INSTRUMENTATION ONLY (a timestamp) — never page content (the
// stateless boundary, design log "LAZY-LIST TIER"). The observer watches
// childList + characterData (true content): attribute-only churn (class
// toggles, style updates) deliberately does NOT restart the quiet window
// — attribute animation would keep a settling page unsettled forever.
const settleProbeJS = `(() => {
  if (!window.__bevinSettle) {
    window.__bevinSettle = { t: performance.now() };
    new MutationObserver(() => { window.__bevinSettle.t = performance.now(); }).observe(
      document.documentElement,
      { subtree: true, childList: true, characterData: true }
    );
  }
  return JSON.stringify({ t: window.__bevinSettle.t, now: performance.now() });
})()`

// netCounter is one session's in-flight request count (CDP Network
// events: requestWillBeSent +1, loadingFinished/loadingFailed -1 — each
// start gets exactly one end; a request that never ends keeps its slot
// until the tab closes, which bounds the damage to the settle cap).
type netCounter struct {
	mu sync.Mutex
	in int
}

func (n *netCounter) inc() {
	n.mu.Lock()
	n.in++
	n.mu.Unlock()
}

func (n *netCounter) dec() {
	n.mu.Lock()
	if n.in > 0 {
		n.in--
	}
	n.mu.Unlock()
}

func (n *netCounter) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.in
}

// netCounterFor returns the session's counter, creating it on first use.
func (i *Instance) netCounterFor(session string) *netCounter {
	i.mu.Lock()
	defer i.mu.Unlock()
	c, ok := i.netState[session]
	if !ok {
		c = &netCounter{}
		i.netState[session] = c
	}
	return c
}

// handleNetEvent is the instance's CDP event sink (the flat client's
// first event consumer — see the cdp.go note): it routes the
// Network-domain events to the per-session counters. Events for unknown
// sessions (a tab closed mid-flight) and browser-level events are
// dropped.
func (i *Instance) handleNetEvent(sessionID, method string, _ json.RawMessage) {
	if sessionID == "" {
		return
	}
	i.mu.Lock()
	c, ok := i.netState[sessionID]
	i.mu.Unlock()
	if !ok {
		return
	}
	switch method {
	case "Network.requestWillBeSent":
		c.inc()
	case "Network.loadingFinished", "Network.loadingFailed":
		c.dec()
	}
}

// settleDecision is the pure stability rule (unit-tested): stable when
// nothing is in flight AND the last DOM mutation is at least quietMs old
// in PAGE time (now and lastMut are both performance.now readings — no
// cross-clock comparison).
func settleDecision(now, lastMut, quietMs float64, inFlight int) bool {
	return inFlight == 0 && (now-lastMut) >= quietMs
}

// Settle waits for the tab's page to be stable (see the settle* constants
// for the rule + cap). It installs the mutation observer (once per
// document) and polls the probe + the Network counter. Returns (settled,
// took); a cap hit is settled=false with took ≈ settleCap — NOT an error
// (a churning page is a state, not a failure).
func (i *Instance) Settle(ctx context.Context, tabID string) (bool, time.Duration, error) {
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return false, 0, err
	}
	c := i.netCounterFor(session)
	start := time.Now()
	deadline := start.Add(settleCap)
	for {
		body, err := i.evaluateString(ctx, session, settleProbeJS)
		if err != nil {
			return false, time.Since(start), err
		}
		var p struct {
			T   float64 `json:"t"`
			Now float64 `json:"now"`
		}
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			return false, time.Since(start), fmt.Errorf("parsing the settle probe: %w", err)
		}
		if settleDecision(p.Now, p.T, float64(settleQuiet.Milliseconds()), c.count()) {
			return true, time.Since(start), nil
		}
		if time.Now().After(deadline) {
			return false, time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return false, time.Since(start), ctx.Err()
		case <-time.After(settlePoll):
		}
	}
}

// settleWord is the one-line settle state for the read/item/scroll
// reports (shared by FormatRead, BoundItems, and ScrollReport).
func settleWord(settled bool, took time.Duration) string {
	if settled {
		return fmt.Sprintf("settled in %s", took.Round(time.Millisecond))
	}
	return fmt.Sprintf("still settling after %s — content is a snapshot", settleCap)
}
