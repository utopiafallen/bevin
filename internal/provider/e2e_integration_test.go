package provider

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestE2ECacheHitIntegration is a manual end-to-end integration test that
// drives the real Llama provider against a REAL llama-server (with the fork's
// cache-hit action). It SKIPs unless a server is listening on
// 127.0.0.1:18092 with a SINGLE slot and --slot-save-path /tmp/e2e-slot-save
// (e.g. ./bin/llama-server -m <model> --port 18092 --parallel 1 --no-ui
// --jinja --slot-save-path /tmp/e2e-slot-save).
//
// It verifies the whole flow end to end: the cache-hit check runs on every
// turn without breaking the request, a same-conversation turn (a high match)
// neither saves nor restores, and a low-match turn (a different conversation
// sharing the single slot) saves the prior conversation + restores this one.
// Saves are detected by the on-disk cache_conv_<id>.bin files (a restore of a
// not-yet-existing file is a no-op and creates none).
//
// A's follow-up is short (so the cached prefix dominates -> high match) while
// B is a long, different conversation (so the shared chat-template preamble is
// a tiny fraction -> low match). Real bevin requests carry the full
// conversation history, so the same reasoning applies there.
//
// Run manually: go test ./internal/provider/ -run TestE2ECacheHitIntegration -v
func TestE2ECacheHitIntegration(t *testing.T) {
	var longB = strings.Repeat("bravo ", 40) // ~160 tokens of B-specific text

	baseURL := "http://127.0.0.1:18092/v1"
	p := NewLlama(baseURL, "")

	// Sanity: the server is up (skip otherwise — this is a manual test).
	if _, err := p.Models(context.Background()); err != nil {
		t.Skipf("llama-server not reachable at %s: %v", baseURL, err)
	}

	files := func() map[string]bool {
		out := map[string]bool{}
		entries, err := os.ReadDir("/tmp/e2e-slot-save")
		if err != nil {
			return out
		}
		for _, e := range entries {
			out[e.Name()] = true
		}
		return out
	}
	logFiles := func(label string) {
		t.Logf("%-8s on-disk cache files: %v", label, keys(files()))
	}

	// Conversation A, turn 1 (the slot is empty -> ratio 0 -> a no-op restore).
	resA1, err := p.Chat(context.Background(), "m", ChatRequest{
		Messages:       []Message{{Role: RoleUser, Text: "What is the capital of France? One word."}},
		ConversationID: "e2eA",
	})
	if err != nil {
		t.Fatalf("A turn1: %v", err)
	}
	logFiles("A.t1")
	// A new conversation's first completed turn always gets an on-disk cache
	// entry (the first-turn save), independent of the cache-hit decision.
	if f := files(); !f["cache_conv_e2eA.bin"] {
		t.Fatalf("A's first completed turn must save its cache, files = %v", keys(f))
	}

	// Conversation A, turn 2 (the SAME conversation continues with the model's
	// ACTUAL completion replayed + a short follow-up -> the slot's cache is a
	// large prefix of this request -> high match -> NO save, NO restore).
	resA2, err := p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{
			{Role: RoleUser, Text: "What is the capital of France? One word."},
			{Role: RoleAssistant, Text: resA1.Text},
			{Role: RoleUser, Text: "And its population?"},
		},
		ConversationID: "e2eA",
	})
	if err != nil {
		t.Fatalf("A turn2: %v", err)
	}
	logFiles("A.t2")
	// A same-conversation continuation (high match) must not switch or save
	// B: no cache_conv_e2eB.bin may appear. (A's own file exists from its
	// first-turn save.)
	if f := files(); f["cache_conv_e2eB.bin"] {
		t.Fatalf("A's high-match continuation must not save/restore B, files = %v", keys(f))
	}

	// Conversation B (a different, LONG conversation -> low match -> save A +
	// restore B, and B's first turn saves B's cache).
	_, err = p.Chat(context.Background(), "m", ChatRequest{
		Messages:       []Message{{Role: RoleUser, Text: longB}},
		ConversationID: "e2eB",
	})
	if err != nil {
		t.Fatalf("B turn1: %v", err)
	}
	logFiles("B.t1")
	if f := files(); !f["cache_conv_e2eA.bin"] {
		t.Fatalf("switching to B must have saved A's cache, files = %v", keys(f))
	}

	// Back to A (the slot now holds B -> low match -> save B + restore A).
	_, err = p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{
			{Role: RoleUser, Text: "What is the capital of France? One word."},
			{Role: RoleAssistant, Text: resA1.Text},
			{Role: RoleUser, Text: "And its population?"},
			{Role: RoleAssistant, Text: resA2.Text},
			{Role: RoleUser, Text: "Is it in Europe?"},
		},
		ConversationID: "e2eA",
	})
	if err != nil {
		t.Fatalf("A turn3: %v", err)
	}
	logFiles("A.t3")
	f := files()
	if !f["cache_conv_e2eA.bin"] || !f["cache_conv_e2eB.bin"] {
		t.Fatalf("after the A->B->A switches both conversations must have a saved cache, files = %v", keys(f))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
