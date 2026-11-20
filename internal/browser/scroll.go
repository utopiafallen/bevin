package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// scrollJS: find the scroll TARGET (the document when it scrolls, else the
// largest-area element with real scrollable overflow — the inner-container
// case virtualized grids present), move it (n viewports down/up, or to the
// top/bottom), and report the new position. %s = the direction (a JSON
// string literal — the pageCleanerJS pattern), %d = the viewport count.
var scrollJS = `(() => {
  const dir = %s;
  const n = %d;
  const de = document.scrollingElement || document.documentElement;
  let el, name;
  if (de.scrollHeight > de.clientHeight + 40) {
    el = de;
    name = "document";
  } else {
    let best = null, bestArea = 0;
    for (const c of document.querySelectorAll("*")) {
      if (c.scrollHeight <= c.clientHeight + 40) continue;
      const r = c.getBoundingClientRect();
      const area = r.width * r.height;
      if (area > bestArea) { bestArea = area; best = c; }
    }
    if (!best) return JSON.stringify({ error: "nothing scrollable (the page fits the viewport)" });
    el = best;
    name = best.tagName.toLowerCase() + (best.id ? "#" + CSS.escape(best.id) : "");
  }
  const vh = el === de ? window.innerHeight : el.clientHeight;
  const max = el.scrollHeight - el.clientHeight;
  const prev = el.scrollTop;
  let top;
  if (dir === "top") top = 0;
  else if (dir === "bottom") top = max;
  else top = Math.max(0, Math.min(max, el.scrollTop + (dir === "up" ? -1 : 1) * n * vh));
  if (el === de) window.scrollTo(0, top); else el.scrollTop = top;
  return JSON.stringify({ target: name, top: el.scrollTop, prev, max, vh });
})()`

// ScrollResult is one scroll call's outcome (before the tool renders it).
type ScrollResult struct {
	Target  string // "document" or "<tag>#<id>"
	Prev    float64 // the scroll position before the move
	Top     float64 // the new scroll position
	Max     float64 // the furthest scrollable position
	Vh      float64 // the target's viewport height
	Bottom  bool    // the bottom was reached (nothing more below)
	Nudge   bool    // the move was large (>2 viewports) — the seatbelt line
	Settled bool
	Took    time.Duration
}

// Scroll moves the tab's page (or its inner scroll container) by whole
// viewports and settles it (the design log's LAZY-LIST TIER): one viewport
// by default — the model never chooses a pixel distance, and the settle
// after the move is what lets the next read trust what it sees.
func (i *Instance) Scroll(ctx context.Context, tabID, direction string, viewports int) (*ScrollResult, error) {
	if viewports < 1 {
		viewports = 1
	}
	if viewports > scrollMaxViewports {
		viewports = scrollMaxViewports
	}
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return nil, err
	}
	dir, _ := json.Marshal(direction)
	body, err := i.evaluateString(ctx, session, fmt.Sprintf(scrollJS, dir, viewports))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Error  string  `json:"error"`
		Target string  `json:"target"`
		Top    float64 `json:"top"`
		Prev   float64 `json:"prev"`
		Max    float64 `json:"max"`
		Vh     float64 `json:"vh"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("parsing the scroll result: %w", err)
	}
	if raw.Error != "" {
		return nil, fmt.Errorf("%s", raw.Error)
	}
	// Trigger grace (the design log's LAZY-LIST TIER, "trigger grace"
	// note): the viewport change may TRIGGER lazy work (a scroll listener
	// firing a fetch) whose first network event lands after a bare settle
	// has already declared stability — settle would return with nothing
	// to observe (no mutation yet, nothing in flight yet) and the next
	// read would race the fetch (the TOCTOU behind the original lazy-
	// list failure, one tick later). Wait up to triggerGrace for in-
	// flight activity to appear and hand off to the settle the moment it
	// it does; a trigger that never comes costs the full grace before
	// the quiet window runs. Deliberately scoped to the scroll command
	// — it is the one place the harness KNOWS the viewport moved (there
	// is no model-controlled JS scroll path anymore — evaluate was
	// removed 2026-11-19; the nudge covers its old job on a
	// multi-viewport scroll command).
	graceDeadline := time.Now().Add(triggerGrace)
	for i.netCounterFor(session).count() == 0 {
		if time.Now().After(graceDeadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	settled, took, err := i.Settle(ctx, tabID)
	if err != nil {
		return nil, err
	}
	return &ScrollResult{
		Target:  raw.Target,
		Prev:    raw.Prev,
		Top:     raw.Top,
		Max:     raw.Max,
		Vh:      raw.Vh,
		Bottom:  raw.Top >= raw.Max-2,
		Nudge:   largeScrollNudgeNeeded(raw.Prev, raw.Top, raw.Vh),
		Settled: settled,
		Took:    took,
	}, nil
}

// ScrollReport renders a scroll result for the model.
func ScrollReport(sr *ScrollResult) string {
	out := fmt.Sprintf("scrolled %s to %.0f of %.0f", sr.Target, sr.Top, sr.Max)
	if sr.Bottom {
		out += " (bottom reached — no more content below)"
	} else {
		out += " (more content may be below)"
	}
	out += fmt.Sprintf(" — page %s", settleWord(sr.Settled, sr.Took))
	if sr.Nudge {
		out += "\nNOTE: " + LargeScrollNudge
	}
	return out
}

// LargeScrollNudge is the one-line seatbelt (the design log's LAZY-LIST
// TIER): appended when a single scroll move covered more than two
// viewport heights — the move that skips un-rendered lazy content (and
// the unmounting of a virtualized list). One line, no capability removed.
const LargeScrollNudge = "moved more than two viewports at once on a page that may have lazy content — content between the start and end positions may not have rendered; step one viewport at a time instead."

// largeScrollNudgeNeeded is the nudge's decision (pure, unit-tested): the
// ACTUAL move (|top - prev|, measured by the scroll itself) exceeds two
// viewport heights — in either direction. The requested count is not the
// criterion (a five-viewport request at the bottom of the page moves
// nothing and must not nudge; the same request on a deep page does). A
// zero-height measurement never nudges — the guard must not fire on
// itself.
func largeScrollNudgeNeeded(prev, top, vh float64) bool {
	d := top - prev
	if d < 0 {
		d = -d
	}
	return vh > 0 && d > 2*vh
}

// scrollMaxViewports caps one scroll command's viewports (the loop is the
// model's; a single call moving the whole list is the big scroll the
// nudge warns about).
const scrollMaxViewports = 10

// triggerGrace bounds the post-scroll wait for lazy work to appear in the
// in-flight counter (see Scroll's "trigger grace" note). 250 ms: long
// enough for a scroll handler to fire its fetch on a loaded box (the
// WSLg stall that caused the original failure), short enough that a
// scroll that triggers nothing pays only the grace before the settle's
// quiet window runs.
const triggerGrace = 250 * time.Millisecond
