package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// readItems' bounds (named constants): at most itemCap items, at most
// itemTextCap chars of text per item (truncated, marked).
const (
	itemCap     = 100
	itemTextCap = 500
)

// readItemsJS is the stateless item extractor (the design log's LAZY-LIST
// TIER; the shelved scrollNextPage reuses it verbatim): the matched
// VISIBLE elements, normalized text, the first href inside, SORTED BY
// BOUNDING BOX (top, left) — visual order, not DOM order (on a virtualized
// grid DOM order is an accident of render timing) — DEDUPED within the
// call by exact normalized text (the PRE-truncation text is the key, so a
// truncation can never merge two distinct items). %s = the selector (a
// JSON string literal — the pageCleanerJS pattern), %d = the caps.
var readItemsJS = `(() => {
  const sel = %s;
  if (!sel) return JSON.stringify({ error: "readItems needs a selector" });
  let els;
  try { els = Array.from(document.querySelectorAll(sel)); }
  catch (e) { return JSON.stringify({ error: "bad selector: " + e.message, items: [] }); }
  if (!els.length) return JSON.stringify({ error: "selector matched nothing: " + sel, items: [] });
  const out = [];
  const seen = new Set();
  let dups = 0;
  for (const el of els) {
    if (out.length >= %d) break;
    const s = getComputedStyle(el);
    if (s.display === "none" || s.visibility === "hidden") continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    const full = (el.innerText || el.textContent || "").replace(/\s+/g, " ").trim();
    if (!full) continue;
    if (seen.has(full)) { dups++; continue; }
    seen.add(full);
    let text = full;
    if (text.length > %d) text = text.slice(0, %d) + "…";
    let url = "";
    const a = el.querySelector("a[href]");
    if (a) url = a.href;
    out.push({ top: r.top, left: r.left, text, url });
  }
  out.sort((a, b) => a.top - b.top || a.left - b.left);
  return JSON.stringify({ dups, items: out.map((it, i) => ({ n: i + 1, text: it.text, url: it.url || "" })) });
})()`

// ItemRef is one collected item: n = its 1-based position in VISUAL
// order, Text the normalized item text (truncated at itemTextCap), URL
// the first link inside the item ("" when it has none).
type ItemRef struct {
	N    int    `json:"n"`
	Text string `json:"text"`
	URL  string `json:"url"`
}

// PageItems is one readItems result (before bounding).
type PageItems struct {
	Error   string
	Dups    int // items dropped by the in-call text dedup
	Items   []ItemRef
	Settled bool
	Took    time.Duration
}

// ReadItems settles the page first (the mid-render fix — see Settle) then
// extracts the items matching selector in visual order.
func (i *Instance) ReadItems(ctx context.Context, tabID, selector string) (*PageItems, error) {
	if selector == "" {
		return nil, fmt.Errorf("readItems needs a selector (the item's CSS selector — read the page first to find one)")
	}
	settled, took, err := i.Settle(ctx, tabID)
	if err != nil {
		return nil, err
	}
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return nil, err
	}
	sel, _ := json.Marshal(selector)
	body, err := i.evaluateString(ctx, session, fmt.Sprintf(readItemsJS, sel, itemCap, itemTextCap, itemTextCap))
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
	return &PageItems{Dups: raw.Dups, Items: raw.Items, Settled: settled, Took: took}, nil
}

// BoundItems renders a readItems result for the model: the header (count,
// dedup, settle state), the items in visual order (numbered; text + first
// link when present), and the notices — the cap (more may remain below)
// and the empty view.
func BoundItems(pi *PageItems) string {
	out := fmt.Sprintf("items: %d (visual order, deduped within the view; %d duplicate texts dropped; page %s)",
		len(pi.Items), pi.Dups, settleWord(pi.Settled, pi.Took))
	for _, it := range pi.Items {
		if it.URL != "" {
			out += fmt.Sprintf("\n%d. %s (%s)", it.N, it.Text, it.URL)
		} else {
			out += fmt.Sprintf("\n%d. %s", it.N, it.Text)
		}
	}
	if len(pi.Items) >= itemCap {
		out += "\n(the cap was hit — more items may remain below; scroll and readItems again)"
	}
	if len(pi.Items) == 0 {
		out += " (no matching items in view)"
	}
	return out
}
