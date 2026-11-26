package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/prompt"
)

// TestConversationCreationBakesProfile pins the creation-time bake (design
// log 2026-11-25 "Profile in the system prompt"): a new conversation's
// frozen system prompt is the base core + the user's CURRENT profile
// section, so a new conversation knows the user from message one.
func TestConversationCreationBakesProfile(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	if err := us.SetProfile("preferences", "prefers terse answers", "curation"); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	want := prompt.SystemPrompt("preferences: prefers terse answers")
	if conv.FrozenSystemPrompt != want {
		t.Fatalf("frozen prompt = %q, want %q", conv.FrozenSystemPrompt, want)
	}
	if !strings.HasPrefix(conv.FrozenSystemPrompt, prompt.Base) {
		t.Fatal("frozen prompt does not start with the base core")
	}
}

// TestConversationCreationNoProfileIsBase: a profile-less conversation's
// frozen prompt is exactly the base core (no empty section, no trailing
// blank line — byte-identical to the pre-injection behavior).
func TestConversationCreationNoProfileIsBase(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.FrozenSystemPrompt != prompt.Base {
		t.Fatalf("frozen prompt = %q, want exactly the base core", conv.FrozenSystemPrompt)
	}
}

// TestCompactionRebakesProfile pins the compaction re-bake: a compaction
// event refreshes the frozen prompt with the CURRENT profile (entries
// written after the conversation was created), leaves the core bytes
// untouched, and the re-baked prompt is the exact core + sorted KVs.
func TestCompactionRebakesProfile(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()
	if cfg.Models[0].MaxContext != 1000 {
		t.Fatalf("test precondition: m1 MaxContext = %d, want 1000", cfg.Models[0].MaxContext)
	}
	fp := &fakeProvider{}
	a.providers["m1"] = fp
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	if err := us.SetProfile("preferences", "an old profile note", "curation"); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !strings.Contains(conv.FrozenSystemPrompt, "an old profile note") {
		t.Fatalf("creation bake missing the profile: %q", conv.FrozenSystemPrompt)
	}
	// A new profile entry lands after creation (a curation run).
	if err := us.SetProfile("work", "works at Acme", "curation"); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	// Grow the conversation past the summarization budget so the
	// compaction fires (the same shape as TestCompactPreservesLastMessages).
	for i := 0; i < 45; i++ {
		if _, err := us.AppendMessage(convID, roleUser, strings.Repeat("m", 100), "", "", nil); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	conv, err = us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if err := a.compactIfNeeded(context.Background(), us, "u1", conv, fp, 0, profile.Default(), false, nil); err != nil {
		t.Fatalf("compactIfNeeded: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var summaries int
	for _, m := range msgs {
		if m.Role == roleSummary {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("summaries = %d, want 1 (the compaction fired)", summaries)
	}
	// The frozen prompt was re-baked: the current profile, the core
	// untouched, the exact core + sorted KVs.
	fresh, err := us.GetConversation(convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !strings.HasPrefix(fresh.FrozenSystemPrompt, prompt.Base) {
		t.Fatalf("re-bake rewrote the core: %q", fresh.FrozenSystemPrompt)
	}
	want := prompt.SystemPrompt("preferences: an old profile note\nwork: works at Acme")
	if fresh.FrozenSystemPrompt != want {
		t.Fatalf("re-baked prompt = %q, want %q", fresh.FrozenSystemPrompt, want)
	}
}

// TestSetProfileKeyCountCap pins the hard key-count bound (design log
// 2026-11-25): the profile is injected into every system prompt, so
// "a handful of small notes" is enforced — a NEW key is refused at the
// cap, an upsert of an existing key is never refused.
func TestSetProfileKeyCountCap(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	tool, ok := a.tools.Get("set_profile")
	if !ok {
		t.Fatal("set_profile is not registered")
	}
	for i := 0; i < profileMaxKeys; i++ {
		if err := us.SetProfile(fmt.Sprintf("k%02d", i), "v", "curation"); err != nil {
			t.Fatalf("SetProfile: %v", err)
		}
	}
	convID, _ := makeCurationConv(t, a, us, "u1")
	ctx := context.WithValue(context.Background(), ctxConversationKey, convID)
	// A new key at the cap: refused.
	res, err := tool.Execute(ctx, "u1", []byte(`{"key":"overflow","value":"v"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, fmt.Sprintf("max %d entries", profileMaxKeys)) {
		t.Fatalf("new-key result = %q, want the cap refusal", res)
	}
	// An upsert of an existing key at the cap: accepted.
	res, err = tool.Execute(ctx, "u1", []byte(`{"key":"k00","value":"updated"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(res, "profile updated") {
		t.Fatalf("upsert result = %q, want the update confirmation", res)
	}
	p, err := us.GetProfile()
	if err != nil || len(p) != profileMaxKeys || p["k00"] != "updated" {
		t.Fatalf("profile = %v (err %v), want %d entries with k00 updated", p, err, profileMaxKeys)
	}
}
