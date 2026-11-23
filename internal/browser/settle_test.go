package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSettleDecision pins the pure ready-indicator rule: stable when
// nothing is in flight AND the last DOM mutation is at least the quiet
// window old in PAGE time (no cross-clock comparison).
func TestSettleDecision(t *testing.T) {
	const quietMs = 300.0
	type tc struct {
		name              string
		now, lastMut, qms float64
		inFlight          int
		want              bool
	}
	cases := []tc{
		{"fresh install, nothing in flight: not quiet yet", 1000, 1000, quietMs, 0, false},
		{"quiet elapsed, nothing in flight: settled", 1400, 1000, quietMs, 0, true},
		{"quiet elapsed exactly: settled", 1300, 1000, quietMs, 0, true},
		{"quiet elapsed but a request in flight: not settled", 1400, 1000, quietMs, 1, false},
		{"mutation during the window: not settled", 1200, 1100, quietMs, 0, false},
		{"mutation right at the edge of the window: settled", 1400, 1100, quietMs, 0, true},
		{"two in flight: not settled", 5000, 1000, quietMs, 2, false},
	}
	for _, c := range cases {
		if got := settleDecision(c.now, c.lastMut, c.qms, c.inFlight); got != c.want {
			t.Errorf("%s: settleDecision = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSettleOutcome pins the settle wait's three outcomes: the ready
// indicator, the stall abandon (a quiet DOM with a busy network — the
// content is what it's going to be), and the churn backstop (a page that
// never goes quiet). The order matters: stalled beats churn (a quiet DOM
// past the churn cap is still the truer "content-stable" claim), and
// neither fires while the page is still progressing.
func TestSettleOutcome(t *testing.T) {
	cases := []struct {
		name        string
		now, lastMu float64
		inFlight    int
		elapsed     time.Duration
		want        SettleState
	}{
		{"indicator: idle + quiet past the window", 5000, 4000, 0, 4500 * time.Millisecond, SettleSettled},
		{"indicator: idle but still churning", 5000, 4900, 0, 4500 * time.Millisecond, SettleWaiting},
		{"busy network, fresh mutation: keep waiting", 5000, 4900, 1, 4500 * time.Millisecond, SettleWaiting},
		{"busy network, quiet for 5s: keep waiting", 5000, 0, 1, 4500 * time.Millisecond, SettleWaiting},
		{"busy network, quiet for 10.5s: stalled (content-stable)", 10500, 0, 1, 8000 * time.Millisecond, SettleStalled},
		{"stalled beats churn: quiet 11s + elapsed 15s is content-stable", 11000, 0, 1, 15 * time.Second, SettleStalled},
		{"idle + churning at the 15s cap: snapshot", 15500, 15400, 0, 15 * time.Second, SettleChurning},
		{"busy network, churning at the 15s cap: snapshot", 15500, 15400, 1, 15 * time.Second, SettleChurning},
		{"quiet for 9.9s, not yet stalled", 10400, 500, 1, 8000 * time.Millisecond, SettleWaiting},
	}
	for _, c := range cases {
		if got := settleOutcome(c.now, c.lastMu, c.inFlight, c.elapsed); got != c.want {
			t.Errorf("%s: settleOutcome = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSettleNote pins the three wordings (the tool never leaves the model
// guessing what it got): settled says how long it took, stalled says the
// content is complete-unless-interactive, churning says snapshot.
func TestSettleNote(t *testing.T) {
	if got := settleNote(SettleSettled, 420*time.Millisecond); got != "settled in 420ms" {
		t.Fatalf("settled note = %q", got)
	}
	if got := settleNote(SettleStalled, 10*time.Second); !strings.Contains(got, "network is still busy") || !strings.Contains(got, "complete") {
		t.Fatalf("stalled note = %q", got)
	}
	if got := settleNote(SettleChurning, 15*time.Second); !strings.Contains(got, "never went quiet") || !strings.Contains(got, "snapshot") {
		t.Fatalf("churning note = %q", got)
	}
}

// TestLargeScrollNudge pins the nudge's decision: the ACTUAL move
// (not the requested count) exceeds two viewport heights, in either
// direction, and a zero-height measurement never nudges.
func TestLargeScrollNudge(t *testing.T) {
	if largeScrollNudgeNeeded(0, 1500, 900) {
		t.Errorf("1.67 viewports must not nudge")
	}
	if largeScrollNudgeNeeded(0, 1799, 900) {
		t.Errorf("just under two viewports must not nudge")
	}
	if !largeScrollNudgeNeeded(0, 2000, 900) {
		t.Errorf("2.2 viewports must nudge")
	}
	// Upward counts too (a virtualized list unmounts above as well).
	if !largeScrollNudgeNeeded(3000, 1000, 900) {
		t.Errorf("a big upward move must nudge")
	}
	// A failed measurement (zero height) never nudges.
	if largeScrollNudgeNeeded(0, 99999, 0) {
		t.Errorf("a zero-height measurement must not nudge")
	}
}

// TestScrollReport pins the scroll result's rendering: the position line,
// the bottom/no-bottom wording, and the settle state.
func TestScrollReport(t *testing.T) {
	sr := &ScrollResult{Target: "document", Top: 900, Max: 4500, Settle: SettleSettled, Took: 420 * time.Millisecond}
	out := ScrollReport(sr)
	for _, want := range []string{
		"scrolled document to 900 of 4500",
		"more content may be below",
		"settled in 420ms",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ScrollReport missing %q:\\n%s", want, out)
		}
	}
	sr2 := &ScrollResult{Target: "div#list", Top: 4500, Max: 4500, Bottom: true, Settle: SettleChurning, Took: 15 * time.Second}
	out2 := ScrollReport(sr2)
	for _, want := range []string{
		"scrolled div#list to 4500 of 4500",
		"bottom reached — no more content below",
		"the page never went quiet — content is a snapshot",
	} {
		if !strings.Contains(out2, want) {
			t.Fatalf("ScrollReport (bottom) missing %q:\\n%s", want, out2)
		}
	}
	if strings.Contains(out2, "NOTE:") {
		t.Fatalf("a move that did not cover two viewports must not carry the nudge:\\n%s", out2)
	}

	// A large move (the nudge flag set by Scroll from the measured
	// delta) carries the one-line seatbelt.
	sr3 := &ScrollResult{Target: "document", Prev: 0, Top: 4500, Max: 4500, Vh: 900, Bottom: true, Nudge: true, Settle: SettleSettled, Took: 600 * time.Millisecond}
	if out3 := ScrollReport(sr3); !strings.Contains(out3, "step one viewport at a time instead") {
		t.Fatalf("a nudged scroll must carry the nudge line:\\n%s", out3)
	}
}

// TestBoundItems pins the readItems result's rendering: the header (count,
// dedup, settle), the numbered items in order (link when present), the cap
// notice, and the empty-view notice.
func TestBoundItems(t *testing.T) {
	pi := &PageItems{
		Dups:   2,
		Settle: SettleSettled,
		Took:   400 * time.Millisecond,
		Items: []ItemRef{
			{N: 1, Text: "First item"},
			{N: 2, Text: "Second item", URL: "https://example.com/2"},
		},
	}
	out := BoundItems(pi)
	for _, want := range []string{
		"items: 2 (visual order, deduped within the view; 2 duplicate texts dropped; page settled in 400ms)",
		"\n1. First item",
		"\n2. Second item (https://example.com/2)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("BoundItems missing %q:\\n%s", want, out)
		}
	}
	if strings.Contains(out, "cap was hit") {
		t.Fatalf("small result must not carry the cap notice:\\n%s", out)
	}

	capped := &PageItems{Items: make([]ItemRef, itemCap)}
	for i := range capped.Items {
		capped.Items[i] = ItemRef{N: i + 1, Text: fmt.Sprintf("item %d", i+1)}
	}
	if out := BoundItems(capped); !strings.Contains(out, "cap was hit") {
		t.Fatalf("a full result must carry the cap notice:\\n%.300s", out)
	}

	if out := BoundItems(&PageItems{Items: []ItemRef{}}); !strings.Contains(out, "no matching items in view") {
		t.Fatalf("empty result must say so:\\n%s", out)
	}
}

// TestCDPEventRouting: an event frame (no id) with a flattened sessionId
// reaches the registered sink — the delivery path of the Network-domain
// counter settle consumes. (The no-sink drop path is exercised by
// TestCDPClientAgainstFakeEndpoint: its fake pushes one event per
// response and the call still resolves.)
func TestCDPEventRouting(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := fakeCDP(t, &mu, &bodies)
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	c, err := Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	type ev struct{ sessionID, method string }
	var gotMu sync.Mutex
	var got []ev
	c.SetEventSink(func(sessionID, method string, _ json.RawMessage) {
		gotMu.Lock()
		got = append(got, ev{sessionID, method})
		gotMu.Unlock()
	})

	if _, err := c.Call(ctx, "SESS123", "Runtime.evaluate", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		gotMu.Lock()
		n := len(got)
		gotMu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no event reached the sink")
		}
		time.Sleep(10 * time.Millisecond)
	}
	gotMu.Lock()
	e := got[0]
	gotMu.Unlock()
	if e.sessionID != "SESS123" || e.method != "Network.loadingFinished" {
		t.Fatalf("event = %+v, want session SESS123 / Network.loadingFinished", e)
	}
}
