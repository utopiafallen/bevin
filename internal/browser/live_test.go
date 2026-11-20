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
	page := `<html><head><title>Bevin E2E</title></head><body>
<h1>E2E Page</h1>
<p>Some paragraph text about testing browsers.</p>
<a href="about:blank" id="other-link">go elsewhere</a>
<button id="toggle" onclick="var d=document.getElementById('after');d.style.display='block';d.textContent='toggled!'">toggle</button>
<div id="after" style="display:none"></div>
<input id="q" type="text" placeholder="search">
<select id="pick"><option value="a">Option A</option><option value="b">Option B</option></select>
</body></html>`
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
		t.Fatalf("the toggle button is not in the element table:\n%s", BoundRead(pr))
	}
	if _, ok := inst.ElementByN("", toggle.N); !ok {
		t.Fatal("the read's element table is not resolvable by #N")
	}
	if in := find("textbox", "search"); in.N < 1 {
		t.Fatalf("the search input is not in the element table:\n%s", BoundRead(pr))
	}
	if link := find("link", "go elsewhere"); link.N < 1 {
		t.Fatalf("the link is not in the element table:\n%s", BoundRead(pr))
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
	if !pi.Settled {
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
