package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/utopiafallen/alcyona-agent/internal/provider"
)

// probeClient is a short-timeout client for endpoint probing, so a dead or
// unreachable endpoint can't hang the wizard for minutes.
var probeClient = &http.Client{Timeout: 15 * time.Second}

// ProbeResult is the outcome of probing an endpoint: the detected provider
// kind + the models it serves (with context lengths where the API exposes
// them).
type ProbeResult struct {
	Provider string                 `json:"provider"`
	Models   []provider.ModelInfo   `json:"models"`
}

// probeModel is the union of the per-model fields the three providers expose:
// the shared id, Anthropic's max_input_tokens, and llama.cpp's architecture.
type probeModel struct {
	ID             string `json:"id"`
	MaxInputTokens int    `json:"max_input_tokens"` // Anthropic
	Architecture   struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"` // llama.cpp
	Meta struct {
		Nctx int `json:"n_ctx"` // llama.cpp context length
	} `json:"meta"` // llama.cpp
}

// probeList is the union of the /models list shapes. Anthropic's list carries
// first_id / last_id / has_more at the top level; OpenAI's and llama.cpp's do
// not, so their presence is the Anthropic signature.
type probeList struct {
	Data    []probeModel `json:"data"`
	FirstID string       `json:"first_id"`
	LastID  string       `json:"last_id"`
	HasMore bool         `json:"has_more"`
}

// probeEndpoint detects the provider kind for baseURL by testing the /models
// signature, then lists the models it serves (ids + context lengths where
// available). baseURL is the API base (e.g. https://api.openai.com/v1); both
// {base}/models and {base}/v1/models are tried so a base with or without the
// /v1 prefix works.
func probeEndpoint(ctx context.Context, baseURL, apiKey string) (*ProbeResult, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("enter the API base URL first")
	}
	cands := []string{base + "/models"}
	if !strings.HasSuffix(base, "/v1") {
		cands = append(cands, base+"/v1/models")
	}
	var lastErr error
	for _, u := range cands {
		pl, err := fetchProbe(ctx, u, apiKey)
		if err != nil {
			lastErr = err
			continue
		}
		kind, models := classify(pl)
		if kind == "" {
			lastErr = fmt.Errorf("%s returned a /models list we couldn't recognize", u)
			continue
		}
		return &ProbeResult{Provider: kind, Models: models}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("nothing to try")
	}
	return nil, lastErr
}

// fetchProbe GETs a /models URL, sending auth headers for both OpenAI (Bearer)
// and Anthropic (x-api-key + anthropic-version); a local llama-server ignores
// them. Returns the parsed list, or an error describing a non-200 / parse
// failure.
func fetchProbe(ctx context.Context, url, apiKey string) (*probeList, error) {
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
	var pl probeList
	if err := json.Unmarshal(b, &pl); err != nil {
		return nil, fmt.Errorf("could not parse the /models response from %s: %w", url, err)
	}
	return &pl, nil
}

// classify determines the provider kind from the /models list shape and builds
// the model list with context lengths where the API provides them:
//   - Anthropic: first_id/last_id/has_more present; context = max_input_tokens.
//   - llama.cpp: items carry an architecture block; context is a server setting
//     (left 0 = auto).
//   - OpenAI-compatible: everything else; context is a best-effort known map
//     (the OpenAI /models list does not expose it).
func classify(pl *probeList) (string, []provider.ModelInfo) {
	if len(pl.Data) == 0 {
		return "", nil
	}
	isAnthropic := pl.FirstID != "" || pl.LastID != "" || pl.HasMore
	hasArch := false
	for _, m := range pl.Data {
		if m.Architecture.InputModalities != nil || m.Architecture.OutputModalities != nil {
			hasArch = true
			break
		}
	}
	out := make([]provider.ModelInfo, 0, len(pl.Data))
	switch {
	case isAnthropic:
		for _, m := range pl.Data {
			out = append(out, provider.ModelInfo{ID: m.ID, MaxContext: m.MaxInputTokens})
		}
		return "anthropic", out
	case hasArch:
		for _, m := range pl.Data {
			out = append(out, provider.ModelInfo{
				ID:               m.ID,
				InputModalities:  m.Architecture.InputModalities,
				OutputModalities: m.Architecture.OutputModalities,
				MaxContext:       m.Meta.Nctx,
			})
		}
		return "llama", out
	default:
		for _, m := range pl.Data {
			out = append(out, provider.ModelInfo{ID: m.ID, MaxContext: openAIContext[m.ID]})
		}
		return "openai", out
	}
}

// openAIContext is a best-effort map of context-window sizes for common OpenAI
// models (the OpenAI /models list does not expose them). Unknown models get 0
// (= auto / the agent decides). Values are the prompt context windows, in
// tokens.
var openAIContext = map[string]int{
	"gpt-4o":           128000,
	"gpt-4o-2024-11-20": 128000,
	"gpt-4o-2024-08-06": 128000,
	"gpt-4o-mini":      128000,
	"gpt-4o-mini-2024-07-18": 128000,
	"gpt-4-turbo":      128000,
	"gpt-4-turbo-2024-04-09": 128000,
	"gpt-4":            128000,
	"gpt-3.5-turbo":    16385,
	"gpt-3.5-turbo-0125": 16385,
	"o1":               200000,
	"o1-mini":          65536,
	"o1-mini-2024-09-12": 65536,
	"o1-preview":       128000,
	"o3":               200000,
	"o3-mini":          200000,
	"o3-mini-2025-01-31": 200000,
	"o4-mini":          200000,
	"o4-mini-2025-04-16": 200000,
	"gpt-4.1":          1047576,
	"gpt-4.1-mini":     1047576,
	"gpt-4.1-nano":     1047576,
}
