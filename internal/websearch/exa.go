package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// exaSearchURL is the Exa Search API endpoint
// (https://exa.ai/docs/search/quickstart). A variable (not a const) so the
// tests can point the client at a fake endpoint without touching the
// production URL.
var exaSearchURL = "https://api.exa.ai/search"

// exaClient is the shared HTTP client for Exa calls (one request per search
// — no pooling to manage; the 30 s ceiling keeps a hung API from holding a
// user's turn open indefinitely).
var exaClient = &http.Client{Timeout: 30 * time.Second}

// exaProvider is the Exa (https://exa.ai) search backend: one POST /search
// per query with a Bearer API key. v1 always requests HIGHLIGHTS
// (contents.highlights — relevance-sized excerpts, the docs' recommended
// output shape for agents) with type "auto" (the best default balance of
// quality and speed); whole-page text is a v2 option, not a v1 one.
type exaProvider struct {
	cfg Config
}

func newExa(cfg Config) Provider { return exaProvider{cfg: cfg} }

// Name is the provider's config name (what the admin's [tools.web_search]
// provider field stores).
func (p exaProvider) Name() string { return "exa" }

// exaResponse is the Exa Search API's response (only the fields bevin uses
// — the API carries more; unknown fields are ignored on unmarshal).
type exaResponse struct {
	Results []exaResult `json:"results"`
}

// exaResult is one Exa result (only the fields bevin uses).
type exaResult struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	PublishedDate string   `json:"publishedDate"`
	Highlights    []string `json:"highlights"`
}

// Search calls the Exa Search API: POST exaSearchURL,
// Authorization: Bearer <api key>, body {query, type, numResults,
// contents:{highlights:true}} → ranked results mapped to the
// provider-neutral Result.
func (p exaProvider) Search(ctx context.Context, query string, numResults int) ([]Result, error) {
	body, err := json.Marshal(map[string]any{
		"query":      query,
		"type":       "auto",
		"numResults": numResults,
		"contents":   map[string]any{"highlights": true},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, exaSearchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	res, err := exaClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exa: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("exa: reading response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("exa: the API rejected the API key (HTTP %d) — the admin should check it on the admin Tools page", res.StatusCode)
		}
		return nil, fmt.Errorf("exa: the API returned HTTP %d: %s", res.StatusCode, exaErrorBody(b))
	}
	var out exaResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("exa: the API returned an unparseable response: %v", err)
	}
	results := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, Result{
			Title:         r.Title,
			URL:           r.URL,
			PublishedDate: r.PublishedDate,
			Highlights:    r.Highlights,
		})
	}
	return results, nil
}

// exaErrorBody bounds an error body for inclusion in a tool result (the
// model sees a snippet, never the whole page).
func exaErrorBody(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		s = "(no body)"
	}
	return s
}
