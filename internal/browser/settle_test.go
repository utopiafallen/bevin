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

// TestSettleDecision pins the pure stability rule: stable when nothing is
// in flight AND the last DOM mutation is at least the quiet window old in
// PAGE time (no cross-clock comparison).
func TestSettleDecision(t *testing.T) {
	const quietMs = 300.0
	type tc struct {
		name                    string
		now, lastMut, qms       float64
		inFlight                int
		want                    bool
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

// TestLargeScrollNote pins the guard's decision: the nudge when the move
// exceeds two viewport heights (either direction), nothing below, and
// nothing when the measurement itself is unusable (zero height).
func TestLargeScrollNote(t *testing.T) {
	start := scrollPos{Y: 0, H: 900}
	if got := LargeScrollNote(start, scrollPos{Y: 1500, H: 900}); got != "" {
		t.Errorf("1.67 viewports must not nudge: %q", got)
	}
	if got := LargeScrollNote(start, scrollPos{Y: 1799, H: 900}); got != "" {
		t.Errorf("just under two viewports must not nudge: %q", got)
	}
	if got := LargeScrollNote(start, scrollPos{Y: 2000, H: 900}); !strings.Contains(got, "incremental scrolling") {
		t.Errorf("2.2 viewports must carry the nudge: %q", got)
	}
	// Upward counts too.
	if got := LargeScrollNote(scrollPos{Y: 3000, H: 900}, scrollPos{Y: 1000, H: 900}); !strings.Contains(got, "incremental scrolling") {
		t.Errorf("a big upward move must nudge: %q", got)
	}
	// A failed measurement (zero height) never nudges.
	if got := LargeScrollNote(start, scrollPos{Y: 99999, H: 0}); got != "" {
		t.Errorf("a zero-height measurement must not nudge: %q", got)
	}
}

// TestScrollReport pins the scroll result's rendering: the position line,
// the bottom/no-bottom wording, and the settle state.
func TestScrollReport(t *testing.T) {
	sr := &ScrollResult{Target: "document", Top: 900, Max: 4500, Settled: true, Took: 420 * time.Millisecond}
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
	sr2 := &ScrollResult{Target: "div#list", Top: 4500, Max: 4500, Bottom: true, Settled: false, Took: 5 * time.Second}
	out2 := ScrollReport(sr2)
	for _, want := range []string{
		"scrolled div#list to 4500 of 4500",
		"bottom reached — no more content below",
		"still settling after 5s",
	} {
		if !strings.Contains(out2, want) {
			t.Fatalf("ScrollReport (bottom) missing %q:\\n%s", want, out2)
		}
	}
}

// TestBoundItems pins the readItems result's rendering: the header (count,
// dedup, settle), the numbered items in order (link when present), the cap
// notice, and the empty-view notice.
func TestBoundItems(t *testing.T) {
	pi := &PageItems{
		Dups:    2,
		Settled: true,
		Took:    400 * time.Millisecond,
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
