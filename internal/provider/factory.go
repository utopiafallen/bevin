package provider

import "fmt"

// New constructs a Provider for the given kind. Kinds are the values used in
// server.toml: "openai" | "anthropic" | "llama".
func New(kind, baseURL, apiKey string) (Provider, error) {
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