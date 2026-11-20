package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/browser"
)

// webBrowserDescriptionBase is the web_browser tool's base description (the
// registry entry, and the wire copy's base). The per-turn WIRE copy carries
// a SEARCH note appended on top (webBrowserWireDescription — the model is
// pointed at web_search for a plain search when it is enabled, and at
// search-engine tabs when it is not). The registry entry itself never
// carries the note (it is per-instance state, not part of the tool).
//
// BREVITY CONTRACT (user directive 2026-11-19): the whole wire tool surface
// must stay small (the descriptions ride every system prompt). The
// description must still stand ALONE (the WS4 test — the model uses the
// tool from the description alone): trim padding, never the behavior. Arg
// details live in the parameter descriptions, not the body.
const webBrowserDescriptionBase = `Browse the web in a real desktop browser the bevin server runs in a dedicated profile (logins made in it persist). #N = an interactive element id from the last read.
Commands:
- openTab (url?) — open a tab (becomes the current one)
- navigate (url) — load url in the current tab (waits for load)
- read (selector?) — the page as clean markdown + numbered interactive elements (#N: role "name"). How you see content; selector drills into a truncated page.
- readItems (selector) — the items matching selector, in VISUAL order (not DOM order), deduped, text + first link, after the page settles.
- scroll (direction?, viewports?) — move whole viewports (direction down|up|top|bottom, default: down 1) and wait for settle; reports position + bottom reached.
- click (element|selector) — click #N or a CSS selector
- type (element|selector, text) — click + type (a trailing newline = Enter)
- select (element|selector, value) — pick a <select> option (value or visible text)
- waitFor (selector, timeout_ms?) — wait until a selector is present+visible (default 10 s)
- listTabs / closeTab (tab?)
Workflow: openTab/navigate → read → act with #N → read again after each action (the page changes; stale #N ids fail). Lazy-loaded/virtualized list pages: read to find the item's selector, then loop scroll + readItems until nothing new appears or the bottom is reached.`

// webBrowserWireDescription is the web_browser description for ONE turn's
// wire request: the base + the search note matching this instance's
// web_search state (user directive: when web_search is off, the agent
// searches the web by opening a search-engine tab here — the model needs
// the pointer to act on it).
func webBrowserWireDescription(wsEnabled bool) string {
	if wsEnabled {
		return webBrowserDescriptionBase + "\nSearching: prefer web_search for plain searches; use this browser to read or interact with a specific page."
	}
	return webBrowserDescriptionBase + "\nSearching: web_search is not configured — openTab a search-engine results page (e.g. https://www.bing.com/search?q=your+query), read the results, then openTab the pages that look useful."
}

// registerWebBrowser registers the web_browser tool (the plan's WS5): ONE
// tool, a single `command` arg over a fixed surface (openTab, navigate,
// read, readItems, scroll, click, type, select, waitFor, listTabs,
// closeTab). NO model-controlled JS execution (the evaluate command and
// waitFor's expression mode were removed 2026-11-19 — a prompt-injected
// page could route attacker code into the user's logged-in profile;
// design log "evaluate REMOVED"). It is registered unconditionally — a
// host without a desktop browser reports the tool as UNAVAILABLE at use
// time (the neutral wording; the user is the one who installs a browser),
// which the model relays to the user.
func registerWebBrowser(r *ToolRegistry, bm *browser.Manager) error {
	return r.Register(Tool{
		Name:        "web_browser",
		Description: webBrowserDescriptionBase,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type": "string",
					"enum": []string{"openTab", "navigate", "read", "readItems", "scroll", "click", "type", "select", "waitFor", "listTabs", "closeTab"},
				},
				"url":        map[string]any{"type": "string", "description": "openTab / navigate (http/https only)"},
				"element":    map[string]any{"type": "string", "description": "click/type/select: #N id from the last read"},
				"selector":   map[string]any{"type": "string", "description": "a CSS selector — fallback for click/type/select, drill-in scope for read, item selector for readItems, wait target for waitFor"},
				"text":       map[string]any{"type": "string", "description": "type: text to type (trailing newline = Enter)"},
				"value":      map[string]any{"type": "string", "description": "select: option value or visible text"},
				"direction":  map[string]any{"type": "string", "description": "scroll: down|up|top|bottom (default down)"},
				"viewports":  map[string]any{"type": "number", "description": "scroll: viewport heights to move (default 1)"},
				"tab":        map[string]any{"type": "string", "description": "tab id from listTabs (default: this conversation's current tab — the last one it opened or used)"},
				"timeout_ms": map[string]any{"type": "number", "description": "waitFor: ceiling (default 10000, max 120000)"},
			},
			"required": []string{"command"},
		},
		Execute: executeWebBrowser(bm),
	})
}

// webBrowserArgs is one web_browser call's arguments (the model's JSON).
// There is deliberately NO expression/JS field: model-controlled JS
// execution in the page was removed (security — see registerWebBrowser).
type webBrowserArgs struct {
	Command   string `json:"command"`
	URL       string `json:"url"`
	Element   string `json:"element"`
	Selector  string `json:"selector"`
	Text      string `json:"text"`
	Value     string `json:"value"`
	Direction string `json:"direction"`
	Viewports int    `json:"viewports"`
	Tab       string `json:"tab"`
	TimeoutMS int    `json:"timeout_ms"`
}

// executeWebBrowser is the web_browser executor bound to the browser manager.
func executeWebBrowser(bm *browser.Manager) func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	return func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
		var a webBrowserArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return fmt.Sprintf("invalid arguments: %v — the only required field is \"command\" (one of openTab, navigate, read, readItems, scroll, click, type, select, waitFor, listTabs, closeTab).", err), nil
		}
		// The per-user browser (lazy launch on first use). A host without a
		// desktop browser lands here with the neutral unavailable report.
		inst, err := bm.ForUser(ctx, handle)
		if err != nil {
			return fmt.Sprintf("web_browser is not available on this host: %v.", err), nil
		}
		// The conversation this call belongs to (harness plumbing — runTurn
		// sets it; see conversationFromContext in filetools.go). The browser
		// INSTANCE is shared by all of the user's conversations, so the
		// browser's conversation-scoped state (working tab + #N table) is
		// keyed on it: one conversation never inherits another's tab or
		// element table.
		convID := conversationFromContext(ctx)
		switch a.Command {
		case "openTab":
			t, err := inst.OpenTab(ctx, convID, a.URL)
			if err != nil {
				if t.ID != "" {
					// The tab exists (at about:blank) — the navigation failed
					// (DNS, TLS, a bad url): the model can see it and retry.
					return fmt.Sprintf("opened tab %s, but navigation to %s failed: %v. The tab is open — fix the url and navigate again.", t.ID, a.URL, err), nil
				}
				return unavailable("openTab", err), nil
			}
			return fmt.Sprintf("opened tab %s at %s (loaded). read to see the page.", t.ID, t.URL), nil
		case "navigate":
			if a.URL == "" {
				return "navigate needs a url.", nil
			}
			tab, err := inst.ResolveTab(ctx, convID, a.Tab)
			if err != nil {
				return unavailable("navigate", err), nil
			}
			if err := inst.Navigate(ctx, tab, a.URL); err != nil {
				return unavailable("navigate", err), nil
			}
			return fmt.Sprintf("navigated to %s (loaded). read to see the page.", a.URL), nil
		case "read":
			tab, err := inst.ResolveTab(ctx, convID, a.Tab)
			if err != nil {
				return unavailable("read", err), nil
			}
			pr, err := inst.Read(ctx, convID, tab, a.Selector)
			if err != nil {
				return unavailable("read", err), nil
			}
			return browser.BoundRead(pr), nil
		case "readItems":
			tab, err := inst.ResolveTab(ctx, convID, a.Tab)
			if err != nil {
				return unavailable("readItems", err), nil
			}
			pi, err := inst.ReadItems(ctx, tab, a.Selector)
			if err != nil {
				return unavailable("readItems", err), nil
			}
			return browser.BoundItems(pi), nil
		case "scroll":
			dir := strings.ToLower(a.Direction)
			if dir == "" {
				dir = "down"
			}
			switch dir {
			case "down", "up", "top", "bottom":
			default:
				return fmt.Sprintf("scroll direction must be down, up, top, or bottom (got %q).", a.Direction), nil
			}
			n := a.Viewports
			if n < 1 {
				n = 1
			}
			if n > 10 {
				n = 10
			}
			tab, err := inst.ResolveTab(ctx, convID, a.Tab)
			if err != nil {
				return unavailable("scroll", err), nil
			}
			sr, err := inst.Scroll(ctx, tab, dir, n)
			if err != nil {
				return unavailable("scroll", err), nil
			}
			return browser.ScrollReport(sr), nil
		case "click", "type", "select":
			return doElementAction(ctx, inst, convID, a, handle)
		case "waitFor":
			if a.Selector == "" {
				return "waitFor needs a selector (a CSS selector that should become present and visible).", nil
			}
			timeout := 10 * time.Second
			if a.TimeoutMS > 0 {
				timeout = time.Duration(a.TimeoutMS) * time.Millisecond
			}
			if timeout > 120*time.Second {
				timeout = 120 * time.Second
			}
			tab, err := inst.ResolveTab(ctx, convID, a.Tab)
			if err != nil {
				return unavailable("waitFor", err), nil
			}
			ok, took, err := inst.Wait(ctx, tab, a.Selector, timeout)
			if err != nil {
				return unavailable("waitFor", err), nil
			}
			if ok {
				return fmt.Sprintf("selector present and visible after %s.", took.Round(time.Millisecond)), nil
			}
			return fmt.Sprintf("the selector was not present and visible within %s (the page state may be final — read to see what is actually there).", timeout), nil
		case "listTabs":
			tabs, err := inst.ListTabs(ctx)
			if err != nil {
				return unavailable("listTabs", err), nil
			}
			if len(tabs) == 0 {
				return "no tabs are open — openTab first.", nil
			}
			out := "tabs:\n"
			for _, t := range tabs {
				out += fmt.Sprintf("- %s: %s — %s\n", t.ID, t.Title, t.URL)
			}
			return strings.TrimRight(out, "\n"), nil
		case "closeTab":
			id, err := inst.CloseTab(ctx, convID, a.Tab)
			if err != nil {
				return unavailable("closeTab", err), nil
			}
			return fmt.Sprintf("closed tab %s.", id), nil
		default:
			return fmt.Sprintf("unknown command %q — the commands are: openTab, navigate, read, readItems, scroll, click, type, select, waitFor, listTabs, closeTab.", a.Command), nil
		}
	}
}

// resolveTarget turns a click/type/select's element-or-selector arg into a
// CSS selector: a "#N" id resolves against the CALLING conversation's
// LATEST read's element table (the server-side resolution — the model never
// guesses selectors); anything else is taken as a raw selector (the
// advanced fallback).
func resolveTarget(inst *browser.Instance, convID, element, selector string) (string, error) {
	if element != "" {
		n, err := parseElementID(element)
		if err != nil {
			return "", err
		}
		ref, ok := inst.ElementByN(convID, n)
		if !ok {
			return "", fmt.Errorf("element #%d is not in the last read's table — the page may have changed; read again and use a fresh #N", n)
		}
		return ref.Selector, nil
	}
	if selector != "" {
		return selector, nil
	}
	return "", fmt.Errorf("needs an element (#N from the last read) or a selector")
}

func parseElementID(s string) (int, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		return 0, fmt.Errorf("%q is not an element id (ids look like #12, from the last read)", s)
	}
	var n int
	if _, err := fmt.Sscanf(s[1:], "%d", &n); err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not an element id (ids look like #12, from the last read)", s)
	}
	return n, nil
}

// doElementAction runs click/type/select: resolve the target, get its
// viewport center, and drive it with real input events (click) or click +
// insert (type) or a value set + events (select).
func doElementAction(ctx context.Context, inst *browser.Instance, convID string, a webBrowserArgs, handle string) (string, error) {
	selector, err := resolveTarget(inst, convID, a.Element, a.Selector)
	if err != nil {
		return err.Error(), nil
	}
	tab, err := inst.ResolveTab(ctx, convID, a.Tab)
	if err != nil {
		return unavailable(a.Command, err), nil
	}
	session, err := inst.SessionFor(ctx, tab)
	if err != nil {
		return unavailable(a.Command, err), nil
	}
	switch a.Command {
	case "click":
		x, y, err := inst.ElementCenter(ctx, session, selector)
		if err != nil {
			return err.Error(), nil
		}
		if err := inst.ClickAt(ctx, session, x, y); err != nil {
			return unavailable("click", err), nil
		}
		return fmt.Sprintf("clicked %s — the page may have changed; read again before the next action.", selector), nil
	case "type":
		if a.Text == "" {
			return "type needs a text.", nil
		}
		x, y, err := inst.ElementCenter(ctx, session, selector)
		if err != nil {
			return err.Error(), nil
		}
		if err := inst.TypeAt(ctx, session, x, y, a.Text); err != nil {
			return unavailable("type", err), nil
		}
		return fmt.Sprintf("typed %d characters into %s.", len(a.Text), selector), nil
	case "select":
		if a.Value == "" {
			return "select needs a value.", nil
		}
		out, err := inst.SelectValue(ctx, session, selector, a.Value)
		if err != nil {
			return err.Error(), nil
		}
		return out, nil
	}
	return "internal: unknown element action " + a.Command, nil
}

// unavailable renders a command's infrastructure failure (the browser died
// mid-call, a CDP error) as a RESULT the model can relay — with the hint
// that a retry may relaunch the browser (the manager relaunches a dead
// instance on the next call).
func unavailable(command string, err error) string {
	return fmt.Sprintf("%s failed: %v. (If the browser window was closed, the next call relaunches it — try the command again.)", command, err)
}
