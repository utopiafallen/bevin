package provider

import (
	"fmt"
	"net/url"
)

// New constructs a Provider for the given kind. Kinds are the values used in
// server.toml: "openai" | "anthropic" | "llama". The base URL must be an
// absolute http(s) URL — a schemeless "host:port" is rejected here (it used
// to slip through url.Parse and only fail at request time with
// "unsupported protocol scheme").
func New(kind, baseURL, apiKey string) (Provider, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("base URL %q must be an absolute http(s) URL, e.g. http://host:port/v1", baseURL)
	}
	switch kind {
	case "openai":
		return NewOpenAI(baseURL, apiKey), nil
	case "anthropic":
		return NewAnthropic(baseURL, apiKey), nil
	case "llama":
		return NewLlama(baseURL, apiKey), nil
	default:
		return nil, fmt.Errorf("unknown provider kind %q", kind)
	}
}
