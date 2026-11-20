package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/utopiafallen/bevin/internal/websearch"
)

// webSearchDescription is the model-facing description on the wire.
const webSearchDescription = `Search the web with the server's configured search API (a natural-language query returns ranked results: title, URL, publication date, and relevance-sized excerpts). Use it for research and lookups; use web_browser to read or interact with a specific page.
Arguments:
- query (required) — the search query in natural language (the subject +, when useful, the kind of source and time period you want)
- num_results (optional) — how many results to fetch (1-20, default 10)`

// webSearchArgs is one web_search call's arguments (the model's JSON).
type webSearchArgs struct {
	Query      string `json:"query"`
	NumResults int    `json:"num_results"`
}

// newSearchProvider resolves the admin's provider choice to a Provider (the
// internal/websearch factory). A variable, not a direct call, so a test can
// swap in a fake provider without touching a vendor's API (the abstraction
// the tool is built on is testable end to end).
var newSearchProvider = func(name string, cfg websearch.Config) (websearch.Provider, error) {
	return websearch.New(name, cfg)
}

// registerWebSearch registers the web_search tool (the admin-configured
// search API — the provider seam is internal/websearch; v1 provider: Exa).
// It is registered unconditionally, like web_browser: a turn OFFERS it to
// the model only while the live config has it enabled + configured
// (wireTools filters the wire list), and a call that still lands (the model
// imitating it from its own history after an admin disabled it) gets the
// structured not-enabled result naming the web_browser fallback instead of
// a silent success.
func registerWebSearch(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "web_search",
		Description: webSearchDescription,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "the search query in natural language (the subject +, when useful, the kind of source and time period you want)",
				},
				"num_results": map[string]any{
					"type":        "number",
					"description": "how many results to fetch (1-20, default 10)",
				},
			},
			"required": []string{"query"},
		},
		Execute: executeWebSearch(a),
	})
}

// executeWebSearch is the web_search executor. It is PROVIDER-AGNOSTIC (the
// point of the internal/websearch seam): it reads the admin's provider
// choice + settings from the live config, resolves the Provider, and
// formats the provider-neutral results — no vendor's API shape appears in
// this file. Every failure mode is a RESULT back to the model (the
// self-healing signal), never a turn error — same contract as
// executeWebBrowser.
func executeWebSearch(a *Agent) func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	return func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
		var p webSearchArgs
		if err := json.Unmarshal(args, &p); err != nil {
			return fmt.Sprintf("invalid arguments: %v — the only required field is \"query\" (a natural-language search query).", err), nil
		}
		if strings.TrimSpace(p.Query) == "" {
			return "web_search needs a query (a natural-language search query).", nil
		}
		// Live config (the npCfg pattern): an admin save applies from the
		// next call, no restart. Not enabled/configured → the structured
		// fallback result (the model searches via web_browser instead —
		// opening a search-engine results page, which is exactly what it
		// already knows how to do).
		ws := a.webSearchState()
		if !ws.Configured() {
			return "web_search is not enabled on this server (the admin has not configured a search provider). To search the web, use web_browser: openTab a search-engine results page (e.g. https://www.bing.com/search?q=your+query) and read the results.", nil
		}
		prov, err := newSearchProvider(ws.Provider, websearch.Config{APIKey: ws.APIKey})
		if err != nil {
			return fmt.Sprintf("web_search: %v — the admin's Tools settings name a provider this build doesn't know.", err), nil
		}
		n := p.NumResults
		if n < 1 {
			n = 10
		}
		if n > 20 {
			n = 20
		}
		results, err := prov.Search(ctx, p.Query, n)
		if err != nil {
			return fmt.Sprintf("web search failed: %v. Retry once; if it keeps failing, search via web_browser (a search-engine results page) or answer from what you already have.", err), nil
		}
		return formatSearchResults(p.Query, results), nil
	}
}

// formatSearchResults renders provider-neutral search results as the
// bounded text block the model reasons over: numbered — title / URL /
// publication date / up to 2 highlights per result (600 chars each) — the
// whole block capped with a truncation notice (the same bounded-output
// discipline as web_browser's read).
func formatSearchResults(query string, results []websearch.Result) string {
	if len(results) == 0 {
		return fmt.Sprintf("web search for %q: no results (the query may be too narrow — try a broader phrasing).", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "web search results for %q (%d):\n", query, len(results))
	for i, r := range results {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, title)
		if r.URL != "" {
			b.WriteString("   " + r.URL + "\n")
		}
		if r.PublishedDate != "" {
			b.WriteString("   published " + r.PublishedDate[:min(10, len(r.PublishedDate))] + "\n")
		}
		for j, h := range r.Highlights {
			if j >= 2 {
				break
			}
			if h = strings.TrimSpace(h); h != "" {
				b.WriteString("   " + snippet(h, 600) + "\n")
			}
		}
	}
	out := b.String()
	const cap = 16000
	if len(out) > cap {
		out = out[:cap] + "\n[truncated — the remaining results were cut; search again with a narrower query if you need them]"
	}
	return out
}

// snippet bounds one line of a tool result (internal newlines become
// spaces; over the cap gets an ellipsis).
func snippet(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
