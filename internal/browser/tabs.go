package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
		// they would only clutter the tab list and the default-tab logic.
		if strings.HasPrefix(t.URL, "chrome://") || strings.HasPrefix(t.URL, "edge://") || strings.HasPrefix(t.URL, "devtools://") || strings.HasPrefix(t.URL, "chrome-extension://") {
			continue
		}
		tabs = append(tabs, Tab{ID: t.ID, Title: t.Title, URL: t.URL})
		i.mu.Lock()
		if i.defaultTab == "" && len(tabs) == 1 {
			i.defaultTab = t.ID
		}
		i.mu.Unlock()
	}
	return tabs, nil
}

// OpenTab creates a new page target (about:blank when url is empty) and
// makes it the instance's default tab. With a url it then navigates EXPLICITLY
// (Page.navigate + the load wait): createTarget(url) opens the target at
// about:blank first and the navigation lands asynchronously — trusting the
// initial readyState would let the first read race the navigation (the
// Chromium race seen in the live e2e). A failed navigation (DNS, etc.)
// returns the tab (it exists, at about:blank) WITH the error — the model
// can retry.
func (i *Instance) OpenTab(ctx context.Context, url string) (Tab, error) {
	if url == "" {
		url = "about:blank"
	}
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
	i.defaultTab = out.TargetID
	i.mu.Unlock()
	tab := Tab{ID: out.TargetID, URL: url}
	if url != "about:blank" {
		if nerr := i.Navigate(ctx, out.TargetID, url); nerr != nil {
			tab.URL = "about:blank"
			return tab, nerr
		}
	}
	return tab, nil
}

// CloseTab closes a page target (the default when id is empty).
func (i *Instance) CloseTab(ctx context.Context, id string) (string, error) {
	if id == "" {
		d, err := i.DefaultTab(ctx)
		if err != nil {
			return "", err
		}
		id = d
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
	if i.defaultTab == id {
		i.defaultTab = ""
	}
	i.mu.Unlock()
	return id, nil
}

// DefaultTab returns the tab commands act on when none is named: the most
// recently opened one, else the first page target, else a fresh about:blank.
func (i *Instance) DefaultTab(ctx context.Context) (string, error) {
	i.mu.Lock()
	id := i.defaultTab
	i.mu.Unlock()
	if id != "" {
		return id, nil
	}
	tabs, err := i.ListTabs(ctx)
	if err != nil {
		return "", err
	}
	if len(tabs) > 0 {
		i.mu.Lock()
		i.defaultTab = tabs[0].ID
		i.mu.Unlock()
		return tabs[0].ID, nil
	}
	t, err := i.OpenTab(ctx, "")
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// ResolveTab maps a command's tab argument ("" = default) to a target id.
func (i *Instance) ResolveTab(ctx context.Context, id string) (string, error) {
	if id == "" {
		return i.DefaultTab(ctx)
	}
	return id, nil
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
	return i.waitForLoad(ctx, session)
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
