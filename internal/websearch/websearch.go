// Package websearch is the search backend for the agent's web_search tool.
//
// The tool (internal/agent) is PROVIDER-AGNOSTIC by design (user directive):
// it reads the admin's provider choice + settings from the live config,
// resolves a Provider here, and formats the provider-neutral results — no
// vendor's API shape leaks into the tool. Adding a search provider = a new
// Provider implementation + a registry entry in New/Providers; the tool,
// the config schema, and the admin UI don't change (the same "no vendor
// if/else in the harness" rule internal/provider follows for model
// endpoints).
package websearch

import (
	"context"
	"fmt"
)

// Result is one search result, provider-neutral: what every search API can
// give bevin — a title, a URL, an optional publication date, and
// relevance-sized excerpts. A provider that can't fill a field leaves it
// empty.
type Result struct {
	Title         string
	URL           string
	PublishedDate string
	Highlights    []string
}

// Config is one search provider's admin-entered settings (what the admin's
// Tools page collects for the chosen provider; v1: an API key).
type Config struct {
	APIKey string
}

// Provider is one search API backend behind the web_search tool.
type Provider interface {
	// Name is the provider's config name (what the admin config stores:
	// "exa").
	Name() string
	// Search runs one search: a natural-language query + a result count →
	// ranked results. The implementation owns its API's quirks (endpoint,
	// auth, request/response shape, content mode) and returns
	// provider-neutral results.
	Search(ctx context.Context, query string, numResults int) ([]Result, error)
}

// New resolves a provider by its config name, built with the admin's
// settings (v1 providers are stateless, so the build is cheap — the tool
// re-resolves from LIVE config on every call, which is what makes an admin
// key edit apply from the next call with no cache to invalidate). An
// unknown name is an error (config.Validate already rejects one while web
// search is enabled — this is the runtime's defensive twin: a turn must
// not crash on a config anomaly).
func New(name string, cfg Config) (Provider, error) {
	switch name {
	case "exa":
		return newExa(cfg), nil
	default:
		return nil, fmt.Errorf("unknown search provider %q (this build knows: %s)", name, "exa")
	}
}

// Providers lists the provider names this build knows. It is the source of
// truth for the admin UI's provider select (like the profile registry: the
// UI never hardcodes the list).
func Providers() []string {
	return []string{"exa"}
}
