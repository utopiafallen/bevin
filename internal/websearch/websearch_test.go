package websearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewFactory — the provider factory: known names resolve, unknown names
// are a loud error (the runtime's defensive twin of config.Validate), and
// the registry lists exactly the names New accepts (single source of truth
// — the admin UI's provider select renders from it, never a hardcoded list).
func TestNewFactory(t *testing.T) {
	p, err := New("exa", Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("exa: %v", err)
	}
	if p.Name() != "exa" {
		t.Fatalf("name = %q, want exa", p.Name())
	}
	if _, err := New("brave", Config{}); err == nil {
		t.Fatal("an unknown provider must be an error")
	}
	if _, err := New("", Config{}); err == nil {
		t.Fatal("a blank provider name must be an error")
	}
	for _, n := range Providers() {
		if _, err := New(n, Config{}); err != nil {
			t.Fatalf("Providers() listed %q but New rejects it: %v", n, err)
		}
	}
}

// TestExaSearch — the Exa client: Bearer auth, the v1 request shape (type
// auto + highlights — the docs' recommended agent output), and provider-
// neutral results (a result missing optional fields comes back with them
// empty, not zero-garbage).
func TestExaSearch(t *testing.T) {
	var gotAuth, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"Go 1.27 release","url":"https://go.dev/blog/127","publishedDate":"2026-11-01T00:00:00.000Z","highlights":["The Go 1.27 release ships…"]},
			{"title":"untitled result","url":"https://example.com/x"}
		]}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	orig := exaSearchURL
	exaSearchURL = ts.URL + "/search"
	defer func() { exaSearchURL = orig }()

	p, err := New("exa", Config{APIKey: "exa-123"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := p.Search(context.Background(), "go 1.27 release", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}
	if results[0].Title != "Go 1.27 release" || results[0].URL != "https://go.dev/blog/127" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	if results[0].PublishedDate == "" || len(results[0].Highlights) != 1 {
		t.Fatalf("results[0] missing date/highlights: %+v", results[0])
	}
	// Optional fields absent on the second result stay empty.
	if results[1].PublishedDate != "" || results[1].Highlights != nil {
		t.Fatalf("results[1] = %+v, want empty optional fields", results[1])
	}
	// Auth: the Bearer key.
	if gotAuth != "Bearer exa-123" {
		t.Fatalf("Authorization = %q, want Bearer exa-123", gotAuth)
	}
	// Request shape: the query + v1's auto type + highlights on.
	var req map[string]any
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("request body = %q: %v", gotBody, err)
	}
	if req["query"] != "go 1.27 release" || req["numResults"] != float64(5) || req["type"] != "auto" {
		t.Fatalf("request = %v", req)
	}
	contents, _ := req["contents"].(map[string]any)
	if contents["highlights"] != true {
		t.Fatalf("contents = %v, want highlights on", req["contents"])
	}
}

// TestExaSearchKeyRejected — a 401 from the Exa API surfaces as a clear
// "the key was rejected" error (the admin can act on it), and an empty
// result set is a RESULT, not an error (a legitimate no-match).
func TestExaSearchKeyRejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer ts.Close()
	orig := exaSearchURL
	exaSearchURL = ts.URL + "/search"
	defer func() { exaSearchURL = orig }()

	p, err := New("exa", Config{APIKey: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("error = %v, want the key-rejection message", err)
	}
}

// TestExaSearchNoResults — zero results is a successful empty result set
// (the tool's "no results" line, not an API failure).
func TestExaSearchNoResults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer ts.Close()
	orig := exaSearchURL
	exaSearchURL = ts.URL + "/search"
	defer func() { exaSearchURL = orig }()

	p, _ := New("exa", Config{APIKey: "k"})
	results, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("an empty result set must not be an error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want empty", results)
	}
}
