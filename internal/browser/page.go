package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The page model the web_browser tool returns (the plan's WS5 contract):
// a CLEAN rendering of the page — never raw HTML (raw HTML is 10-50x the
// tokens of its readable content).
//
//   - default read = DETERMINISTIC clean (no LLM): readability-style main
//     content → markdown, boilerplate stripped, PLUS a numbered
//     interactive-elements list (a11y role + name + a server-side
//     resolvable selector) so the agent can still act;
//   - UNBOUNDED (user decision 2026-11-21 — the old 32,000-char budget
//     made real pages unusable): read returns the WHOLE cleaned page, no
//     truncation, no element cap. The only guard is the cleaner's MAX_NODES
//     walk ceiling for pathological pages, and hitting it is REPORTED (a
//     partial clean says so instead of silently truncating);
//   - aux-model extraction (goal/oversize) is the follow-up tier: v1
//     covers subtree scoping with the selector + slice pagination instead.

// PageRead is one read's result.
type PageRead struct {
	// Error is set by the cleaner when the drill-in selector matched
	// nothing (the elements list is still the full page's).
	Error    string       `json:"error,omitempty"`
	Title    string       `json:"title"`
	URL      string       `json:"url"`
	Markdown string       `json:"markdown"`
	Elements []elementRef `json:"elements"`
	// Aborted is set when the cleaner's MAX_NODES walk ceiling was hit
	// (a pathological page size): the clean is PARTIAL — the result says
	// so, it never silently truncates.
	Aborted bool `json:"aborted"`
	// The settle state (the read waits for readiness before extracting —
	// the mid-render fix, design log LAZY-LIST TIER, redesigned
	// 2026-11-23: a page still progressing is never cut off mid-load;
	// the state says exactly what the wait found).
	Settle SettleState `json:"settle"`
	Took   time.Duration
}

// pageCleanerJS is the deterministic cleaner, run in the page via
// Runtime.evaluate. It reads the LIVE DOM (read-only — it never mutates the
// page): main content → markdown (boilerplate tags + invisible subtrees
// skipped, node-bounded so a pathological page can't wedge the read) and
// the numbered interactive elements with unique selectors. The optional
// selector argument (a JSON string, injected by the caller) confines the
// clean to a subtree (the drill-in path).
var pageCleanerJS = `(() => {
  const REMOVE = new Set(['SCRIPT','STYLE','NOSCRIPT','TEMPLATE','SVG','CANVAS','IFRAME','OBJECT','EMBED','VIDEO','AUDIO','NAV','FOOTER','HEADER','ASIDE','DIALOG','MAP','SEARCH']);
  const MAX_NODES = 60000;
  let nodeCount = 0, aborted = false;
  function visible(el) {
    if (!el) return false;
    const s = getComputedStyle(el);
    return s.display !== 'none' && s.visibility !== 'hidden';
  }
  // Consent/cookie dialogs (the industry-standard vendors: OneTrust,
  // Didomi, Cookiebot, Quantcast, Iubenda, StackPath) + any role=dialog/
  // alert box: their controls (Accept / Close / Manage Preferences) are
  // not page content — they padded the element table to 300+ entries on
  // real dealer pages and distracted the reader. Heuristic by design
  // (recorded, design log BROWSER-ROBUST): a site about cookies loses
  // its cookie buttons from the table, an acceptable price.
  const CONSENT = /onetrust|didomi|cookiebot|quantcast|iubenda|sp_message|consent/i;
  function inConsentBox(el) {
    let n = el, depth = 0;
    while (n && n.nodeType === 1 && depth < 30) {
      const role = n.getAttribute && n.getAttribute('role');
      if (role === 'dialog' || role === 'alert') return true;
      const id = n.id || '';
      const cls = typeof n.className === 'string' ? n.className : '';
      if (CONSENT.test(id) || CONSENT.test(cls)) return true;
      n = n.parentElement;
      depth++;
    }
    return false;
  }
  function inlineText(node) {
    let out = '';
    for (const n of node.childNodes) {
      if (nodeCount++ > MAX_NODES) { aborted = true; return out; }
      if (n.nodeType === 3) { out += n.nodeValue; continue; }
      if (n.nodeType !== 1) continue;
      if (REMOVE.has(n.tagName) || !visible(n)) continue;
      if (n.tagName === 'BR') { out += '\n'; continue; }
      if (n.tagName === 'A' && n.href) {
        const t = inlineText(n).replace(/\s+/g, ' ').trim();
        if (t) out += '[' + t + '](' + n.href + ')';
        continue;
      }
      out += inlineText(n);
    }
    return out;
  }
  function roleFor(tag, el) {
    switch (tag) {
      case 'a': return 'link';
      case 'button': case 'summary': return 'button';
      case 'input': {
        const t = (el.type || 'text').toLowerCase();
        if (t === 'checkbox') return 'checkbox';
        if (t === 'radio') return 'radio';
        if (t === 'submit' || t === 'button' || t === 'reset') return 'button';
        if (t === 'range') return 'slider';
        return 'textbox';
      }
      case 'select': return el.multiple ? 'listbox' : 'combobox';
      case 'textarea': return 'textbox';
      case 'label': return 'label';
      default: return 'generic';
    }
  }
  function pathOf(el) {
    const parts = [];
    let n = el;
    while (n && n.nodeType === 1 && n.tagName.toLowerCase() !== 'body' && parts.length < 8) {
      if (n.id) { parts.unshift('#' + CSS.escape(n.id)); break; }
      let part = n.tagName.toLowerCase();
      const parent = n.parentElement;
      if (parent) {
        const sibs = Array.from(parent.children).filter(c => c.tagName === n.tagName);
        if (sibs.length > 1) part += ':nth-of-type(' + (sibs.indexOf(n) + 1) + ')';
      }
      parts.unshift(part);
      n = parent;
    }
    return 'body > ' + parts.join(' > ');
  }
  const elements = [];
  const seen = new Set();
  const candidates = Array.from(document.querySelectorAll(
    'a[href],button,[role="button"],[role="link"],input,select,textarea,summary,[onclick],label[for]'));
  for (const el of candidates) {
    if (!visible(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    if (inConsentBox(el)) continue;
    const tag = el.tagName.toLowerCase();
    let name = (el.getAttribute('aria-label') || '').trim();
    if (!name) name = (el.getAttribute('title') || '').trim();
    if (!name && tag !== 'input') name = (el.textContent || '').replace(/\s+/g, ' ').trim();
    if (!name && tag === 'input') name = el.placeholder || el.value || el.name || el.type || 'input';
    if (!name && tag === 'label' && el.htmlFor) {
      const t = document.getElementById(el.htmlFor);
      if (t && (t.placeholder || t.value)) name = t.placeholder || t.value;
    }
    name = String(name).slice(0, 80);
    let selector;
    if (el.id) {
      try { if (document.querySelectorAll('#' + CSS.escape(el.id)).length === 1) selector = '#' + CSS.escape(el.id); } catch (e) {}
    }
    if (!selector) selector = pathOf(el);
    if (seen.has(selector)) continue;
    seen.add(selector);
    elements.push({ n: elements.length + 1, role: (el.getAttribute('role') || roleFor(tag, el)), name, selector, tag });
  }
  const md = [];
  function directTextOf(el) {
    // The element's OWN text (text nodes that are its direct children) —
    // the text of a generic container with raw text (a div/span is content
    // too; walking only element children would drop it).
    let out = '';
    for (const n of el.childNodes) {
      if (n.nodeType === 3) out += n.nodeValue;
    }
    return out.replace(/\s+/g, ' ').trim();
  }
  function walk(el, depth) {
    if (aborted || depth > 50) return;
    for (const child of el.children) {
      if (nodeCount++ > MAX_NODES) { aborted = true; return; }
      const tag = child.tagName;
      if (REMOVE.has(tag) || !visible(child)) continue;
      const h = /^H([1-6])$/.exec(tag);
      if (h) {
        const t = inlineText(child).replace(/\s+/g, ' ').trim();
        if (t) md.push('\n\n' + '#'.repeat(+h[1]) + ' ' + t);
      } else if (tag === 'P') {
        const t = inlineText(child).replace(/\s+/g, ' ').trim();
        if (t) md.push('\n\n' + t);
      } else if (tag === 'UL' || tag === 'OL') {
        let i = 1;
        for (const li of child.children) {
          if (li.tagName !== 'LI') continue;
          const t = inlineText(li).replace(/\s+/g, ' ').trim();
          if (t) md.push((tag === 'OL' ? '\n' + (i++) + '. ' : '\n- ') + t);
        }
      } else if (tag === 'PRE') {
        const fence = String.fromCharCode(96).repeat(3);
        md.push('\n\n' + fence + '\n' + (child.textContent || '').trim() + '\n' + fence);
      } else if (tag === 'TABLE') {
        const rows = [];
        for (const tr of child.querySelectorAll('tr')) {
          const cells = Array.from(tr.children).map(td => inlineText(td).replace(/\s+/g, ' ').trim());
          if (cells.length) rows.push(cells);
        }
        if (rows.length) md.push('\n\n' + rows.map(r => '| ' + r.join(' | ') + ' |').join('\n'));
      } else if (tag === 'HR') {
        md.push('\n\n---');
      } else {
        const t = directTextOf(child);
        if (t) md.push('\n\n' + t);
        walk(child, depth + 1);
      }
    }
  }
  // The injected value is a JSON string literal ("" when absent) — a valid
  // JS string as-is (NOT wrapped in a template literal — that would leave
  // the JSON's quote characters in the value).
  const sel = %s;
  // The guard must test sel BEFORE calling querySelector (an empty string
  // is a SELECTOR SYNTAX ERROR, not a no-match).
  const root = sel ? document.querySelector(sel) : document.body;
  if (!root) return JSON.stringify({ error: 'selector not found: ' + sel, title: document.title, url: location.href, markdown: '', elements: elements });
  // The ROOT'S OWN direct text: a drill-in to a heading/paragraph (the
  // element's text is a direct text node — walk() below only sees ELEMENT
  // children, so without this a read of an h1/p selector comes back
  // EMPTY). A no-op for the default root (body's text lives in its
  // element children).
  {
    const t = directTextOf(root);
    if (t) {
      const h = /^H([1-6])$/.exec(root.tagName);
      if (h) md.push('\n\n' + '#'.repeat(+h[1]) + ' ' + t);
      else md.push('\n\n' + t);
    }
  }
  walk(root, 0);
  return JSON.stringify({
    title: document.title,
    url: location.href,
    markdown: md.join('').replace(/\n{3,}/g, '\n\n').trim(),
    elements: elements,
    aborted: aborted
  });
})()`

// Read returns the clean page model for the tab: it settles the page
// FIRST (the mid-render fix — the content extracted is what the page
// actually delivered, design log LAZY-LIST TIER), then the deterministic
// markdown (confined to selector when given — the drill-in path) + the
// numbered interactive elements. The element table is remembered on the
// instance keyed by the CALLING conversation (the #N ids of the
// conversation's LATEST read are what that conversation's click/type/select
// resolve — one conversation's table never leaks into another's).
func (i *Instance) Read(ctx context.Context, convID, tabID, selector string) (*PageRead, error) {
	settled, took, err := i.Settle(ctx, tabID)
	if err != nil {
		return nil, err
	}
	session, err := i.SessionFor(ctx, tabID)
	if err != nil {
		return nil, err
	}
	sel, _ := json.Marshal(selector) // JSON string literal ("" when absent)
	extract := func() (PageRead, error) {
		body, err := i.evaluateString(ctx, session, fmt.Sprintf(pageCleanerJS, sel))
		if err != nil {
			return PageRead{}, err
		}
		var pr PageRead
		if err := json.Unmarshal([]byte(body), &pr); err != nil {
			return PageRead{}, fmt.Errorf("parsing the page clean: %w", err)
		}
		return pr, nil
	}
	pr, err := extract()
	if err != nil {
		return nil, err
	}
	if pr.Error != "" {
		// A FRESH SPA may still be rendering: the settle saw the chrome
		// go quiet, the content arrives async (the observed car-research
		// failure: read with "h1, h2, h3" three seconds after a navigate
		// — "selector not found" — the page rendered its content a
		// moment later, and the model paid a second call to find out).
		// Wait for the selector (bounded, the Wait contract) and re-extract
		// ONCE before reporting not-found; the error that survives says
		// what it means and what the model should do instead.
		if strings.HasPrefix(pr.Error, "selector not found:") && selector != "" {
			ok, _, werr := i.Wait(ctx, tabID, selector, 5*time.Second)
			if werr != nil {
				return nil, werr
			}
			if ok {
				if pr2, e2 := extract(); e2 == nil && pr2.Error == "" {
					pr = pr2
				}
			}
		}
		if pr.Error != "" {
			if strings.HasPrefix(pr.Error, "selector not found:") {
				return nil, fmt.Errorf("%s (waited 5s for the page to render — it still doesn't use this; read without a selector to see what IS there)", pr.Error)
			}
			return nil, fmt.Errorf("%s", pr.Error)
		}
	}
	pr.Settle = settled
	pr.Took = took
	if pr.Elements == nil {
		pr.Elements = []elementRef{}
	}
	i.mu.Lock()
	table := make(map[int]elementRef, len(pr.Elements))
	for _, e := range pr.Elements {
		table[e.N] = e
	}
	i.convRead[convID] = table
	i.mu.Unlock()
	return &pr, nil
}

// FormatRead renders a read result for the model: the title/url header,
// the FULL cleaned markdown (unbounded — see the file header; the only
// notice is the partial-clean flag when the cleaner's node ceiling was
// hit), and the numbered interactive-element table.
func FormatRead(pr *PageRead) string {
	md := pr.Markdown
	els := ""
	for _, e := range pr.Elements {
		els += fmt.Sprintf("#%d %s %q <%s>\n", e.N, e.Role, e.Name, e.Tag)
	}
	out := fmt.Sprintf("title: %s\nurl: %s\n", pr.Title, pr.URL)
	if pr.Settle != SettleSettled {
		out += "(" + settleNote(pr.Settle, pr.Took) + ")\n"
	}
	if pr.Aborted {
		out += "(the page is enormous — the clean hit its node ceiling and the content may be PARTIAL; read with a selector to get a subtree)\n"
	}
	out += "content:\n"
	out += md
	if els != "" {
		out += "\n\ninteractive elements (click/type/select take the #N id, or a raw selector):\n" + els
	}
	return out
}

// ElementByN resolves a "#N" id from the CALLING conversation's latest read
// (the server-side resolution of the plan's element model).
func (i *Instance) ElementByN(convID string, n int) (elementRef, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.convRead[convID][n]
	return e, ok
}
