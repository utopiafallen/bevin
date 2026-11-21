package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/websearch"
)

// fakeSearchProvider is a stand-in search backend at the provider seam:
// the web_search executor must be PROVIDER-AGNOSTIC — it calls whatever the
// admin's settings resolve and formats the provider-neutral results, so the
// test swaps the factory (newSearchProvider) for this and never touches a
// vendor's API.
type fakeSearchProvider struct {
	results   []websearch.Result
	err       error
	lastQuery string
	lastN     int
}

func (f *fakeSearchProvider) Name() string { return "fake" }

func (f *fakeSearchProvider) Search(_ context.Context, query string, numResults int) ([]websearch.Result, error) {
	f.lastQuery = query
	f.lastN = numResults
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

// wireToolNames returns the wire's tool names + a map to the descriptions.
func wireToolNames(wire []provider.Tool) (map[string]bool, map[string]string) {
	names := map[string]bool{}
	descs := map[string]string{}
	for _, tl := range wire {
		names[tl.Name] = true
		descs[tl.Name] = tl.Description
	}
	return names, descs
}

// TestWebSearchNotEnabled — with web search disabled/unconfigured (the
// default), web_search is NOT offered on the wire (wireTools filters it),
// the web_browser description carries the search-engine fallback note, and
// a call that still lands (the model imitating the tool from its own
// history after an admin disabled it) gets the structured not-enabled
// result naming the web_browser fallback — a result, never a turn error.
func TestWebSearchNotEnabled(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	names, descs := wireToolNames(a.wireTools(nil, nil))
	if names["web_search"] {
		t.Fatal("web_search must not be offered while the admin has it disabled")
	}
	if !names["web_browser"] {
		t.Fatal("web_browser must still be offered")
	}
	if !strings.Contains(descs["web_browser"], "search-engine results page") {
		t.Fatalf("the web_browser wire description should carry the browser-search note (web_search is off): %.200s", descs["web_browser"])
	}

	// A slipped-through call: the structured fallback result (the loop stays
	// healthy — a result row, not an error).
	tl, ok := a.tools.Get("web_search")
	if !ok {
		t.Fatal("web_search must stay REGISTERED (only the wire hides it)")
	}
	out, err := tl.Execute(context.Background(), "u1", json.RawMessage(`{"query":"test"}`))
	if err != nil {
		t.Fatalf("not-enabled must be a result, not an error: %v", err)
	}
	if !strings.Contains(out, "not enabled") || !strings.Contains(out, "web_browser") {
		t.Fatalf("not-enabled result = %q, want it naming the web_browser fallback", out)
	}
}

// TestWebSearchEnabled — with web search enabled: web_search is offered on
// the wire, the web_browser note flips to "prefer web_search", the executor
// resolves the ADMIN'S provider from the live config (the npCfg pattern —
// an admin save applies from the next call) through the provider seam, and
// the results are formatted provider-neutrally (title / URL / date /
// highlights).
func TestWebSearchEnabled(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	// Enable in the LIVE config (an admin save would do this via
	// applyConfig; the test flips it directly).
	a.cfgMu.Lock()
	a.cfg.Tools.WebSearch = config.WebSearch{Enabled: true, Provider: "exa", APIKey: "exa-test"}
	a.cfgMu.Unlock()

	// The provider seam: a fake backend standing in for Exa (the executor
	// must not know which vendor it is talking to).
	fp := &fakeSearchProvider{results: []websearch.Result{
		{Title: "First", URL: "https://example.com/1", PublishedDate: "2026-11-01T00:00:00.000Z", Highlights: []string{"h1", "h2", "h3"}},
		{Title: "", URL: "https://example.com/2"},
	}}
	orig := newSearchProvider
	newSearchProvider = func(name string, cfg websearch.Config) (websearch.Provider, error) {
		if name != "exa" {
			t.Fatalf("the executor resolved provider %q, want the admin's exa", name)
		}
		if cfg.APIKey != "exa-test" {
			t.Fatalf("the executor passed key %q, want the live config's", cfg.APIKey)
		}
		return fp, nil
	}
	defer func() { newSearchProvider = orig }()

	// The wire offers web_search + the web_browser note flips.
	names, descs := wireToolNames(a.wireTools(nil, nil))
	if !names["web_search"] {
		t.Fatal("web_search must be offered when the admin has it enabled")
	}
	if !strings.Contains(descs["web_browser"], "prefer web_search") {
		t.Fatalf("the web_browser wire description should say to prefer web_search (it is on): %.200s", descs["web_browser"])
	}
	if strings.Contains(descs["web_browser"], "search-engine results page") {
		t.Fatal("the browser-search fallback note must not be present while web_search is on")
	}

	// Executor: the query reaches the provider, the results are formatted
	// (numbered title / URL / date / up to 2 highlights — h3 is cut).
	tl, ok := a.tools.Get("web_search")
	if !ok {
		t.Fatal("web_search must resolve")
	}
	out, err := tl.Execute(context.Background(), "u1", json.RawMessage(`{"query":"the answer","num_results":7}`))
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	if fp.lastQuery != "the answer" {
		t.Fatalf("provider query = %q, want the model's query", fp.lastQuery)
	}
	for _, want := range []string{"1. First", "https://example.com/1", "2026-11-01", "h1", "h2", "2. (untitled)", "https://example.com/2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted results missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "h3") {
		t.Fatalf("more than 2 highlights per result:\n%s", out)
	}

	// A provider failure is a RESULT back to the model (retry / browser
	// fallback), never a turn error.
	fp.err = context.DeadlineExceeded
	out, err = tl.Execute(context.Background(), "u1", json.RawMessage(`{"query":"again"}`))
	if err != nil {
		t.Fatalf("a provider failure must be a result, not an error: %v", err)
	}
	if !strings.Contains(out, "web search failed") || !strings.Contains(out, "web_browser") {
		t.Fatalf("failure result = %q, want the retry + browser fallback", out)
	}
}

// TestWebSearchArgValidation — the argument contract: a blank query is a
// result (the model fixes its call), malformed JSON is a result naming the
// required field, and num_results clamps to [1,20] (the provider never
// gets a zero/negative or unbounded count).
func TestWebSearchArgValidation(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	a.cfgMu.Lock()
	a.cfg.Tools.WebSearch = config.WebSearch{Enabled: true, Provider: "exa", APIKey: "k"}
	a.cfgMu.Unlock()

	fp := &fakeSearchProvider{}
	orig := newSearchProvider
	newSearchProvider = func(string, websearch.Config) (websearch.Provider, error) { return fp, nil }
	defer func() { newSearchProvider = orig }()

	tl, ok := a.tools.Get("web_search")
	if !ok {
		t.Fatal("web_search must resolve")
	}
	// Blank query -> a result naming the fix (the provider is never called).
	out, err := tl.Execute(context.Background(), "u1", json.RawMessage(`{"query":"   "}`))
	if err != nil {
		t.Fatalf("blank query must be a result, not an error: %v", err)
	}
	if !strings.Contains(out, "needs a query") {
		t.Fatalf("blank-query result = %q", out)
	}
	// Malformed args -> a result naming the required field.
	out, err = tl.Execute(context.Background(), "u1", json.RawMessage(`{`))
	if err != nil {
		t.Fatalf("malformed args must be a result, not an error: %v", err)
	}
	if !strings.Contains(out, "invalid arguments") || !strings.Contains(out, "query") {
		t.Fatalf("malformed-args result = %q", out)
	}
	// num_results clamps: absent/0/negative -> the default 10, >20 -> 20.
	for in, want := range map[string]int{`{"query":"q"}`: 10, `{"query":"q","num_results":0}`: 10, `{"query":"q","num_results":-3}`: 10, `{"query":"q","num_results":99}`: 20} {
		fp.err = nil
		fp.lastN = -1
		if _, err := tl.Execute(context.Background(), "u1", json.RawMessage(in)); err != nil {
			t.Fatalf("execute(%s): %v", in, err)
		}
		if fp.lastN == -1 {
			t.Fatalf("args %s: the provider was never called", in)
		}
		if fp.lastN != want {
			t.Fatalf("args %s: the provider got num_results %d, want the clamped %d", in, fp.lastN, want)
		}
	}
}
