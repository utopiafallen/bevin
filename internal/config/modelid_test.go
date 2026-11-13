package config

import "testing"

// TestModelIDSlug — the internal model id derived from a provider model id:
// lowercased, non-alphanumerics collapsed to dashes, capped, never empty.
func TestModelIDSlug(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":            "gpt-4o",
		"claude-3-5-sonnet": "claude-3-5-sonnet",
		"Meta-Llama-3.1-8B": "meta-llama-3-1-8b",
		"embeddinggemma-2":  "embeddinggemma-2",
		"":                  "model",
		"///___":            "model",
	}
	for in, want := range cases {
		if got := ModelIDSlug(in); got != want {
			t.Errorf("ModelIDSlug(%q) = %q, want %q", in, got, want)
		}
	}
	// 40-char cap.
	long := ModelIDSlug("a-very-long-model-identifier-that-keeps-going-and-going")
	if len(long) > 40 {
		t.Errorf("ModelIDSlug cap violated: len(%q) = %d", long, len(long))
	}
}

// TestAssignModelIDs — missing internal ids are derived deterministically,
// existing ids are kept, and collisions get a numeric suffix so two entries
// can never collapse onto one slot pool.
func TestAssignModelIDs(t *testing.T) {
	models := []Model{
		{ID: "kept", ModelID: "whatever"},
		{ModelID: "gpt-4o"},
		{ModelID: "gpt-4o"}, // collides with the previous derivation
		{ModelID: "GPT-4O"}, // slug collides again (case-insensitive)
	}
	assignModelIDs(models)
	if models[0].ID != "kept" {
		t.Errorf("existing id changed: %q", models[0].ID)
	}
	want := []string{"kept", "gpt-4o", "gpt-4o-2", "gpt-4o-3"}
	for i, w := range want {
		if models[i].ID != w {
			t.Errorf("model %d id = %q, want %q", i, models[i].ID, w)
		}
	}
}

// TestValidateFillsModelIDs — config.Validate repairs pre-existing files
// saved before internal ids were derived (empty ids become derived, not an
// error).
func TestValidateFillsModelIDs(t *testing.T) {
	c := ServerConfig{
		Auth: Auth{SessionSecret: "s"},
		Models: []Model{
			{Provider: "openai", BaseURL: "http://x/v1", ModelID: "gpt-4o", Slots: 1, Enabled: true},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Models[0].ID != "gpt-4o" {
		t.Errorf("Validate did not derive the model id: %q", c.Models[0].ID)
	}
}
