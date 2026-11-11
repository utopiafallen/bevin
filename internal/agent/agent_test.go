package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
)

func TestIsModelUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"openai 404 does not exist", errors.New(`openai: 404 Not Found: {"error":{"message":"The model 'gpt-9' does not exist"}}`), true},
		{"anthropic model not found", errors.New(`anthropic: 404 Not Found: {"type":"error","message":"model: Not found"}`), true},
		{"llama unknown model", errors.New(`llama: 404 Not Found: unknown model 'foo'`), true},
		{"generic 404 no model", errors.New(`openai: 404 Not Found: page not found`), false},
		{"rate limit", errors.New(`openai: 429 Too Many Requests: rate limit exceeded`), false},
		{"timeout", errors.New(`context deadline exceeded`), false},
		{"connection refused", errors.New(`openai: Post http://x: dial tcp: connection refused`), false},
		{"500 server error", errors.New(`openai: 500 Internal Server Error: boom`), false},
	}
	for _, c := range cases {
		if got := isModelUnavailable(c.err); got != c.want {
			t.Errorf("%s: isModelUnavailable = %v, want %v", c.name, got, c.want)
		}
	}
}

func newTestAgent(t *testing.T) (*Agent, *config.ServerConfig, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.ServerConfig{
		Version: config.ConfigVersion,
		Auth:    config.Auth{Mode: "internal", SessionSecret: "test-secret"},
		Models: []config.Model{
			{ID: "m1", Provider: "openai", BaseURL: "http://u", ModelID: "x", DisplayName: "X", MaxContext: 1000, Slots: 1, Enabled: true},
			{ID: "m2", Provider: "openai", BaseURL: "http://u", ModelID: "y", DisplayName: "Y", MaxContext: 1000, Slots: 1, Enabled: true},
		},
	}
	a, err := New(cfg, dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, cfg, dir
}

func TestSetModelAutoDisabledPersists(t *testing.T) {
	a, cfg, dir := newTestAgent(t)
	defer a.Close()

	// Initially no model is auto-disabled.
	if cfg.Models[0].AutoDisabled || cfg.Models[1].AutoDisabled {
		t.Fatal("expected no model to start auto-disabled")
	}

	// Auto-disable m1 -> the in-memory config + the persisted config update.
	a.SetModelAutoDisabled("m1", true)
	if !cfg.Models[0].AutoDisabled {
		t.Fatal("m1 should be auto-disabled in memory")
	}
	if cfg.Models[1].AutoDisabled {
		t.Fatal("m2 should not be auto-disabled")
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load after auto-disable: %v", err)
	}
	if !loaded.Models[0].AutoDisabled || loaded.Models[1].AutoDisabled {
		t.Fatalf("persisted state = %v/%v, want true/false", loaded.Models[0].AutoDisabled, loaded.Models[1].AutoDisabled)
	}

	// Re-enable m1 (a later success) -> persisted state clears.
	a.SetModelAutoDisabled("m1", false)
	loaded, err = config.Load(dir)
	if err != nil {
		t.Fatalf("Load after re-enable: %v", err)
	}
	if loaded.Models[0].AutoDisabled {
		t.Fatal("m1 should be re-enabled (auto-disabled cleared) in persisted config")
	}

	// Unknown model -> no-op (no panic, no change).
	a.SetModelAutoDisabled("nope", true)
	if cfg.Models[0].AutoDisabled || cfg.Models[1].AutoDisabled {
		t.Fatal("unknown model should not change state")
	}
}

func TestModelsFiltersAutoDisabled(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	// Both models available -> both returned.
	if got := len(a.Models()); got != 2 {
		t.Fatalf("Models() = %d, want 2", got)
	}
	// Auto-disable m1 -> only m2 returned.
	a.SetModelAutoDisabled("m1", true)
	got := a.Models()
	if len(got) != 1 || got[0].ID != "m2" {
		t.Fatalf("Models() after auto-disable = %+v, want only m2", got)
	}
	// AllModels still returns both (with the auto-disabled state).
	if got := len(a.AllModels()); got != 2 {
		t.Fatalf("AllModels() = %d, want 2", got)
	}
}

var _ = fmt.Sprintf
