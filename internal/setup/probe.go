package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
)

// probeClient is a short-timeout client for endpoint probing, so a dead or
// unreachable endpoint can't hang the wizard for minutes.
var probeClient = &http.Client{Timeout: 15 * time.Second}

// ProbeResult is the outcome of probing an endpoint: the detected provider
// kind, the models it serves (with context lengths where the API exposes
// them), and the RESOLVED base URL — the exact variant that succeeded (the
// scheme + /v1 as tried). Callers should adopt it so the saved config is the
// URL that actually works.
type ProbeResult struct {
	Provider string               `json:"provider"`
	Models   []provider.ModelInfo `json:"models"`
	BaseURL  string               `json:"base_url"`
}

// ProbeEndpoint is the exported probe (shared by the setup wizard and the
// admin config pages): detect the provider kind + list the endpoint's models.
func ProbeEndpoint(ctx context.Context, baseURL, apiKey string) (*ProbeResult, error) {
	return probeEndpoint(ctx, baseURL, apiKey)
}

// ctxFieldKeys are the keys we recognize as a context-window length, wherever
// they appear in a model's JSON subtree. The search is forgiving about the
// data shape — n_ctx can sit at the top level, in a "meta" block, or elsewhere.
// "max_tokens" is deliberately excluded: it is an OUTPUT limit, not the
// context window.
var ctxFieldKeys = []string{
	"n_ctx", "max_input_tokens", "context_length",
	"max_context", "max_context_tokens", "max_context_length",
}

// probeEndpoint detects the provider kind for baseURL by testing the /models
// signature, then lists the models it serves (ids + context lengths where
// available). The base URL is normalized before probing: a missing scheme is
// tried as both https:// and http:// (whichever succeeds wins), and a missing
// /v1 suffix is tried with and without — so "api.openai.com",
// "https://api.openai.com", and "https://api.openai.com/v1" all work.
func probeEndpoint(ctx context.Context, baseURL, apiKey string) (*ProbeResult, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, fmt.Errorf("enter the API base URL first")
	}
	base := strings.TrimRight(raw, "/")
	// base variants: add a scheme if it is missing (https first, then http).
	var bases []string
	if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
		bases = []string{base}
	} else {
		bases = []string{"https://" + base, "http://" + base}
	}
	// candidate /models URLs: each base, with /v1 added only if the base does
	// not already end in /v1. Each candidate remembers the BASE it came from
	// (the URL with the /models suffix stripped) so the probe can report the
	// exact base that worked.
	seen := map[string]bool{}
	type cand struct {
		base, url string
	}
	var cands []cand
	for _, b := range bases {
		add := func(u, base string) {
			if !seen[u] {
				seen[u] = true
				cands = append(cands, cand{base: base, url: u})
			}
		}
		add(b+"/models", b)
		if !strings.HasSuffix(b, "/v1") {
			add(b+"/v1/models", b+"/v1")
		}
	}
	// Try the candidate /models URLs IN PARALLEL: a dead or slow variant (e.g.
	// a LAN IP whose bare /models endpoint hangs or is filtered) must not
	// delay the one that succeeds. Each candidate runs in its own goroutine;
	// we return the first success, or the last error if every candidate fails.
	type probeOut struct {
		res *ProbeResult
		err error
	}
	outCh := make(chan probeOut, len(cands))
	for _, c := range cands {
		go func(c cand) {
			b, err := fetchProbe(ctx, c.url, apiKey)
			if err != nil {
				outCh <- probeOut{nil, err}
				return
			}
			kind, models, cerr := classify(b)
			if cerr != nil {
				outCh <- probeOut{nil, cerr}
				return
			}
			outCh <- probeOut{&ProbeResult{Provider: kind, Models: models, BaseURL: c.base}, nil}
		}(c)
	}
	var lastErr error
	for range cands {
		out := <-outCh
		if out.err == nil {
			return out.res, nil
		}
		lastErr = out.err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("nothing to try")
	}
	return nil, lastErr
}

// fetchProbe GETs a /models URL, sending auth headers for both OpenAI (Bearer)
// and Anthropic (x-api-key + anthropic-version); a local llama-server ignores
// them. Returns the raw response body, or an error describing a non-200 or
// transport failure.
func fetchProbe(ctx context.Context, url, apiKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := probeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(b))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, url, snippet)
	}
	return b, nil
}

// classify determines the provider kind from the /models list shape and builds
// the model list with context lengths found anywhere in each model's JSON
// subtree (forgiving about the data shape). Provider signature: Anthropic
// carries first_id/last_id/has_more at the top level; llama.cpp items carry an
// "architecture" block; OpenAI-compatible is everything else.
func classify(raw []byte) (string, []provider.ModelInfo, error) {
	var top struct {
		Data    []map[string]any `json:"data"`
		FirstID string           `json:"first_id"`
		LastID  string           `json:"last_id"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", nil, fmt.Errorf("could not parse the /models response: %w", err)
	}
	if len(top.Data) == 0 {
		return "", nil, fmt.Errorf("the /models response listed no models")
	}
	isAnthropic := top.FirstID != "" || top.LastID != "" || top.HasMore
	hasArch := false
	for _, m := range top.Data {
		if _, ok := m["architecture"].(map[string]any); ok {
			hasArch = true
			break
		}
	}
	out := make([]provider.ModelInfo, 0, len(top.Data))
	for _, m := range top.Data {
		mi := provider.ModelInfo{}
		if id, ok := m["id"].(string); ok {
			mi.ID = id
		}
		mi.MaxContext = findContextLength(m)
		if hasArch {
			if arch, ok := m["architecture"].(map[string]any); ok {
				mi.InputModalities = toStringSlice(arch["input_modalities"])
				mi.OutputModalities = toStringSlice(arch["output_modalities"])
			}
		}
		out = append(out, mi)
	}
	kind := "openai"
	switch {
	case isAnthropic:
		kind = "anthropic"
	case hasArch:
		kind = "llama"
	default:
		// OpenAI: fill the context from the best-effort map where the JSON had
		// none (the OpenAI /models list does not expose it).
		for i := range out {
			if out[i].MaxContext == 0 {
				out[i].MaxContext = openAIContext[out[i].ID]
			}
		}
	}
	return kind, out, nil
}

// findContextLength searches a model's JSON subtree (any depth) for a
// context-window length under one of ctxFieldKeys, returning the first
// positive value found.
func findContextLength(node any) int {
	switch v := node.(type) {
	case map[string]any:
		for _, k := range ctxFieldKeys {
			if n, ok := toInt(v[k]); ok && n > 0 {
				return n
			}
		}
		for _, val := range v {
			if n := findContextLength(val); n > 0 {
				return n
			}
		}
	case []any:
		for _, val := range v {
			if n := findContextLength(val); n > 0 {
				return n
			}
		}
	}
	return 0
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// openAIContext is a best-effort map of context-window sizes for common OpenAI
// models (the OpenAI /models list does not expose them). Unknown models get 0
// (= auto / the agent decides). Values are the prompt context windows, in
// tokens.
var openAIContext = map[string]int{
	"gpt-4o":              128000,
	"gpt-4o-2024-11-20":   128000,
	"gpt-4o-2024-08-06":   128000,
	"gpt-4o-mini":         128000,
	"gpt-4o-mini-2024-07-18": 128000,
	"gpt-4-turbo":         128000,
	"gpt-4-turbo-2024-04-09": 128000,
	"gpt-4":               128000,
	"gpt-3.5-turbo":       16385,
	"gpt-3.5-turbo-0125":  16385,
	"o1":                  200000,
	"o1-mini":             65536,
	"o1-mini-2024-09-12":  65536,
	"o1-preview":          128000,
	"o3":                  200000,
	"o3-mini":             200000,
	"o3-mini-2025-01-31":  200000,
	"o4-mini":             200000,
	"o4-mini-2025-04-16":  200000,
	"gpt-4.1":             1047576,
	"gpt-4.1-mini":        1047576,
	"gpt-4.1-nano":        1047576,
}
