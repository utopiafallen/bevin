package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// The settle parameters (named constants — the harness's stability model,
// not a per-deployment dial). The READY INDICATOR is page-derived, not
// time-derived: a page is stable when nothing is in flight (the
// Network-domain counter) AND no DOM mutation has landed for the quiet
// window. The two ABANDON conditions replace the old 5s-from-start hard
// cap (removed 2026-11-23 — it cut off pages still mid-load, which is why
// 17 of 21 reads in the first research session were partial snapshots):
// neither fires while the page is still progressing.
//
//	settleStallQuiet: the DOM has been completely quiet this long while
//		the network is still busy (a stalled/slow request, an
//		SSE/WebSocket) — the content is what it's going to be; waiting
//		longer cannot render more of it.
//	settleChurnCap: the absolute backstop, only reachable by a page that
//		NEVER stops mutating (live dashboards, rotating ad creatives) —
//		it reports a snapshot and says so.
const (
	settleQuiet      = 300 * time.Millisecond
	settleStallQuiet = 10 * time.Second
	settleChurnCap   = 15 * time.Second
	settlePoll       = 100 * time.Millisecond
)

// SettleState is the outcome of a settle wait. Three final states, three
// wordings — the tool never leaves the model guessing what it got:
//
//	SettleSettled  the ready indicator fired (network idle + DOM quiet) —
//	              the content is what the page delivered;
//	SettleStalled  the DOM is stable but the network is still busy — the
//	              content is what it's going to be (more may only arrive
//	              on interaction);
//	SettleChurning the page never went quiet — the content is a
//	              SNAPSHOT; a later read may differ.
type SettleState int

const (
	SettleWaiting SettleState = iota // keep waiting (not a final state)
	SettleSettled
	SettleStalled
	SettleChurning
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

// settleDecision is the pure ready-indicator rule (unit-tested): stable
// when nothing is in flight AND the last DOM mutation is at least quietMs
// old in PAGE time (now and lastMut are both performance.now readings —
// no cross-clock comparison).
func settleDecision(now, lastMut, quietMs float64, inFlight int) bool {
	return inFlight == 0 && (now-lastMut) >= quietMs
}

// settleOutcome decides one poll of the settle wait (a pure function of the
// polled values — unit-tested without a browser). The ready indicator
// first, then the two abandonment conditions. now/lastMut are PAGE time
// (ms, from the injected observer); inFlight is the session's in-flight
// request count; elapsed is wall time since the wait began. The order
// matters: a stalled page (quiet DOM, busy network) past the churn cap is
// reported as content-stable — that is the truer claim than "snapshot".
func settleOutcome(now, lastMut float64, inFlight int, elapsed time.Duration) SettleState {
	if settleDecision(now, lastMut, float64(settleQuiet.Milliseconds()), inFlight) {
		return SettleSettled
	}
	if inFlight > 0 && now-lastMut >= float64(settleStallQuiet.Milliseconds()) {
		return SettleStalled
	}
	if elapsed >= settleChurnCap {
		return SettleChurning
	}
	return SettleWaiting
}

// Settle waits for the tab's page to be ready (the ready indicator — see
// the settle* constants; a page that is still progressing is never cut
// off mid-load). It installs the mutation observer (once per document)
// and polls the probe + the Network counter. Returns (state, took); an
// abandoned wait is a STATE, not an error (a churning or stalled page is
// a condition the report speaks to, not a failure).
func (i *Instance) Settle(ctx context.Context, tabID string) (SettleState, time.Duration, error) {
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return SettleWaiting, 0, err
	}
	c := i.netCounterFor(session)
	start := time.Now()
	for {
		body, err := i.evaluateString(ctx, session, settleProbeJS)
		if err != nil {
			return SettleWaiting, time.Since(start), err
		}
		var p struct {
			T   float64 `json:"t"`
			Now float64 `json:"now"`
		}
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			return SettleWaiting, time.Since(start), fmt.Errorf("parsing the settle probe: %w", err)
		}
		if out := settleOutcome(p.Now, p.T, c.count(), time.Since(start)); out != SettleWaiting {
			return out, time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return SettleWaiting, time.Since(start), ctx.Err()
		case <-time.After(settlePoll):
		}
	}
}

// settleNote is the settle state's wording for the read/item/scroll
// reports (shared by FormatRead, BoundItems, and ScrollReport) — three
// states, three wordings, and the settled state says only how long it
// took.
func settleNote(state SettleState, took time.Duration) string {
	switch state {
	case SettleSettled:
		return fmt.Sprintf("settled in %s", took.Round(time.Millisecond))
	case SettleStalled:
		return "content stopped changing but the page's network is still busy — treat the content as complete unless it loads more on interaction"
	default: // SettleChurning (and SettleWaiting, defensive)
		return "the page never went quiet — content is a snapshot; a later read may differ"
	}
}
