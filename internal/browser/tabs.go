package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
)

// Tab is one page target of the instance's browser (type=page — extension
// background pages and devtools frontends are filtered out).
type Tab struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// ListTabs returns the browser's page targets.
func (i *Instance) ListTabs(ctx context.Context) ([]Tab, error) {
	body, err := httpGet(ctx, fmt.Sprintf("http://127.0.0.1:%d/json", i.port))
	if err != nil {
		return nil, err
	}
	var targets []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("parsing the CDP target list: %w", err)
	}
	tabs := []Tab{}
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		// Browser-INTERNAL pages (a sync prompt, a new-tab page, devtools,
		// extension backgrounds): not browsable content for the tool —
		// they would only clutter the tab list.
		if strings.HasPrefix(t.URL, "chrome://") || strings.HasPrefix(t.URL, "edge://") || strings.HasPrefix(t.URL, "devtools://") || strings.HasPrefix(t.URL, "chrome-extension://") {
			continue
		}
		tabs = append(tabs, Tab{ID: t.ID, Title: t.Title, URL: t.URL})
	}
	return tabs, nil
}

// allowedBrowseURL validates (and normalizes) the url web_browser points
// the browser at: only http and https (plus about:blank). This is a
// security gate — file:// would let a prompt-injected page (or the model
// obeying one) read arbitrary local files into the remote model's context,
// and data:/javascript:/view-source:/browser-internal schemes can execute
// or smuggle content. A schemeless "host/path" is normalized to https://.
func allowedBrowseURL(raw string) (string, error) {
	if raw == "" || raw == "about:blank" {
		return "about:blank", nil
	}
	if !strings.Contains(raw, ":") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %v", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
		return raw, nil
	default:
		return "", fmt.Errorf("url scheme %q is not allowed (only http and https pages can be opened)", u.Scheme)
	}
}

// OpenTab creates a new page target (about:blank when url is empty) and
// makes it the CALLING conversation's working tab (the tab-less default for
// that conversation — conversations of the same user share the instance, so
// the working tab is per-conversation, never instance-global). With a url it
// then navigates EXPLICITLY
// (Page.navigate + the load wait): createTarget(url) opens the target at
// about:blank first and the navigation lands asynchronously — trusting the
// initial readyState would let the first read race the navigation (the
// Chromium race seen in the live e2e). A failed navigation (DNS, etc.)
// returns the tab (it exists, at about:blank) WITH the error — the model
// can retry.
func (i *Instance) OpenTab(ctx context.Context, convID, url string) (Tab, error) {
	if url == "" {
		url = "about:blank"
	}
	u, err := allowedBrowseURL(url)
	if err != nil {
		return Tab{}, err
	}
	url = u
	res, err := i.cdp.Call(ctx, "", "Target.createTarget", map[string]any{"url": url})
	if err != nil {
		return Tab{}, err
	}
	var out struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(res, &out); err != nil || out.TargetID == "" {
		return Tab{}, fmt.Errorf("Target.createTarget did not return a target id")
	}
	i.mu.Lock()
	i.convTab[convID] = out.TargetID
	i.mu.Unlock()
	i.restarted.Store(false) // a first use: the restart wording has served
	tab := Tab{ID: out.TargetID, URL: url}
	if url != "about:blank" {
		if nerr := i.Navigate(ctx, out.TargetID, url); nerr != nil {
			tab.URL = "about:blank"
			return tab, nerr
		}
		tab.Title = i.Title(ctx, out.TargetID)
	}
	return tab, nil
}

// CloseTab closes a page target (the CALLING conversation's working tab when
// id is empty). Closing a conversation's working tab unpins it — the
// conversation's next tab-less command gets the open-a-tab-first signal.
func (i *Instance) CloseTab(ctx context.Context, convID, id string) (string, error) {
	if id == "" {
		var err error
		id, err = i.ResolveTab(ctx, convID, "")
		if err != nil {
			return "", err
		}
	}
	res, err := i.cdp.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": id})
	if err != nil {
		return id, err
	}
	var out struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal(res, &out)
	i.mu.Lock()
	if s, ok := i.sessions[id]; ok {
		delete(i.netState, s) // the tab's in-flight counter goes with it
	}
	delete(i.sessions, id)
	if i.convTab[convID] == id {
		delete(i.convTab, convID)
	}
	i.mu.Unlock()
	return id, nil
}

// ResolveTab resolves a command's tab argument for the CALLING conversation
// (the instance is shared by all of the user's conversations):
//   - an explicit id is used as-is and PINS the conversation's working tab
//     (an explicit choice is the conversation's own);
//   - an empty id resolves to the conversation's working tab — the last tab
//     IT opened or named — and errors when the conversation has none (a
//     conversation never silently inherits another conversation's tab);
//   - a pinned tab is verified live: one closed meanwhile (by another
//     conversation, or by the user watching the window) is unpinned and
//     reported as a clean error, not a raw CDP failure.
func (i *Instance) ResolveTab(ctx context.Context, convID, id string) (string, error) {
	pinned := false
	i.mu.Lock()
	if id == "" {
		id = i.convTab[convID]
		pinned = true
		if id == "" {
			i.mu.Unlock()
			// A restarted instance (the CDP endpoint died and the manager
			// relaunched) has NO tabs — but the model's conversation
			// remembers opening one, so the plain "no tab yet" wording
			// reads as a lie. The coarse flag clears on the first
			// successful openTab/navigate (see OpenTab/Navigate).
			if i.restarted.Load() {
				return "", errors.New("the browser just restarted — its previous tabs are gone; openTab again")
			}
			return "", errors.New("this conversation has no tab yet — openTab first (listTabs shows the existing tabs if you want to work in one)")
		}
	} else {
		i.convTab[convID] = id
	}
	i.mu.Unlock()
	if !pinned {
		return id, nil
	}
	tabs, err := i.ListTabs(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range tabs {
		if t.ID == id {
			return id, nil
		}
	}
	i.mu.Lock()
	delete(i.convTab, convID)
	i.mu.Unlock()
	return "", errors.New("the tab this conversation was using was closed — openTab first (or name one from listTabs)")
}

// sessionFor returns the flattened CDP session for a tab target
// (Target.attachToTarget, cached per tab — the session lives with the
// target).
func (i *Instance) SessionFor(ctx context.Context, tabID string) (string, error) {
	i.mu.Lock()
	s, ok := i.sessions[tabID]
	i.mu.Unlock()
	if ok {
		return s, nil
	}
	res, err := i.cdp.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": tabID, "flatten": true})
	if err != nil {
		return "", err
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &out); err != nil || out.SessionID == "" {
		return "", fmt.Errorf("Target.attachToTarget did not return a session id")
	}
	i.mu.Lock()
	i.sessions[tabID] = out.SessionID
	i.mu.Unlock()
	// The Network domain from the first attach: the settle counter
	// (in-flight requests) sees every request from then on. A failure is
	// non-fatal — settling then relies on the mutation half only.
	if _, err := i.cdp.Call(ctx, out.SessionID, "Network.enable", nil); err != nil {
		log.Printf("bevin: Network.enable (settle counter): %v", err)
	}
	i.netCounterFor(out.SessionID) // route events from the first attach
	return out.SessionID, nil
}

// evaluate runs an expression in the tab's page and returns the value
// (returnByValue — a JSON-able result; undefined/complex values come back
// as their string form).
func (i *Instance) Evaluate(ctx context.Context, sessionID, expression string) (json.RawMessage, error) {
	res, err := i.cdp.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Type        string          `json:"type"`
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("parsing Runtime.evaluate: %w", err)
	}
	if out.ExceptionDetails != nil {
		text := out.ExceptionDetails.Text
		if out.Result.Description != "" {
			text += ": " + out.Result.Description
		}
		return nil, fmt.Errorf("page script error: %s", text)
	}
	if out.Result.Type == "undefined" {
		return json.RawMessage("null"), nil
	}
	if out.Result.Value != nil {
		return out.Result.Value, nil
	}
	return json.RawMessage("null"), nil
}

// evaluateString is evaluate + "must be a JSON string" (the cleaner and the
// element-probe helpers return strings).
func (i *Instance) evaluateString(ctx context.Context, sessionID, expression string) (string, error) {
	v, err := i.Evaluate(ctx, sessionID, expression)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", fmt.Errorf("page script returned %s, want a string", string(v))
	}
	return s, nil
}

// Navigate sends the tab to url and waits for the load to settle
// (document.readyState → complete, polled — v1's event-free wait). A
// navigation that fails in the page (DNS, TLS, a hard 404 is still a load)
// reports the page's errorText when the browser gives one.
func (i *Instance) Navigate(ctx context.Context, tabID, url string) error {
	if url == "" {
		return fmt.Errorf("navigate needs a url")
	}
	u, err := allowedBrowseURL(url)
	if err != nil {
		return err
	}
	url = u
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return err
	}
	res, err := i.cdp.Call(ctx, session, "Page.navigate", map[string]any{"url": url})
	if err != nil {
		return err
	}
	var out struct {
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(res, &out); err == nil && out.ErrorText != "" {
		return fmt.Errorf("navigation failed: %s", out.ErrorText)
	}
	err = i.waitForLoad(ctx, session)
	i.restarted.Store(false) // a first use: the restart wording has served
	return err
}

// Title reads the tab's current document.title (the openTab/navigate
// results carry it — a 404's title is visible without burning a read).
// Best-effort: a probe failure yields "" (the result renders the plain
// "loaded" form).
func (i *Instance) Title(ctx context.Context, tabID string) string {
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return ""
	}
	t, err := i.evaluateString(ctx, session, "JSON.stringify(document.title)")
	if err != nil {
		return ""
	}
	t = strings.TrimSpace(t)
	if len(t) > 120 {
		t = t[:117] + "..."
	}
	return t
}

// waitForLoad polls document.readyState until complete (30 s ceiling).
func (i *Instance) waitForLoad(ctx context.Context, session string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, err := i.Evaluate(ctx, session, "document.readyState")
		if err == nil {
			var state string
			if json.Unmarshal(v, &state) == nil && (state == "complete" || state == "interactive") {
				if state == "complete" {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the page did not finish loading within 30 s (it may still be rendering — proceed and re-read if needed)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Wait polls until a selector is present + visible (or the timeout
// passes). Returns how long it took. Selector-only: waitFor's expression
// mode (model-controlled JS in the page) was removed 2026-11-19 —
// security; the harness's own probes keep using Evaluate directly.
func (i *Instance) Wait(ctx context.Context, tabID, selector string, timeout time.Duration) (bool, time.Duration, error) {
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return false, 0, err
	}
	if selector == "" {
		return false, 0, fmt.Errorf("waitFor needs a selector")
	}
	sel, _ := json.Marshal(selector) // a JS string literal (quote-safe)
	probe := fmt.Sprintf("(() => { const el = document.querySelector(%s); if (!el) return false; const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; })()", sel)
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		v, err := i.Evaluate(ctx, session, probe)
		if err == nil {
			var ok bool
			if json.Unmarshal(v, &ok) == nil && ok {
				return true, time.Since(start), nil
			}
		}
		if time.Now().After(deadline) {
			return false, time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return false, time.Since(start), ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// elementCenter resolves a selector to the element's viewport center
// (scrolling it into view first) — the coordinate target of the input
// events. "not found" / "not visible" are distinct outcomes so the tool can
// tell the model to re-read (the element table may be stale).
func (i *Instance) ElementCenter(ctx context.Context, session, selector string) (x, y float64, err error) {
	sel, _ := json.Marshal(selector)
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%s);
		if (!el) return "not found";
		el.scrollIntoView({ block: "center" });
		const r = el.getBoundingClientRect();
		if (r.width === 0 || r.height === 0) return "not visible";
		return JSON.stringify({ x: r.left + r.width / 2, y: r.top + r.height / 2 });
	})()`, sel)
	s, err := i.evaluateString(ctx, session, expr)
	if err != nil {
		return 0, 0, err
	}
	if s == "not found" {
		return 0, 0, fmt.Errorf("element not found (the page may have changed since the last read — read again and use a fresh #N)")
	}
	if s == "not visible" {
		return 0, 0, fmt.Errorf("element not visible (zero size — it may be hidden or off-canvas; try another element)")
	}
	var c struct {
		X, Y float64
	}
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return 0, 0, fmt.Errorf("element geometry: %w", err)
	}
	return c.X, c.Y, nil
}

// elementCenterRefJS resolves a #N element ref against the LIVE page,
// tolerating a page that RE-RENDERED after the read (the observed failure
// on carousel-heavy sites — 10 of 12 clicks in the first research session
// died when the stored structural selector stopped matching): the stored
// selector first — trusted only if it still points at the RECORDED
// element (an id selector always, an nth-of-type path only if the hit's
// tag + name still match: a re-render that keeps the path's shape can
// match a DIFFERENT element) — then a (tag + name) re-match among the
// page's visible INTERACTIVE elements of that tag (the same candidate
// set the read listed). The recorded name is what the read saw (the
// read's derivation, mirrored here: aria-label → title → text → the
// input fallbacks, 80-char truncated); exact match before
// case-insensitive. More than one candidate at the best stage is
// reported as ambiguous — never guessed.
// Returns {x, y, selector} (a fresh path selector for the resolved
// element — select needs it) or {why: "missing" | "multiple"}.
// %s x3 = selector, name, tag (JSON string literals — the pageCleanerJS
// pattern).
var elementCenterRefJS = `(() => {
  const sel = %s, name = %s, tag = %s;
  const norm = s => String(s || "").replace(/\s+/g, " ").trim();
  function visible(el) {
    const s = getComputedStyle(el);
    return s.display !== "none" && s.visibility !== "hidden";
  }
  // The read's name derivation (pageCleanerJS, mirrored — the stored
  // name must be compared the same way it was made).
  function nameOf(el) {
    let n = norm(el.getAttribute("aria-label"));
    if (!n) n = norm(el.getAttribute("title"));
    if (!n && el.tagName.toLowerCase() !== "input") n = norm(el.textContent);
    if (!n && el.tagName.toLowerCase() === "input") n = norm(el.placeholder || el.value || el.name || el.type || "input");
    return String(n).slice(0, 80);
  }
  function pathOf(el) {
    const parts = [];
    let n = el;
    while (n && n.nodeType === 1 && n.tagName.toLowerCase() !== "body" && parts.length < 8) {
      if (n.id) { parts.unshift("#" + CSS.escape(n.id)); break; }
      let part = n.tagName.toLowerCase();
      const parent = n.parentElement;
      if (parent) {
        const sibs = Array.from(parent.children).filter(c => c.tagName === n.tagName);
        if (sibs.length > 1) part += ":nth-of-type(" + (sibs.indexOf(n) + 1) + ")";
      }
      parts.unshift(part);
      n = parent;
    }
    return "body > " + parts.join(" > ");
  }
  function centerOf(el) {
    el.scrollIntoView({ block: "center" });
    const r = el.getBoundingClientRect();
    return JSON.stringify({ x: r.left + r.width / 2, y: r.top + r.height / 2, selector: pathOf(el) });
  }
  let el = null;
  try { el = document.querySelector(sel); } catch (e) {}
  if (el && visible(el) && el.getBoundingClientRect().width > 0) {
    const same = el.tagName.toLowerCase() === tag && nameOf(el) === norm(name);
    if (sel.startsWith("#") || same) return centerOf(el);
  }
  const nm = norm(name);
  if (!nm) return JSON.stringify({ why: "missing" });
  const INTERACTIVE = 'a[href],button,[role="button"],[role="link"],input,select,textarea,summary,[onclick],label[for]';
  let cands = [];
  try { cands = Array.from(document.querySelectorAll(INTERACTIVE)).filter(c => (tag === "*" || c.tagName.toLowerCase() === tag) && visible(c)); } catch (e) {}
  const stages = [
    c => nameOf(c) === nm,
    c => nameOf(c).toLowerCase() === nm.toLowerCase()
  ];
  for (const stage of stages) {
    const hits = cands.filter(stage);
    if (hits.length === 1) return centerOf(hits[0]);
    if (hits.length > 1) return JSON.stringify({ why: "multiple" });
  }
  return JSON.stringify({ why: "missing" });
})()`

// ResolveRef resolves a #N element ref for input events (see
// elementCenterRefJS): the stored selector when it still matches, a
// tag+name re-match when the page re-rendered. The returned selector is
// fresh (select re-queries it).
func (i *Instance) ResolveRef(ctx context.Context, session string, ref elementRef) (x, y float64, selector string, err error) {
	sel, _ := json.Marshal(ref.Selector)
	name, _ := json.Marshal(ref.Name)
	tag, _ := json.Marshal(ref.Tag)
	body, err := i.evaluateString(ctx, session, fmt.Sprintf(elementCenterRefJS, sel, name, tag))
	if err != nil {
		return 0, 0, "", err
	}
	var res struct {
		X        float64 `json:"x"`
		Y        float64 `json:"y"`
		Selector string  `json:"selector"`
		Why      string  `json:"why"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return 0, 0, "", fmt.Errorf("element geometry: %w", err)
	}
	switch res.Why {
	case "":
		return res.X, res.Y, res.Selector, nil
	case "multiple":
		return 0, 0, "", fmt.Errorf("multiple elements match %q — read again and use a fresh #N, or a more specific selector", ref.Name)
	default:
		return 0, 0, "", fmt.Errorf("element not found (the page may have changed since the last read — read again and use a fresh #N)")
	}
}

// elementByTextJS resolves a TEXT target — the click-only rescue in
// ElementTarget for a key that is not valid CSS and matches no
// interactive name (the "click the thing that says X" task; the observed
// car-research case — the page rendered the "See Inventory" control as a
// plain div, so it is in no element table and no interactive name set;
// the click lands, and the interactivity note tells the model whether
// anything will happen). It searches ALL visible elements — not just the
// interactive set — but only after the strict name path missed.
// Innermost wins: an element whose text is matched only because a
// MATCHED CHILD carries it is not itself the target (nesting is the
// ambiguity a naive text match produces on real pages). Stages mirror
// the #N re-match: exact normalized text, then case-insensitive, then
// case-insensitive CONTAINS (a partial text copied from the page
// resolves the same way the model expects). More than one candidate at
// any stage is "multiple" — never guessed.
// %s x2 = text, tag (JSON string literals; tag "" = any element).
var elementByTextJS = `(() => {
  const text = %s, tag = %s;
  const norm = s => String(s || "").replace(/\s+/g, " ").trim();
  const nm = norm(text);
  if (!nm) return JSON.stringify({ why: "missing" });
  function visible(el) {
    const s = getComputedStyle(el);
    return s.display !== "none" && s.visibility !== "hidden";
  }
  function pathOf(el) {
    const parts = [];
    let n = el;
    while (n && n.nodeType === 1 && n.tagName.toLowerCase() !== "body" && parts.length < 8) {
      if (n.id) { parts.unshift("#" + CSS.escape(n.id)); break; }
      let part = n.tagName.toLowerCase();
      const parent = n.parentElement;
      if (parent) {
        const sibs = Array.from(parent.children).filter(c => c.tagName === n.tagName);
        if (sibs.length > 1) part += ":nth-of-type(" + (sibs.indexOf(n) + 1) + ")";
      }
      parts.unshift(part);
      n = n.parentElement;
    }
    return "body > " + parts.join(" > ");
  }
  const cands = [];
  for (const el of document.querySelectorAll("body *")) {
    if (tag !== "" && el.tagName.toLowerCase() !== tag) continue;
    if (!visible(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    cands.push({ el: el, text: norm(el.textContent) });
  }
  const lower = cands.map(c => c.text.toLowerCase());
  const nmLower = nm.toLowerCase();
  const stages = [
    i => cands[i].text === nm,
    i => lower[i] === nmLower,
    i => lower[i].includes(nmLower)
  ];
  for (const stage of stages) {
    const hits = [];
    for (let i = 0; i < cands.length; i++) {
      if (stage(i)) hits.push(cands[i].el);
    }
    if (!hits.length) continue;
    const inner = hits.filter(h => !hits.some(o => o !== h && h.contains(o)));
    const list = inner.length ? inner : hits;
    if (list.length > 1) return JSON.stringify({ why: "multiple" });
    const el = list[0];
    el.scrollIntoView({ block: "center" });
    const r = el.getBoundingClientRect();
    return JSON.stringify({ x: r.left + r.width / 2, y: r.top + r.height / 2, selector: pathOf(el) });
  }
  return JSON.stringify({ why: "missing" });
})()`

// elementInteractivityJS reports whether the element at a selector is
// the kind that an input action applies to (the probe behind the
// click/type notes — the model clicked a "See Inventory" control the
// page rendered as a div and read the unchanged page twice before trying
// the form submit; the note tells it, IN THE RESULT, that the element
// won't react). %s = the selector (JSON string literal).
var elementInteractivityJS = `(() => {
	const el = document.querySelector(%s);
	if (!el) return JSON.stringify({ why: "missing" });
	const tag = el.tagName.toLowerCase();
	const INTERACTIVE = 'a[href],button,input,select,textarea,summary,[onclick],label[for],[role="button"],[role="link"]';
	const interactive = !!(el.matches(INTERACTIVE) || getComputedStyle(el).cursor === "pointer");
	const textLike = (tag === "input" && !["button","checkbox","radio","submit","reset","file","hidden","range","color","image"].includes(el.type)) || tag === "textarea" || el.isContentEditable;
	return JSON.stringify({ tag: tag, interactive: interactive, textLike: textLike });
})()`

// ElementInteractivity probes a resolved element (see
// elementInteractivityJS): tag + whether a click applies (interactive)
// or a type applies (textLike).
func (i *Instance) ElementInteractivity(ctx context.Context, session, selector string) (tag string, interactive, textLike bool, err error) {
	sel, _ := json.Marshal(selector)
	body, err := i.evaluateString(ctx, session, fmt.Sprintf(elementInteractivityJS, sel))
	if err != nil {
		return "", false, false, err
	}
	var res struct {
		Tag         string `json:"tag"`
		Interactive bool   `json:"interactive"`
		TextLike    bool   `json:"textLike"`
		Why         string `json:"why"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Why == "missing" {
		return "", false, false, fmt.Errorf("element not found: %s", selector)
	}
	return res.Tag, res.Interactive, res.TextLike, nil
}

// elementByText resolves a text target via elementByTextJS (the
// click-only text rescue, see its comment). The error wordings
// mirror the #N name fallback — same contract, same ambiguity refusal.
func (i *Instance) elementByText(ctx context.Context, session, tag, text string) (x, y float64, selector string, err error) {
	textJSON, _ := json.Marshal(text)
	tagJSON, _ := json.Marshal(tag)
	body, err := i.evaluateString(ctx, session, fmt.Sprintf(elementByTextJS, textJSON, tagJSON))
	if err != nil {
		return 0, 0, "", err
	}
	var res struct {
		X        float64 `json:"x"`
		Y        float64 `json:"y"`
		Selector string  `json:"selector"`
		Why      string  `json:"why"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return 0, 0, "", fmt.Errorf("element geometry: %w", err)
	}
	switch res.Why {
	case "":
		return res.X, res.Y, res.Selector, nil
	case "multiple":
		return 0, 0, "", fmt.Errorf("multiple elements say %q — read again and use a fresh #N, or a more specific selector", text)
	default:
		return 0, 0, "", fmt.Errorf("no visible element says %q (the text may be partial or gone — read to see the page's current text, or use a #N from the last read)", text)
	}
}

// playwrightRedirect names the Playwright selector forms the model
// writes by habit and redirects them to this tool's own vocabulary
// (#N, a CSS selector, or the element's visible text). The tool does
// NOT accept them (decided 2026-11-24): a working Playwright form would
// teach the model to write more of them — the boundary is that this
// tool is a legibility + robustness layer, not a second locator
// language. One clear line instead of the raw querySelector
// SyntaxError the raw form produced (the observed car-research
// failure). (msg, true) when the key is a Playwright form.
func playwrightRedirect(key string) (string, bool) {
	const general = `that looks like Playwright selector syntax — this tool doesn't take it. Pass a plain CSS selector, or the element's visible text alone (e.g. "See Inventory")`
	switch {
	case strings.HasPrefix(key, "id="):
		if id := strings.TrimSpace(key[len("id="):]); isSimpleID(id) {
			return "id= is not a selector form here — use #" + id + " instead", true
		}
		return general, true
	case strings.HasPrefix(key, "css="):
		return "drop the \"css=\" prefix — selectors here are plain CSS (pass the selector as-is)", true
	case strings.Contains(key, ":has-text("),
		strings.HasPrefix(key, "text="),
		strings.HasPrefix(key, "xpath="),
		strings.HasPrefix(key, "//"),
		strings.HasPrefix(key, "aria/"),
		strings.HasPrefix(key, "role="):
		return general, true
	}
	return "", false
}

func isSimpleID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// selectorShapedKey reports whether the key is written as a SELECTOR:
// it starts with selector punctuation (a class, an id, an attribute, a
// pseudo-class, a universal, a combinator). Copied page text essentially
// never starts that way, so a key that does not is TEXT — even a
// multi-word key like "No Such Text Here", which happens to parse as
// valid CSS (a chain of unknown element types) and comes back
// "not found" rather than a syntax error. A selector-shaped key that
// matches nothing is a stale/changed page — its error stands (a text
// search for ".foo" would only add noise).
func selectorShapedKey(key string) bool {
	if key == "" {
		return false
	}
	switch key[0] {
	case '.', '#', '[', ':', '*', '>', '+', '~':
		return true
	}
	return false
}

// ElementTarget resolves a click/type/select/fill target the way the
// model gives it:
//
//   - a #N id from the CALLING conversation's last read (re-matched when
//     the page re-rendered — ResolveRef);
//   - a CSS selector (used exactly as given — a "#..." key that is not
//     "#N" is an id selector);
//   - last resort, a NAME (a label, a button's text, an input's
//     placeholder: not a selector at all) re-matched by name alone
//     across the page's visible INTERACTIVE elements — the fill
//     contract, and what the model already writes successfully for fill.
//
// When the key is not valid CSS (it is the element's TEXT, not a
// selector) and textRescue is set (click only), the text is matched
// against ALL visible elements, innermost wins (elementByText) — the
// "click the thing that says X" task, covering controls the element
// table does not expose because the page renders them as plain divs
// (the observed car-research case). type/select/fill stay strict — a
// name among interactive elements only: typing into a heading would be
// a silent no-op the model can't recover from.
//
// Playwright selector syntax (button:has-text("X"), text=X, css=, id=,
// xpath=, aria/, role=) is NOT accepted — a working Playwright form
// would teach the model to write more of them (recreating Playwright's
// surface); playwrightRedirect answers with one line pointing at this
// tool's own vocabulary instead of the raw querySelector SyntaxError
// the raw form produced.
//
// Ambiguity at any stage is an error, never a guess.
func (i *Instance) ElementTarget(ctx context.Context, session, convID, key string, textRescue bool) (x, y float64, selector string, err error) {
	if key != "" {
		var n int
		if _, e := fmt.Sscanf(key, "#%d", &n); e == nil && n > 0 {
			ref, ok := i.ElementByN(convID, n)
			if !ok {
				return 0, 0, "", fmt.Errorf("element #%d is not in the last read's table — the page may have changed; read again and use a fresh #N", n)
			}
			return i.ResolveRef(ctx, session, ref)
		}
		if msg, pw := playwrightRedirect(key); pw {
			return 0, 0, "", errors.New(msg)
		}
	}
	cx, cy, e := i.ElementCenter(ctx, session, key)
	if e == nil {
		return cx, cy, key, nil
	}
	// The key is not a working selector — try it as a NAME (the fill
	// contract: a label, a button's text, a placeholder). This runs on
	// EVERY failure: a bare word ("Save") is a valid CSS type selector
	// that matches nothing — the name is what the model meant.
	if key != "" {
		name, _ := json.Marshal(key)
		tag, _ := json.Marshal("*")
		body, e2 := i.evaluateString(ctx, session, fmt.Sprintf(elementCenterRefJS, `"missing-selector"`, name, tag))
		if e2 == nil {
			var res struct {
				X        float64 `json:"x"`
				Y        float64 `json:"y"`
				Selector string  `json:"selector"`
				Why      string  `json:"why"`
			}
			if json.Unmarshal([]byte(body), &res) == nil {
				switch res.Why {
				case "":
					return res.X, res.Y, res.Selector, nil
				case "multiple":
					return 0, 0, "", fmt.Errorf("multiple elements match %q — read again and use a fresh #N, or a more specific selector", key)
				}
			}
		}
		// The text rescue (click only): no interactive element bears the
		// name — it may be the visible text of a NON-interactive control.
		// Gated on the key NOT being selector-shaped (see
		// selectorShapedKey): a real selector that matches nothing is a
		// stale/changed page, its error stands. The rescue's error
		// wordings (not found vs multiple) are the useful ones for a
		// text-shaped key.
		if textRescue && !selectorShapedKey(key) {
			rx, ry, rsel, terr := i.elementByText(ctx, session, "", key)
			if terr == nil {
				return rx, ry, rsel, nil
			}
			return 0, 0, "", terr
		}
	}
	return 0, 0, "", e
}

// ClickAt sends a real mouse click at viewport coordinates (move, press,
// release — the Input domain, so the page sees genuine input events, not a
// synthetic el.click()).
func (i *Instance) ClickAt(ctx context.Context, session string, x, y float64) error {
	for _, ev := range []struct {
		method string
		params map[string]any
	}{
		{"Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y}},
		{"Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1}},
		{"Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1}},
	} {
		if _, err := i.cdp.Call(ctx, session, ev.method, ev.params); err != nil {
			return err
		}
	}
	return nil
}

// TypeAt focuses (a click at the coordinates) then inserts the text via
// Input.insertText (the full string at once — no per-key latency; newlines
// inside the text are inserted as text, a trailing "\n" is sent as a real
// Enter key so forms submit).
func (i *Instance) TypeAt(ctx context.Context, session string, x, y float64, text string) error {
	if err := i.ClickAt(ctx, session, x, y); err != nil {
		return err
	}
	if strings.HasSuffix(text, "\n") {
		body := strings.TrimSuffix(text, "\n")
		if body != "" {
			if _, err := i.cdp.Call(ctx, session, "Input.insertText", map[string]any{"text": body}); err != nil {
				return err
			}
		}
		for _, ph := range []map[string]any{
			{"type": "keyDown", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13},
			{"type": "keyUp", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13},
		} {
			if _, err := i.cdp.Call(ctx, session, "Input.dispatchKeyEvent", ph); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := i.cdp.Call(ctx, session, "Input.insertText", map[string]any{"text": text})
	return err
}

// SelectValue sets a <select>'s value (by option value or text) and fires
// the input + change events a page's listeners expect.
func (i *Instance) SelectValue(ctx context.Context, session, selector, value string) (string, error) {
	sel, _ := json.Marshal(selector)
	val, _ := json.Marshal(value)
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%s);
		if (!el) return "not found";
		if (el.tagName !== "SELECT") return "not a select element";
		let chosen = null;
		for (const o of el.options) {
			if (o.value === %s || o.text.trim() === %s) { chosen = o; break; }
		}
		if (!chosen) return "option not found: " + JSON.stringify(Array.from(el.options).slice(0, 20).map(o => o.value));
		el.value = chosen.value;
		el.dispatchEvent(new Event("input", { bubbles: true }));
		el.dispatchEvent(new Event("change", { bubbles: true }));
		return "selected: " + (chosen.text || chosen.value);
	})()`, sel, val, val)
	return i.evaluateString(ctx, session, expr)
}
