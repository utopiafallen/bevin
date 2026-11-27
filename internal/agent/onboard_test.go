package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/store"
)

// TestOnboardingStateRoundtrip — the explicit state rows: a fresh user is
// not complete; MarkOnboarded records both rows; OnboardingState reports
// them back (the store row, not the conversation, is the truth).
func TestOnboardingStateRoundtrip(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	if done, id, err := a.OnboardingState("u1"); err != nil || done || id != "" {
		t.Fatalf("fresh state = (%v, %q, %v), want (false, \"\", nil)", done, id, err)
	}
	if err := a.MarkOnboarded("u1", "c1"); err != nil {
		t.Fatalf("MarkOnboarded: %v", err)
	}
	done, id, err := a.OnboardingState("u1")
	if err != nil || !done || id != "c1" {
		t.Fatalf("state after mark = (%v, %q, %v), want (true, \"c1\", nil)", done, id, err)
	}
}

// TestOnboardCompleteNoConversation — completion with a DELETED conversation
// stays complete: Onboard returns "" (no re-seed) instead of creating a new
// "Meet Bevin" conversation. The state, not the artifact, is the truth —
// deleting the conversation must not re-run onboarding.
func TestOnboardCompleteNoConversation(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	if err := a.MarkOnboarded("u1", "gone"); err != nil {
		t.Fatalf("MarkOnboarded: %v", err)
	}
	id, err := a.Onboard(context.Background(), "u1", "m1", "hello")
	if err != nil || id != "" {
		t.Fatalf("Onboard with a deleted conversation = (%q, %v), want (\"\", nil)", id, err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	if _, err := us.FindConversationByTitle(onboardingTitle); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted onboarding conversation was re-seeded (lookup err = %v, want ErrNotFound)", err)
	}
}

// TestOnboardCrashRecovery — a prior attempt that seeded the conversation
// and died before recording completion: Onboard ADOPTS it (same id, the
// welcome seeded, the state recorded) instead of creating a second
// conversation. The title lookup is recovery here — the state row is the
// idempotency key.
func TestOnboardCrashRecovery(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	crashed, err := us.CreateConversation(onboardingTitle, "m1")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	id, err := a.Onboard(context.Background(), "u1", "m1", "Hi u1 — welcome")
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if id != crashed.ID {
		t.Fatalf("adopted id = %q, want the pre-seeded %q", id, crashed.ID)
	}
	msgs, err := us.ListMessages(crashed.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "Hi u1") {
		t.Fatalf("adopted messages = %+v, want the one welcome", msgs)
	}
	done, stored, err := a.OnboardingState("u1")
	if err != nil || !done || stored != crashed.ID {
		t.Fatalf("state after recovery = (%v, %q, %v), want (true, %q, nil)", done, stored, err, crashed.ID)
	}
}

// TestOnboardFreshSeed — a truly fresh user: Onboard seeds the conversation
// (title, model, frozen prompt, one welcome) AND records completion; a
// second call returns the same conversation without duplicating the welcome.
func TestOnboardFreshSeed(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()

	id, err := a.Onboard(context.Background(), "u1", "m1", "Hi — welcome")
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if id == "" {
		t.Fatal("fresh Onboard returned no conversation id")
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	conv, err := us.GetConversation(id)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.Title != onboardingTitle || conv.ActiveModelID != "m1" || conv.FrozenSystemPrompt == "" {
		t.Fatalf("seeded conversation = %+v, want title %q model m1 frozen prompt", conv, onboardingTitle)
	}
	msgs, err := us.ListMessages(id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "assistant" {
		t.Fatalf("seeded messages = %+v, want one assistant welcome", msgs)
	}
	// Second call: same conversation, no duplicate welcome.
	id2, err := a.Onboard(context.Background(), "u1", "m1", "Hi — welcome again")
	if err != nil || id2 != id {
		t.Fatalf("second Onboard = (%q, %v), want (%q, nil)", id2, err, id)
	}
	msgs, _ = us.ListMessages(id)
	if len(msgs) != 1 {
		t.Fatalf("welcome duplicated on re-onboard: %d messages", len(msgs))
	}
}
