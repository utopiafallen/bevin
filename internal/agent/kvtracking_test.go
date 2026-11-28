package agent

import (
	"context"
	"testing"
)

// TestResolveModelRefTracked verifies the bevin-side KV-cache backend
// preference in model resolution (2026-11-27): a display name served by
// several providers resolves to the provider on the tracked KV-cache holder
// (cache locality beats least-loaded); a stale tracking (a backend gone from
// the config) falls back to the plain least-loaded rule; concrete model IDs
// resolve to themselves regardless of the tracking.
func TestResolveModelRefTracked(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	// Two providers of one display name (the KV-cache affinity scenario):
	// both llama, on different backends.
	cfg.Models[0].DisplayName = "Qwen"
	cfg.Models[0].Provider = "llama"
	cfg.Models[0].BaseURL = "http://a:1/v1"
	cfg.Models[1].DisplayName = "Qwen"
	cfg.Models[1].Provider = "llama"
	cfg.Models[1].BaseURL = "http://b:1/v1"

	// No tracking: least-loaded (a tie breaks by catalog order -> m1).
	if id, ok := a.resolveModelRefTracked("Qwen", ""); !ok || id != "m1" {
		t.Fatalf("untracked = (%q, %v), want (m1, true) (least-loaded, tie by catalog order)", id, ok)
	}
	// Tracked on the SECOND provider's backend: the holder wins over
	// least-loaded (m2 is not more idle — the preference is cache locality,
	// not load).
	if id, ok := a.resolveModelRefTracked("Qwen", "http://b:1/v1"); !ok || id != "m2" {
		t.Fatalf("tracked b = (%q, %v), want (m2, true) (the KV-cache holder beats least-loaded)", id, ok)
	}
	// Tracked on the first provider's backend: the same preference for m1.
	if id, ok := a.resolveModelRefTracked("Qwen", "http://a:1/v1"); !ok || id != "m1" {
		t.Fatalf("tracked a = (%q, %v), want (m1, true)", id, ok)
	}
	// A stale tracking (a backend gone from the config): the plain
	// least-loaded rule applies.
	if id, ok := a.resolveModelRefTracked("Qwen", "http://gone:9/v1"); !ok || id != "m1" {
		t.Fatalf("stale = (%q, %v), want (m1, true) (fallback to least-loaded)", id, ok)
	}
	// A concrete model ID resolves to itself, tracking or not.
	if id, ok := a.resolveModelRefTracked("m2", "http://a:1/v1"); !ok || id != "m2" {
		t.Fatalf("concrete = (%q, %v), want (m2, true) (concrete ids resolve to themselves)", id, ok)
	}
}

// TestUpdateConversationKVBackendPreference pins the model-switch path's
// use of the tracking (2026-11-27): switching a conversation to a display
// name served by several providers lands it on the provider that HOLDS the
// conversation's KV cache file (not the least-loaded one); a tracking whose
// backend is gone from the config is stale — cleared by the switch, and the
// plain least-loaded rule applies.
func TestUpdateConversationKVBackendPreference(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	cfg.Models[0].DisplayName = "Qwen"
	cfg.Models[0].Provider = "llama"
	cfg.Models[0].BaseURL = "http://a:1/v1"
	cfg.Models[1].DisplayName = "Qwen"
	cfg.Models[1].Provider = "llama"
	cfg.Models[1].BaseURL = "http://b:1/v1"
	a.providers["m1"] = &fakeProvider{}
	a.providers["m2"] = &fakeProvider{}

	ctx := context.Background()
	convID, err := a.EnsureConversation(ctx, "u1", "", "switcher")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	qwen := "Qwen"

	// A live tracking on the second provider's backend: the switch to the
	// display name lands on the holder (m2), not the least-loaded (m1).
	if err := us.SetConversationKVBackend(convID, "http://b:1/v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateConversation("u1", convID, &qwen, nil, nil, nil); err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ActiveModelID != "m2" {
		t.Fatalf("switch with live tracking = %q, want m2 (the KV-cache holder beats least-loaded)", conv.ActiveModelID)
	}

	// A stale tracking (the backend is gone from the config): the switch
	// clears it and falls back to the plain least-loaded rule (m1).
	if err := us.SetConversationKVBackend(convID, "http://gone:9/v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateConversation("u1", convID, &qwen, nil, nil, nil); err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}
	if conv, _ = us.GetConversation(convID); conv.ActiveModelID != "m1" {
		t.Fatalf("switch with stale tracking = %q, want m1 (least-loaded fallback)", conv.ActiveModelID)
	}
	if b, err := us.GetConversationKVBackend(convID); err != nil || b != "" {
		t.Fatalf("stale tracking not cleared: backend = (%q, %v), want (\"\", nil)", b, err)
	}
}

// TestTurnRecordsKVBackend verifies the turn-side half of bevin's explicit
// KV management: after a completed model call on a llama backend, the
// conversation's KV-cache holder is recorded durably (the always-save rule
// wrote the file on that backend), so model resolution can route the
// conversation back to it. A turn on a non-llama model records nothing (its
// backend holds no bevin-managed KV file).
func TestTurnRecordsKVBackend(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	cfg.Models[0].Provider = "llama"
	cfg.Models[0].BaseURL = "http://llama-a:8080/v1"
	a.providers["m1"] = &fakeProvider{}
	a.providers["m2"] = &fakeProvider{}

	ctx := context.Background()
	// The default model (m1, now llama) conversation.
	convID, err := a.EnsureConversation(ctx, "u1", "", "llama conv")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	if _, _, err := a.Turn(ctx, "u1", convID, "hi", nil); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := us.GetConversationKVBackend(convID); err != nil || b != "http://llama-a:8080/v1" {
		t.Fatalf("kv backend = (%q, %v), want (\"http://llama-a:8080/v1\", nil)", b, err)
	}

	// A conversation switched to the non-llama model: no holder is recorded.
	convID2, err := a.EnsureConversation(ctx, "u1", "", "openai conv")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	m2 := "m2"
	if _, err := a.UpdateConversation("u1", convID2, &m2, nil, nil, nil); err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}
	if _, _, err := a.Turn(ctx, "u1", convID2, "hi", nil); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if b, err := us.GetConversationKVBackend(convID2); err != nil || b != "" {
		t.Fatalf("kv backend (non-llama) = (%q, %v), want (\"\", nil)", b, err)
	}
}
