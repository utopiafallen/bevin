package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	tab, err := inst.OpenTab(ctx, srv.URL)
	if err != nil {
		t.Fatalf("OpenTab: %v", err)
	}
	if tab.ID == "" {
		t.Fatal("OpenTab returned no tab id")
	}

	// read: the clean page — title/url, the markdown (heading + paragraph),
	// and the element table with the expected entries.
	pr, err := inst.Read(ctx, tab.ID, "")
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
	if _, ok := inst.ElementByN(toggle.N); !ok {
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
	ok, took, err := inst.Wait(ctx, tab.ID, "#after", "", 15*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !ok {
		t.Fatalf("the toggle click had no effect after %s (the click did not land)", took)
	}

	// read again: the revealed text is in the clean now (the page changed).
	pr2, err := inst.Read(ctx, tab.ID, "")
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
	if _, err := inst.CloseTab(ctx, tab.ID); err != nil {
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
