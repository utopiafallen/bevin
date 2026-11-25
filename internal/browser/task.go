package browser

// task.go — the TASK-SHAPED commands (design log BROWSER-ROBUST batch 2,
// 2026-11-23): operations organized around the agent's tasks, one call
// each, one report of what happened and HOW IT STOPPED. They are the
// boundary test against replicating a browser-automation framework:
// task-shaped (not mechanism-shaped), no state across calls (the whole
// loop is one call — the stateless boundary is per-CALL, not per-session),
// bounded work that always ends in a report. What they deliberately are
// NOT: a Playwright — no persistent locators/collections, no network
// layer, no synthetic contexts, no unbounded waits (see the design log's
// BROWSER TOOL BOUNDARY invariants).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// collect's bounds (named constants — bounded work that ends in a report,
// boundary invariant 5): the item cap (the hard output bound), the scroll
// step cap, and the wall-time cap (a pathological page must not hold the
// turn — the report says the time cap was hit).
const (
	collectMaxItems = 500
	collectMaxSteps = 20
	collectMaxWall  = 120 * time.Second
)

// CollectReport is one collect's result: every item matching the selector
// on the page (lazy-loaded ones included — the loop runs inside the call),
// in first-seen visual order, plus HOW the loop stopped.
type CollectReport struct {
	Items   []ItemRef `json:"items"`
	Stop    string    `json:"stop"`    // "bottom" | "no-new" | "cap" | "time" | "steps"
	Dropped int       `json:"dropped"` // cross-batch duplicates dropped (the in-batch dedup is inside the extractor)
}

// collectItems is one settle+extract pass of the collect loop (readItems'
// extraction at collect's higher item cap — the full pre-truncation text
// rides along as each item's dedupe key).
func (i *Instance) collectItems(ctx context.Context, tabID, selector string) (*PageItems, error) {
	settled, took, err := i.Settle(ctx, tabID)
	if err != nil {
		return nil, err
	}
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return nil, err
	}
	sel, _ := json.Marshal(selector)
	body, err := i.evaluateString(ctx, session, fmt.Sprintf(readItemsJS, sel, collectMaxItems, itemTextCap, itemTextCap))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Error string    `json:"error"`
		Dups  int       `json:"dups"`
		Items []ItemRef `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("parsing the item extraction: %w", err)
	}
	if raw.Error != "" {
		return nil, fmt.Errorf("%s", raw.Error)
	}
	if raw.Items == nil {
		raw.Items = []ItemRef{}
	}
	return &PageItems{Dups: raw.Dups, Items: raw.Items, Settle: settled, Took: took}, nil
}

// Collect returns EVERY item matching selector on the page — including
// lazy-loaded ones. The scroll → settle → extract → dedupe loop runs
// server-side INSIDE the call: the model names the item selector and reads
// the result; it never runs the loop or tracks state across calls (the
// stateless boundary is per-call — this is not the shelved stateful
// scrollNextPage). Stops (reported, never silent): the bottom, no new
// items after scrolling, the item cap, the wall cap, or the step cap.
func (i *Instance) Collect(ctx context.Context, tabID, selector string) (*CollectReport, error) {
	if selector == "" {
		return nil, fmt.Errorf("collect needs a selector (the CSS selector of ONE item in the list — read the page first to find one)")
	}
	start := time.Now()
	rep := &CollectReport{Items: []ItemRef{}}
	seen := map[string]bool{}
	prevTop := -1.0
	zeros := 0
	for step := 0; step < collectMaxSteps; step++ {
		if time.Since(start) > collectMaxWall {
			rep.Stop = "time"
			break
		}
		pi, err := i.collectItems(ctx, tabID, selector)
		if err != nil {
			return nil, err
		}
		newCount := i.addCollected(rep, seen, pi)
		// An empty first pass: the selector matched nothing (or the list
		// is empty) — stop before burning a scroll on it.
		if step == 0 && len(rep.Items) == 0 {
			rep.Stop = "no-new"
			break
		}
		if len(rep.Items) >= collectMaxItems {
			rep.Stop = "cap"
			break
		}
		sr, err := i.Scroll(ctx, tabID, "down", 1)
		if err != nil {
			return nil, err
		}
		if sr.Bottom || sr.Top == prevTop {
			// The bottom (or the scroll stopped moving). On a LAZY page the
			// bottom is not the end — the scroll that got here may have just
			// triggered the next batch, and the page GROWS: a final extract
			// that brings new items means there is more to scroll, so keep
			// going (the next scroll has room again). Nothing new is the end.
			if pi2, err := i.collectItems(ctx, tabID, selector); err == nil {
				if i.addCollected(rep, seen, pi2) > 0 && len(rep.Items) < collectMaxItems {
					prevTop = sr.Top
					continue
				}
			}
			if len(rep.Items) >= collectMaxItems {
				rep.Stop = "cap"
			} else {
				rep.Stop = "bottom"
			}
			break
		}
		prevTop = sr.Top
		if newCount == 0 {
			zeros++
		} else {
			zeros = 0
		}
		// Two consecutive no-new passes away from the bottom: the list
		// looks exhausted (or the page gates the rest behind a control) —
		// stop rather than scroll into the void.
		if zeros >= 2 {
			rep.Stop = "no-new"
			break
		}
	}
	if rep.Stop == "" {
		rep.Stop = "steps" // the step budget ran out without a stop signal
	}
	return rep, nil
}

// addCollected appends pi's items to rep that have not been seen before
// (keyed on the PRE-truncation text — a truncation can never merge two
// distinct items) and returns how many were new.
func (i *Instance) addCollected(rep *CollectReport, seen map[string]bool, pi *PageItems) int {
	newCount := 0
	for _, it := range pi.Items {
		key := it.Full
		if key == "" {
			key = it.Text
		}
		if seen[key] {
			rep.Dropped++
			continue
		}
		seen[key] = true
		it.N = len(rep.Items) + 1 // renumbered in the final, first-seen order
		rep.Items = append(rep.Items, it)
		newCount++
	}
	return newCount
}

// FormatCollect renders a collect result for the model: the count, the
// items in first-seen order (numbered; text + first link when present),
// and the stop reason — always stated, because "where did it stop" is the
// difference between "the complete list" and "a partial list".
func FormatCollect(rep *CollectReport) string {
	out := fmt.Sprintf("collected %d item(s)", len(rep.Items))
	for _, it := range rep.Items {
		if it.URL != "" {
			out += fmt.Sprintf("\n%d. %s (%s)", it.N, it.Text, it.URL)
		} else {
			out += fmt.Sprintf("\n%d. %s", it.N, it.Text)
		}
	}
	out += "\n" + collectStopLine(rep.Stop)
	if len(rep.Items) == 0 {
		out += " (no matching items — the selector may be wrong, or the list is empty)"
	}
	return out
}

// collectStopLine is the stop reason's wording (the report never leaves
// the completeness question open).
func collectStopLine(stop string) string {
	switch stop {
	case "bottom":
		return "stop: the page's bottom was reached — this is the complete list"
	case "no-new":
		return "stop: no new items after scrolling — the list looks exhausted (more may remain behind a control like a \"load more\" button)"
	case "cap":
		return fmt.Sprintf("stop: the item cap (%d) was hit — more may remain", collectMaxItems)
	case "time":
		return fmt.Sprintf("stop: the time cap (%s) was hit — more may remain", collectMaxWall/time.Second)
	default: // "steps"
		return fmt.Sprintf("stop: the scroll cap (%d) was hit — more may remain", collectMaxSteps)
	}
}

// Fill types text into several fields in ONE call and (when submit is
// given) clicks it. Each field key is the same element reference as
// click/type: the #N id from the CALLING conversation's last read, or a
// raw selector. Every field's outcome is reported; if ANY field failed,
// submit is NOT clicked — a form that is known to be incomplete is never
// submitted (the model fixes the field and retries with submit).
func (i *Instance) Fill(ctx context.Context, convID, tabID string, fields map[string]string, submit string) (string, error) {
	if len(fields) == 0 {
		return "", fmt.Errorf("fill needs fields (a field→text map; each key is a #N id or selector)")
	}
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic order (map iteration is random) and report order
	var done, failed []string
	for _, k := range keys {
		text := fields[k]
		// strict (no text rescue): a field is an interactive element by
		// its name — a text-shaped key must not "type" into a heading.
		x, y, _, err := i.ElementTarget(ctx, session, convID, k, false)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", k, err))
			continue
		}
		if err := i.TypeAt(ctx, session, x, y, text); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", k, err))
			continue
		}
		done = append(done, k)
	}
	if len(failed) > 0 {
		return fmt.Sprintf("filled %d of %d field(s); failed: %s — NOT submitted (fix the field, then retry with submit).",
			len(done), len(fields), strings.Join(failed, "; ")), nil
	}
	if submit == "" {
		return fmt.Sprintf("filled %d field(s)", len(fields)), nil
	}
	x, y, _, err := i.ElementTarget(ctx, session, convID, submit, false)
	if err != nil {
		return fmt.Sprintf("filled %d field(s) but submit %q failed (%v) — the form may be incomplete; read the page.",
			len(fields), submit, err), nil
	}
	if err := i.ClickAt(ctx, session, x, y); err != nil {
		return fmt.Sprintf("filled %d field(s) but clicking submit %q failed: %v. read the page.",
			len(fields), submit, err), nil
	}
	return fmt.Sprintf("filled %d field(s) and submitted (%q clicked). read to see the result.", len(fields), submit), nil
}
