package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWebBrowserLive is the WS5 acceptance test: the FULL stack against a
// REAL browser — detection, launch (a dedicated visible window with its own
// profile), CDP attach, tab management, the page clean (markdown + the
// numbered element table), a REAL input click that changes the page, a
// type, a select, and teardown. It is env-gated (BEVIN_E2E_BROWSER=1): it
// needs a desktop host with a Chromium-based browser (e.g. the WSLg box
// with Edge) and takes ~10 s to launch.
func TestWebBrowserLive(t *testing.T) {
	if os.Getenv("BEVIN_E2E_BROWSER") != "1" {
		t.Skip("set BEVIN_E2E_BROWSER=1 to run the live browser e2e (needs a desktop host with a browser)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The page under test: a heading + paragraph (the clean's markdown),
	// a button that reveals a div (the click + waitFor path), a text input
	// (the type path), and a select (the select path).
	// The long body: 2,500 paragraphs (~140k chars of markdown — far past
	// the old 32k read budget): the read must come back WHOLE,
	// untruncated (user decision 2026-11-21 — the budget made real pages
	// unusable).
	var longBody strings.Builder
	for n := 1; n <= 2500; n++ {
		fmt.Fprintf(&longBody, "<p>LONGPAGE-%04d — some body text that makes the page long.</p>\n", n)
	}
	page := `<html><head><title>Bevin E2E</title></head><body>
<h1>E2E Page</h1>
<p>Some paragraph text about testing browsers.</p>
<a href="about:blank" id="other-link">go elsewhere</a>
<button id="toggle" onclick="var d=document.getElementById('after');d.style.display='block';d.textContent='toggled!'">toggle</button>
<div id="after" style="display:none"></div>
<input id="q" type="text" placeholder="search">
<select id="pick"><option value="a">Option A</option><option value="b">Option B</option></select>
<span id="actwrap"><button class="applybtn" onclick="var d=document.getElementById('after2');d.style.display='block';d.textContent='applied!'">apply</button></span>
<div id="after2" style="display:none"></div>
<div id="formwrap"><input id="f1" type="text" placeholder="first field"><input id="f2" type="text" placeholder="second field"><button class="submitbtn" onclick="var d=document.getElementById('after3');d.style.display='block';d.textContent=document.getElementById('f1').value+'|'+document.getElementById('f2').value">send</button></div>
<div id="after3" style="display:none"></div>
` + longBody.String() + `</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	mgr := NewManager(dataDir)
	t.Cleanup(func() {
		mgr.Close()
		// Let the browser's process tree (a killed main process can orphan
		// children for a moment) release the profile files before the temp
		// dir is removed — a straggler makes the RemoveAll fail the test.
		time.Sleep(2 * time.Second)
	})
	inst, err := mgr.ForUser(ctx, "e2e-user")
	if err != nil {
		t.Fatalf("ForUser (browser launch): %v", err)
	}
	if mgr.Instances() != 1 {
		t.Fatalf("Instances = %d, want 1", mgr.Instances())
	}

	// openTab(url): a new tab, loaded.
	tab, err := inst.OpenTab(ctx, "", srv.URL)
	if err != nil {
		t.Fatalf("OpenTab: %v", err)
	}
	if tab.ID == "" {
		t.Fatal("OpenTab returned no tab id")
	}

	// read: the clean page — title/url, the markdown (heading + paragraph),
	// and the element table with the expected entries.
	pr, err := inst.Read(ctx, "", tab.ID, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if pr.Title != "Bevin E2E" {
		t.Fatalf("title = %q (url=%s), want %q; markdown head:\n%.400s", pr.Title, pr.URL, "Bevin E2E", pr.Markdown)
	}
	if !strings.Contains(pr.Markdown, "E2E Page") || !strings.Contains(pr.Markdown, "Some paragraph text about testing browsers") {
		t.Fatalf("markdown missing the page content:\n%s", pr.Markdown)
	}
	find := func(role, name string) elementRef {
		for _, e := range pr.Elements {
			if e.Role == role && strings.EqualFold(e.Name, name) {
				return e
			}
		}
		return elementRef{}
	}
	toggle := find("button", "toggle")
	if toggle.N < 1 {
		t.Fatalf("the toggle button is not in the element table:\n%s", FormatRead(pr))
	}
	if _, ok := inst.ElementByN("", toggle.N); !ok {
		t.Fatal("the read's element table is not resolvable by #N")
	}
	if in := find("textbox", "search"); in.N < 1 {
		t.Fatalf("the search input is not in the element table:\n%s", FormatRead(pr))
	}
	if link := find("link", "go elsewhere"); link.N < 1 {
		t.Fatalf("the link is not in the element table:\n%s", FormatRead(pr))
	}

	// Unbounded read: the FINAL line of the long body (past the old 32k
	// budget by ~4x) must be present — the read never truncates.
	if !strings.Contains(pr.Markdown, "LONGPAGE-2500") {
		t.Fatalf("read must be unbounded (whole page): the long body's final line is missing (markdown = %d chars); tail:\n%.400s", len(pr.Markdown), pr.Markdown)
	}

	// click the toggle button (REAL input events at the element's center)
	// and wait for its effect.
	session, err := inst.SessionFor(ctx, tab.ID)
	if err != nil {
		t.Fatalf("SessionFor: %v", err)
	}
	x, y, err := inst.ElementCenter(ctx, session, "#toggle")
	if err != nil {
		t.Fatalf("ElementCenter: %v", err)
	}
	if err := inst.ClickAt(ctx, session, x, y); err != nil {
		t.Fatalf("ClickAt: %v", err)
	}
	ok, took, err := inst.Wait(ctx, tab.ID, "#after", 15*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !ok {
		t.Fatalf("the toggle click had no effect after %s (the click did not land)", took)
	}

	// read again: the revealed text is in the clean now (the page changed).
	pr2, err := inst.Read(ctx, "", tab.ID, "")
	if err != nil {
		t.Fatalf("Read (after click): %v", err)
	}
	if !strings.Contains(pr2.Markdown, "toggled!") {
		t.Fatalf("post-click markdown missing the revealed text:\n%s", pr2.Markdown)
	}

	// type into the search input (focus click + text insertion).
	qx, qy, err := inst.ElementCenter(ctx, session, "#q")
	if err != nil {
		t.Fatalf("ElementCenter(#q): %v", err)
	}
	if err := inst.TypeAt(ctx, session, qx, qy, "hello bevin"); err != nil {
		t.Fatalf("TypeAt: %v", err)
	}
	v, err := inst.Evaluate(ctx, session, `document.getElementById("q").value`)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if strings.TrimSpace(string(v)) != `"hello bevin"` {
		t.Fatalf("input value = %s, want \"hello bevin\"", v)
	}

	// select: pick Option B by its visible text.
	out, err := inst.SelectValue(ctx, session, "#pick", "Option B")
	if err != nil {
		t.Fatalf("SelectValue: %v", err)
	}
	if !strings.Contains(out, "Option B") {
		t.Fatalf("SelectValue = %q, want the chosen option", out)
	}

	// Re-match (BROWSER-ROBUST batch 1): the "apply" button has NO id —
	// the read stored it an nth-of-type PATH. The page re-renders (a new
	// button inserted before it — the path now matches the WRONG
	// element), and the stored #N must still resolve to the right
	// element: the resolver verifies the stored hit's identity (tag +
	// name) and re-matches by (tag + name) — the failure that killed 10
	// of 12 clicks in the car-research session.
	apply := find("button", "apply")
	if apply.N < 1 {
		t.Fatalf("the apply button is not in the element table:\n%s", FormatRead(pr))
	}
	if _, err := inst.Evaluate(ctx, session, `(() => { const b = document.createElement("button"); b.textContent = "other"; b.id = "otherbtn"; document.getElementById("actwrap").insertBefore(b, document.querySelector(".applybtn")); return "ok"; })()`); err != nil {
		t.Fatalf("DOM rearrange: %v", err)
	}
	ax, ay, aSel, err := inst.ElementTarget(ctx, session, "", fmt.Sprintf("#%d", apply.N), false)
	if err != nil {
		t.Fatalf("ElementTarget (re-match) = %v — the stored #N must survive a re-render", err)
	}
	if strings.Contains(aSel, "otherbtn") {
		t.Fatalf("re-match resolved %q — that is the WRONG element (the shifted path's hit)", aSel)
	}
	if err := inst.ClickAt(ctx, session, ax, ay); err != nil {
		t.Fatalf("ClickAt (re-match): %v", err)
	}
	ok3, took3, err := inst.Wait(ctx, tab.ID, "#after2", 15*time.Second)
	if err != nil {
		t.Fatalf("Wait (re-match): %v", err)
	}
	if !ok3 {
		t.Fatalf("the re-matched click had no effect after %s (it landed on the wrong element)", took3)
	}

	// Ambiguity: two buttons with the same name — the resolver must
	// refuse to guess (the "read again and use a fresh #N" outcome, not
	// a silent mis-click).
	if _, err := inst.Evaluate(ctx, session, `(() => { const b = document.createElement("button"); b.textContent = "apply"; b.id = "applydup"; document.getElementById("actwrap").appendChild(b); return "ok"; })()`); err != nil {
		t.Fatalf("DOM ambiguity: %v", err)
	}
	if _, _, _, err := inst.ElementTarget(ctx, session, "", fmt.Sprintf("#%d", apply.N), false); err == nil || !strings.Contains(err.Error(), "multiple elements match") {
		t.Fatalf("ambiguous re-match err = %v, want the multiple-match refusal", err)
	}

	// Fill (BROWSER-ROBUST batch 2): one call fills BOTH fields (by their
	// placeholder names — not selectors) and clicks the submit; the
	// per-field outcomes come back in the result (the car-research
	// session needed four separate calls for this).
	res, err := inst.Fill(ctx, "", tab.ID, map[string]string{"first field": "alpha", "second field": "beta"}, "send")
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if strings.Contains(res, "failed") || strings.Contains(res, "not found") {
		t.Fatalf("fill must succeed on a static form:\\n%s", res)
	}
	v3, err := inst.Evaluate(ctx, session, `[document.getElementById("f1").value, document.getElementById("f2").value]`)
	if err != nil {
		t.Fatalf("Evaluate (fill check): %v", err)
	}
	if strings.TrimSpace(string(v3)) != `["alpha","beta"]` {
		t.Fatalf("filled values = %s, want [\"alpha\",\"beta\"]", v3)
	}
	ok4, took4, err := inst.Wait(ctx, tab.ID, "#after3", 15*time.Second)
	if err != nil {
		t.Fatalf("Wait (fill submit): %v", err)
	}
	if !ok4 {
		t.Fatalf("the fill's submit click had no effect after %s", took4)
	}

	// tab management: the browser was launched with ONE initial tab (the
	// about:blank the launch args pass) + the tab the test opened.
	tabs, err := inst.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs: %v", err)
	}
	var listing []string
	for _, tb := range tabs {
		listing = append(listing, tb.URL)
	}
	if len(tabs) != 2 {
		t.Fatalf("tabs = %d (%v), want 2 (the launch tab + the opened one)", len(tabs), listing)
	}
	if _, err := inst.CloseTab(ctx, "", tab.ID); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	tabs, err = inst.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs (after close): %v", err)
	}
	if len(tabs) != 1 {
		t.Fatalf("tabs after close = %d, want 1 (the launch tab)", len(tabs))
	}

	// Teardown: the manager closes the browser; a second ForUser call
	// relaunches it (the dead-instance recovery the tool relies on).
	mgr.Close()
	inst2, err := mgr.ForUser(ctx, "e2e-user")
	if err != nil {
		t.Fatalf("ForUser (relaunch): %v", err)
	}
	defer inst2.Close()
}

// TestWebBrowserLazyListLive is the LAZY-LIST TIER acceptance (design
// log): a list that appends its items after an ASYNC fetch when scrolled
// near the bottom. Pins: settle-before-extract (the first read after a
// scroll includes the async-appended items — the mid-render case that
// caused the original failure), readItems' visual order + in-call dedup,
// the scroll command's one-viewport step + reached-bottom, the full
// collection (a Go-side dedupe simulates the model's in-context diff),
// and the large-scroll nudge on a multi-viewport move (the seatbelt that
// replaced the removed evaluate guard).
func TestWebBrowserLazyListLive(t *testing.T) {
	if os.Getenv("BEVIN_E2E_BROWSER") != "1" {
		t.Skip("set BEVIN_E2E_BROWSER=1 to run the live browser e2e (needs a desktop host with a browser)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const totalItems = 60
	var mu sync.Mutex
	fetches := 0
	// The lazy-list page: 20 items synchronously (one viewport + a bit),
	// the rest appended in batches of 10 after a 250 ms fetch when the
	// page is scrolled near its bottom. A duplicate-text item (#dup)
	// exercises the in-call dedup.
	listPage := fmt.Sprintf(`<html><head><meta charset="utf-8"><title>Lazy List</title>
<style>body{margin:0}.item{height:50px;line-height:50px;padding:0 8px;box-sizing:border-box;border-bottom:1px solid #ccc}</style>
</head><body>
<div id="list"></div>
<div class="item" id="dup">Product 1 — some details about item 1</div>
<script>
const total = %d;
let next = 21, busy = false, done = false;
const list = document.getElementById("list");
function render(n) {
  const d = document.createElement("div");
  d.className = "item";
  d.textContent = "Product " + n + " — some details about item " + n;
  list.appendChild(d);
}
async function more() {
  if (busy || done) return;
  busy = true;
  try {
    const res = await fetch("/more?from=" + next);
    if (!res.ok) return;
    const batch = await res.json();
    if (batch.length) {
      for (const n of batch) render(n);
      next = batch[batch.length - 1] + 1;
      if (next > total) done = true;
    }
  } finally { busy = false; }
}
window.addEventListener("scroll", () => {
  const de = document.scrollingElement;
  if (de.scrollTop + window.innerHeight >= de.scrollHeight - 100) {
    // The 100 ms delay simulates a stalled scroll handler (the WSLg
    // stall behind the original failure): the lazy fetch starts AFTER
    // the scroll has returned — without the trigger grace, the settle
    // declares stability before the fetch even begins and the next
    // read sees only the original 20 items.
    setTimeout(more, 100);
  }
});
for (let n = 1; n <= 20; n++) render(n);
</script>
</body></html>`, totalItems)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lazy":
			fmt.Fprint(w, listPage)
		case "/more":
			from, err := strconv.Atoi(r.URL.Query().Get("from"))
			if err != nil || from < 1 {
				http.Error(w, "bad from", http.StatusBadRequest)
				return
			}
			mu.Lock()
			fetches++
			mu.Unlock()
			time.Sleep(250 * time.Millisecond) // the render gap settle must bridge
			batch := []int{}
			for n := from; n <= totalItems && n < from+10; n++ {
				batch = append(batch, n)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(batch)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	mgr := NewManager(dataDir)
	t.Cleanup(func() {
		mgr.Close()
		time.Sleep(2 * time.Second) // let the process tree release the profile (see TestWebBrowserLive)
	})
	inst, err := mgr.ForUser(ctx, "e2e-lazy")
	if err != nil {
		t.Fatalf("ForUser (browser launch): %v", err)
	}
	tab, err := inst.OpenTab(ctx, "", srv.URL+"/lazy")
	if err != nil {
		t.Fatalf("OpenTab: %v", err)
	}

	numRe := regexp.MustCompile(`Product (\d+)`)
	numOf := func(text string) int {
		m := numRe.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("no item number in %q", text)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}

	// The initial read: 20 items in visual order, the duplicate dropped,
	// the page settled.
	pi, err := inst.ReadItems(ctx, tab.ID, ".item")
	if err != nil {
		t.Fatalf("ReadItems (initial): %v", err)
	}
	if len(pi.Items) != 20 {
		t.Fatalf("initial items = %d, want 20:\n%s", len(pi.Items), BoundItems(pi))
	}
	if pi.Dups != 1 {
		t.Fatalf("initial dups = %d, want 1 (the duplicate-text item)", pi.Dups)
	}
	if pi.Settle != SettleSettled {
		t.Fatal("the initial read must settle (a static page settles within the quiet window)")
	}
	for i, it := range pi.Items {
		if n := numOf(it.Text); n != i+1 {
			t.Fatalf("initial order: position %d = item %d, want %d (visual order, not DOM order)", i+1, n, i+1)
		}
	}

	// The model loop: scroll one viewport + readItems, until the bottom
	// is reached with nothing new. The Go-side dedupe simulates the
	// model's in-context diff (the only bookkeeping the stateless tier
	// leaves to the model).
	seen := map[string]bool{}
	var order []int
	add := func(pi *PageItems) int {
		newItems := 0
		for _, it := range pi.Items {
			if !seen[it.Text] {
				seen[it.Text] = true
				order = append(order, numOf(it.Text))
				newItems++
			}
		}
		return newItems
	}
	add(pi)

	var checkedFirst bool
	for iter := 0; iter < 10; iter++ {
		sr, err := inst.Scroll(ctx, tab.ID, "down", 1)
		if err != nil {
			t.Fatalf("scroll (iter %d): %v", iter+1, err)
		}
		pi, err := inst.ReadItems(ctx, tab.ID, ".item")
		if err != nil {
			t.Fatalf("ReadItems (iter %d): %v", iter+1, err)
		}
		// Visual order within every call (the geometry sort pin).
		for i := 1; i < len(pi.Items); i++ {
			if numOf(pi.Items[i].Text) < numOf(pi.Items[i-1].Text) {
				t.Fatalf("iter %d: items not in visual order: %d before %d", iter+1, numOf(pi.Items[i].Text), numOf(pi.Items[i-1].Text))
			}
		}
		if !checkedFirst {
			checkedFirst = true
			found30 := false
			for _, it := range pi.Items {
				if numOf(it.Text) == 30 {
					found30 = true
				}
			}
			// The first read after scrolling MUST include the async
			// batch (21-30): settle waited for the fetch + the append.
			// Without the settle, this read races the 250 ms fetch and
			// sees only the original 20 (the original failure).
			if !found30 {
				last := pi.Items[len(pi.Items)-1].Text
				t.Fatalf("the first read after scrolling missed the async-appended items (settle did not wait): %d items, last = %q", len(pi.Items), last)
			}
		}
		if sr.Bottom && add(pi) == 0 {
			break
		}
	}

	if len(order) != totalItems {
		t.Fatalf("collected %d items, want %d — content was dropped (the original failure): %v…", len(order), totalItems, order)
	}
	for i, n := range order {
		if n != i+1 {
			t.Fatalf("collection order: position %d = item %d, want %d", i+1, n, i+1)
		}
	}
	mu.Lock()
	f := fetches
	mu.Unlock()
	if f < 4 {
		t.Fatalf("fetches = %d, want >= 4 (the async lazy path must have been used)", f)
	}

	// The large-scroll nudge: a multi-viewport move (back to the top
	// from the bottom of the lazy list) carries the step-one-viewport
	// note. The assertion follows the MEASURED decision (the display
	// height varies between boxes): the wiring is what this pins —
	// Scroll sets Nudge from its own measurement and the report
	// renders it.
	sr2, err := inst.Scroll(ctx, tab.ID, "top", 1)
	if err != nil {
		t.Fatalf("scroll (top): %v", err)
	}
	if want := largeScrollNudgeNeeded(sr2.Prev, sr2.Top, sr2.Vh); sr2.Nudge != want {
		t.Fatalf("Nudge = %v, the decision says %v (prev=%.0f top=%.0f vh=%.0f)", sr2.Nudge, want, sr2.Prev, sr2.Top, sr2.Vh)
	}
	if sr2.Nudge && !strings.Contains(ScrollReport(sr2), "step one viewport at a time instead") {
		t.Fatalf("a nudged scroll must carry the nudge line: %q", ScrollReport(sr2))
	}
}

// TestWebBrowserCollectLive (design log BROWSER-ROBUST batch 2,
// 2026-11-23): collect is the TASK-SHAPED lazy-list command — one call
// does the scroll+settle+extract+dedupe loop that the model used to do
// itself (the car-research transcript: 15+ scroll/readItems pairs to
// gather one list). Same lazy page as TestWebBrowserLazyListLive: the
// single call must return the whole list in order with a stop reason —
// no model-side bookkeeping, no partial snapshot.
func TestWebBrowserCollectLive(t *testing.T) {
	if os.Getenv("BEVIN_E2E_BROWSER") != "1" {
		t.Skip("set BEVIN_E2E_BROWSER=1 to run the live browser e2e (needs a desktop host with a browser)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const totalItems = 60
	var mu sync.Mutex
	fetches := 0
	// The same lazy-list page as the manual-loop test (20 sync, then
	// batches of 10 after a 250 ms fetch on scroll-near-bottom, plus the
	// duplicate-text item).
	listPage := fmt.Sprintf(`<html><head><meta charset="utf-8"><title>Lazy List</title>
<style>body{margin:0}.item{height:50px;line-height:50px;padding:0 8px;box-sizing:border-box;border-bottom:1px solid #ccc}</style>
</head><body>
<div id="list"></div>
<div class="item" id="dup">Product 1 — some details about item 1</div>
<script>
const total = %d;
let next = 21, busy = false, done = false;
const list = document.getElementById("list");
function render(n) {
  const d = document.createElement("div");
  d.className = "item";
  d.textContent = "Product " + n + " — some details about item " + n;
  list.appendChild(d);
}
async function more() {
  if (busy || done) return;
  busy = true;
  try {
    const res = await fetch("/more?from=" + next);
    if (!res.ok) return;
    const batch = await res.json();
    if (batch.length) {
      for (const n of batch) render(n);
      next = batch[batch.length - 1] + 1;
      if (next > total) done = true;
    }
  } finally { busy = false; }
}
window.addEventListener("scroll", () => {
  const de = document.scrollingElement;
  if (de.scrollTop + window.innerHeight >= de.scrollHeight - 100) {
    setTimeout(more, 100);
  }
});
for (let n = 1; n <= 20; n++) render(n);
</script>
</body></html>`, totalItems)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lazy":
			fmt.Fprint(w, listPage)
		case "/more":
			from, err := strconv.Atoi(r.URL.Query().Get("from"))
			if err != nil || from < 1 {
				http.Error(w, "bad from", http.StatusBadRequest)
				return
			}
			mu.Lock()
			fetches++
			mu.Unlock()
			time.Sleep(250 * time.Millisecond)
			batch := []int{}
			for n := from; n <= totalItems && n < from+10; n++ {
				batch = append(batch, n)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(batch)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	mgr := NewManager(dataDir)
	t.Cleanup(func() {
		mgr.Close()
		time.Sleep(2 * time.Second)
	})
	inst, err := mgr.ForUser(ctx, "e2e-collect")
	if err != nil {
		t.Fatalf("ForUser (browser launch): %v", err)
	}
	tab, err := inst.OpenTab(ctx, "", srv.URL+"/lazy")
	if err != nil {
		t.Fatalf("OpenTab: %v", err)
	}

	// THE pin: one call, the whole list.
	cr, err := inst.Collect(ctx, tab.ID, ".item")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cr.Stop != "bottom" {
		t.Fatalf("stop = %q, want bottom (the list is finite): %d items", cr.Stop, len(cr.Items))
	}
	if len(cr.Items) != totalItems {
		t.Fatalf("collected %d items, want %d — content was dropped inside one call:\\n%.400s", len(cr.Items), totalItems, FormatCollect(cr))
	}
	// In order, deduped (the duplicate-text item collapsed into #1).
	numRe := regexp.MustCompile(`Product (\d+)`)
	for i, it := range cr.Items {
		m := numRe.FindStringSubmatch(it.Text)
		if m == nil {
			t.Fatalf("position %d is not a list item: %q", i+1, it.Text)
		}
		n, _ := strconv.Atoi(m[1])
		if n != i+1 {
			t.Fatalf("collection order: position %d = item %d, want %d", i+1, n, i+1)
		}
	}
	// The lazy path was actually exercised (async fetches, not the sync
	// initial batch alone).
	mu.Lock()
	f := fetches
	mu.Unlock()
	if f < 4 {
		t.Fatalf("fetches = %d, want >= 4 (the async lazy path must have been used)", f)
	}
	t.Logf("collect: %d items (stop: %s, %d fetches)", len(cr.Items), cr.Stop, f)
}

// TestWebBrowserSelectorVocabLive (design log BROWSER-ROBUST batch 3,
// 2026-11-24 — the car-research redo's failures): the model writes
// Playwright selector forms by habit (button:has-text("See Inventory")
// came straight back as a querySelector SyntaxError) and clicks controls
// a page renders as plain divs. Decision 2026-11-24: the tool does NOT
// accept the Playwright forms (a working form would teach the model to
// write more — recreating Playwright's surface); it answers with one
// line pointing at the tool's own vocabulary (#N, CSS, the element's
// visible text) — and the bare-text form now actually RESOLVES for
// clicks (interactive name first, then a rescue across all visible
// elements for the div-rendered controls). Pins: the Playwright-form
// rejection (the redirect wording, not a SyntaxError, not a click), the
// bare-text click (a button via the name path; a div control via the
// rescue), the click-only strictness (type/fill keys stay interactive),
// the ambiguity refusal (never a guess), the contains stage (partial
// text → innermost element), and the ElementInteractivity probe behind
// the click/type notes.
func TestWebBrowserSelectorVocabLive(t *testing.T) {
	if os.Getenv("BEVIN_E2E_BROWSER") != "1" {
		t.Skip("set BEVIN_E2E_BROWSER=1 to run the live browser e2e (needs a desktop host with a browser)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The page: a button wrapped in a div carrying the SAME text (the
	// innermost-wins case for the rescue), a non-interactive div control
	// (the "See Inventory" case — a JS-attached handler, no role, no
	// cursor), two same-named buttons (ambiguity), and a unique
	// paragraph (the contains stage: a partial text resolves to the
	// innermost element carrying it).
	page := `<html><head><meta charset="utf-8"><title>Selector Vocab</title></head><body>
<h1>Forms Page</h1>
<p>Static content here.</p>
<div id="wrap"><button id="see-btn" onclick="window.__seen = true">See Inventory</button></div>
<div id="sic">See Inventory 2</div>
<div id="saves"><button id="save1">Save</button><button id="save2">Save</button></div>
<p id="uniq">UniqueHeadingHere</p>
<input id="q" type="text" placeholder="search">
<script>
document.getElementById("sic").addEventListener("click", () => { window.__sic = true; });
</script>
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	mgr := NewManager(dataDir)
	t.Cleanup(func() {
		mgr.Close()
		time.Sleep(2 * time.Second) // let the process tree release the profile
	})
	inst, err := mgr.ForUser(ctx, "e2e-forms")
	if err != nil {
		t.Fatalf("ForUser (browser launch): %v", err)
	}
	tab, err := inst.OpenTab(ctx, "", srv.URL)
	if err != nil {
		t.Fatalf("OpenTab: %v", err)
	}
	session, err := inst.SessionFor(ctx, tab.ID)
	if err != nil {
		t.Fatalf("SessionFor: %v", err)
	}
	flag := func(js string) bool {
		v, err := inst.Evaluate(ctx, session, js)
		if err != nil {
			t.Fatalf("Evaluate(%s): %v", js, err)
		}
		return strings.TrimSpace(string(v)) == "true"
	}

	// THE observed failure, now a REDIRECT: the model's exact Playwright
	// form is refused with one line pointing at the tool's vocabulary —
	// not a raw SyntaxError, and not a click.
	if _, _, _, err := inst.ElementTarget(ctx, session, "", `button:has-text("See Inventory")`, true); err == nil || !strings.Contains(err.Error(), "Playwright") || !strings.Contains(err.Error(), "visible text") {
		t.Fatalf("ElementTarget(:has-text) err = %v, want the Playwright-redirect wording (not a SyntaxError, not a resolution)", err)
	}

	// The tool's own vocabulary for the same task: the element's visible
	// text, bare. The wrapper div carries the SAME text as the button —
	// the name path (interactive candidates only) sees just the button;
	// the click must land on it.
	x, y, sel, err := inst.ElementTarget(ctx, session, "", "See Inventory", true)
	if err != nil {
		t.Fatalf("ElementTarget(bare text) = %v — the tool's own text form must resolve", err)
	}
	if !strings.Contains(sel, "see-btn") {
		t.Fatalf("bare-text resolved %q — want see-btn, not the same-text wrapper", sel)
	}
	if err := inst.ClickAt(ctx, session, x, y); err != nil {
		t.Fatalf("ClickAt: %v", err)
	}
	if !flag("window.__seen") {
		t.Fatal("the bare-text click did not land on the button (window.__seen unset)")
	}

	// The div control (the redo's stuck case): no interactive element
	// bears the name — the click-only rescue finds the div among ALL
	// visible elements; the click still LANDS (real input events — the
	// page's JS handler fires).
	x, y, sel, err = inst.ElementTarget(ctx, session, "", "See Inventory 2", true)
	if err != nil {
		t.Fatalf("ElementTarget(div text, rescue) = %v — the div-rendered control must resolve for clicks", err)
	}
	if !strings.Contains(sel, "sic") {
		t.Fatalf("rescue resolved %q, want the sic div", sel)
	}
	if err := inst.ClickAt(ctx, session, x, y); err != nil {
		t.Fatalf("ClickAt: %v", err)
	}
	if !flag("window.__sic") {
		t.Fatal("the rescue click did not land on the div (window.__sic unset)")
	}

	// Click-only strictness: the SAME key with the rescue off (type/
	// select/fill semantics) must NOT resolve a non-interactive div —
	// typing into a heading would be a silent no-op.
	if _, _, _, err := inst.ElementTarget(ctx, session, "", "See Inventory 2", false); err == nil {
		t.Fatal("ElementTarget(div text, no rescue) resolved — type/fill keys must stay strict (interactive names only)")
	}

	// The interactivity probe (behind the click/type notes): the button
	// is interactive, the div control is NOT (the note the model needed
	// instead of two re-reads), the input is text-like, the paragraph
	// is not.
	if tag, interactive, _, err := inst.ElementInteractivity(ctx, session, "#see-btn"); err != nil || tag != "button" || !interactive {
		t.Fatalf("ElementInteractivity(#see-btn) = (%q, %v, %v), want (button, true)", tag, interactive, err)
	}
	if tag, interactive, _, err := inst.ElementInteractivity(ctx, session, "#sic"); err != nil || tag != "div" || interactive {
		t.Fatalf("ElementInteractivity(#sic) = (%q, %v, %v), want (div, false) — a plain div is not interactive", tag, interactive, err)
	}
	if tag, _, textLike, err := inst.ElementInteractivity(ctx, session, "#q"); err != nil || tag != "input" || !textLike {
		t.Fatalf("ElementInteractivity(#q) = (%q, %v, %v), want (input, textLike=true)", tag, textLike, err)
	}
	if tag, _, textLike, err := inst.ElementInteractivity(ctx, session, "#uniq"); err != nil || tag != "p" || textLike {
		t.Fatalf("ElementInteractivity(#uniq) = (%q, %v, %v), want (p, textLike=false)", tag, textLike, err)
	}

	// Ambiguity: two same-named buttons — the name path refuses, never
	// guessed (the bare-text form inherits the same refusal).
	if _, _, _, err := inst.ElementTarget(ctx, session, "", "Save", true); err == nil || !strings.Contains(err.Error(), "multiple elements match") {
		t.Fatalf("ambiguous bare text err = %v, want the multiple-match refusal", err)
	}

	// The contains stage: a PARTIAL text (a phrase copied from the page
	// that is no element's full text) resolves to the innermost element
	// carrying it — not the ancestors, not a guess.
	if _, _, sel, err := inst.ElementTarget(ctx, session, "", "Heading", true); err != nil || !strings.Contains(sel, "uniq") {
		t.Fatalf("partial-text = (%q, %v), want the uniq paragraph (contains stage, innermost)", sel, err)
	}

	// Missing text: an honest not-found (no partial guesses).
	if _, _, _, err := inst.ElementTarget(ctx, session, "", "No Such Text Here", true); err == nil || !strings.Contains(err.Error(), "no visible element says") {
		t.Fatalf("missing text err = %v, want the not-found wording", err)
	}

	// The Playwright forms the tool refuses — one clear line each.
	if _, _, _, err := inst.ElementTarget(ctx, session, "", "id=sic", true); err == nil || !strings.Contains(err.Error(), "#sic") {
		t.Fatalf("ElementTarget(id=) err = %v, want the #sic redirect", err)
	}
	if _, _, _, err := inst.ElementTarget(ctx, session, "", "css=#see-btn", true); err == nil || !strings.Contains(err.Error(), "css=") {
		t.Fatalf("ElementTarget(css=) err = %v, want the drop-the-prefix redirect", err)
	}
	for _, key := range []string{"text=Save", "xpath=//button", "//button", "aria/Save", "role=button"} {
		if _, _, _, err := inst.ElementTarget(ctx, session, "", key, true); err == nil || !strings.Contains(err.Error(), "Playwright") {
			t.Fatalf("ElementTarget(%q) err = %v, want the Playwright-redirect wording (not a SyntaxError)", key, err)
		}
	}
}

// TestWebBrowserReadSelectorRetryLive (design log BROWSER-ROBUST batch 3,
// 2026-11-24 — the car-research redo's failure): a read with a selector
// right after a navigate hit "selector not found" because the SPA's
// content was still rendering — the model paid a second call to find out.
// Pins: a not-found read WAITS (bounded 5 s) and re-extracts once — a
// heading that lands 3.5 s after load is in the SAME call's result — and
// a selector the page never uses fails with the actionable wording (read
// without a selector).
func TestWebBrowserReadSelectorRetryLive(t *testing.T) {
	if os.Getenv("BEVIN_E2E_BROWSER") != "1" {
		t.Skip("set BEVIN_E2E_BROWSER=1 to run the live browser e2e (needs a desktop host with a browser)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	page := `<html><head><meta charset="utf-8"><title>Late Render</title></head><body>
<h1>Early Heading</h1>
<p>Static content here.</p>
<script>setTimeout(() => {
  const h = document.createElement("h1");
  h.id = "late";
  h.textContent = "LateHeading";
  document.body.appendChild(h);
}, 3500);</script>
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	mgr := NewManager(dataDir)
	t.Cleanup(func() {
		mgr.Close()
		time.Sleep(2 * time.Second)
	})
	inst, err := mgr.ForUser(ctx, "e2e-laterender")
	if err != nil {
		t.Fatalf("ForUser (browser launch): %v", err)
	}
	tab, err := inst.OpenTab(ctx, "", srv.URL)
	if err != nil {
		t.Fatalf("OpenTab: %v", err)
	}

	// THE pin: the heading lands 3.5 s after load (well after the
	// settle) — the read must still come back WITH it (the retry waited
	// and re-extracted inside the same call).
	start := time.Now()
	pr, err := inst.Read(ctx, "", tab.ID, "#late")
	if err != nil {
		t.Fatalf("Read(#late) = %v — the retry must catch the late-rendered heading", err)
	}
	if !strings.Contains(pr.Markdown, "LateHeading") {
		t.Fatalf("read missing the late-rendered heading (the retry did not re-extract):\\n%.300s", pr.Markdown)
	}
	if took := time.Since(start); took < 3*time.Second {
		t.Logf("read returned in %s (the first extract may already have seen the heading — the pin still holds)", took)
	}

	// A selector the page never uses: fails after the bounded wait, with
	// the actionable wording (not a bare "not found").
	if _, err := inst.Read(ctx, "", tab.ID, "table"); err == nil || !strings.Contains(err.Error(), "waited 5s") || !strings.Contains(err.Error(), "read without a selector") {
		t.Fatalf("Read(table) err = %v, want the waited-then-not-found wording with the read-without-a-selector pointer", err)
	}
}
