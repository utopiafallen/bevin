package profile

import (
	"strings"
	"testing"
)

// TestGeneralIsTheDefault pins the default profile's identity + the values
// the agent's request builder relies on. "general" is the behavior-neutral
// formalization of what the harness does today, so these values ARE the
// current behavior — a change here is a behavior change and needs a
// conscious decision (not an accidental edit).
func TestGeneralIsTheDefault(t *testing.T) {
	g := Default()
	if g.Name != General {
		t.Fatalf("name = %q, want %q", g.Name, General)
	}
	if g.CharsPerToken != 4 {
		t.Fatalf("CharsPerToken = %d, want 4 (chars/4, the rule of thumb it formalizes)", g.CharsPerToken)
	}
	if g.KeepLastMessages != 5 {
		t.Fatalf("KeepLastMessages = %d, want 5 (the verbatim tail the request builder relies on)", g.KeepLastMessages)
	}
	if !g.ReasoningInTranscript {
		t.Fatal("ReasoningInTranscript = false, want true (the send-all-reasoning-back default)")
	}
	if !strings.Contains(g.SummaryFraming("S"), "summary") {
		t.Fatalf("SummaryFraming = %q, want it to frame the summary text", g.SummaryFraming("S"))
	}
	if !strings.Contains(g.SummaryFraming("S"), "S") {
		t.Fatalf("SummaryFraming must keep the summary text intact")
	}
	// It is THE default.
	if Default().Name != General {
		t.Fatalf("Default = %q, want %q", Default().Name, General)
	}
}

// TestRegistry pins the registry's basics: general is registered, lookup
// finds it, and the listing is stable + complete (the UI renders from it).
func TestRegistry(t *testing.T) {
	g, err := Get(General)
	if err != nil {
		t.Fatalf("Get(%q): %v", General, err)
	}
	if g.Name != General {
		t.Fatalf("lookup returned %q", g.Name)
	}
	// Lookup tolerates surrounding whitespace (a config value with a stray
	// space must not read as an unknown profile).
	g2, err := Get("  " + General + "  ")
	if err != nil {
		t.Fatalf("Get with whitespace: %v", err)
	}
	if g2.Name != General {
		t.Fatalf("whitespace lookup returned %q", g2.Name)
	}
	// The listing is non-empty and contains the default first (the UI's
	// select renders general at the top).
	list := List()
	if len(list) == 0 {
		t.Fatal("List() is empty")
	}
	if list[0].Name != General {
		t.Fatalf("List()[0] = %q, want %q first", list[0].Name, General)
	}
	for _, p := range list {
		if p.Name == "" || p.Description == "" {
			t.Fatalf("profile %q: name/description must be set for the UI", p.Name)
		}
	}
}

// TestUnknownProfileFailsLoudly is the "fail loudly" contract: an unknown
// name (a typo in server.toml, a profile a newer build dropped) is an error,
// never a silent fall-back — a silently changed harness behavior is worse
// than a startup failure.
func TestUnknownProfileFailsLoudly(t *testing.T) {
	_, err := Get("definitely-not-a-profile")
	if err == nil {
		t.Fatal("Get(unknown) = no error, want a loud one")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-profile") {
		t.Fatalf("error = %q, want it to name the unknown profile", err)
	}
	// A blank name is NOT an unknown name (it self-heals to the default at
	// the config layer) — Get treats it as unknown, but the config layer is
	// what self-heals; here we only pin that Get itself is strict.
	if _, err := Get(""); err == nil {
		t.Fatal("Get(\"\") = no error (strict lookup must not invent a profile)")
	}
}

// TestQwen38Profile pins the first model-specific profile: general's harness
// behavior + the reasoning-effort levels (the per-conversation control's
// option list). The harness values must stay equal to general's — qwen3.8 is
// a control addition, not a behavior change.
func TestQwen38Profile(t *testing.T) {
	p, err := Get("qwen3.8")
	if err != nil {
		t.Fatalf("Get(qwen3.8): %v", err)
	}
	g := Default()
	if p.ReasoningInTranscript != g.ReasoningInTranscript ||
		p.KeepLastMessages != g.KeepLastMessages ||
		p.CompactionTriggerFraction != g.CompactionTriggerFraction ||
		p.CharsPerToken != g.CharsPerToken {
		t.Fatalf("qwen3.8 deviates from general's harness behavior: %+v", p)
	}
	want := []string{"low", "medium", "xhigh"}
	if len(p.ReasoningEfforts) != len(want) {
		t.Fatalf("ReasoningEfforts = %v, want %v", p.ReasoningEfforts, want)
	}
	for i := range want {
		if p.ReasoningEfforts[i] != want[i] {
			t.Fatalf("ReasoningEfforts = %v, want %v", p.ReasoningEfforts, want)
		}
	}
}

// TestReasoningEffortFor pins the request builder's gate: a conversation's
// stored choice is sent only when the profile supports it. Everything else —
// a profile with no efforts, an empty choice, a level the profile doesn't
// know (stale after a profile reassignment) — resolves to "" (nothing on the
// wire; the provider's own default applies). Surrounding whitespace is
// tolerated.
func TestReasoningEffortFor(t *testing.T) {
	// general: no effort support at all — always "".
	if got := Default().ReasoningEffortFor("low"); got != "" {
		t.Fatalf("general ReasoningEffortFor(low) = %q, want \"\"", got)
	}
	q, err := Get("qwen3.8")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"low", "medium", "xhigh"} {
		if got := q.ReasoningEffortFor(e); got != e {
			t.Fatalf("qwen3.8 ReasoningEffortFor(%q) = %q, want it back", e, got)
		}
	}
	if got := q.ReasoningEffortFor("  low  "); got != "low" {
		t.Fatalf("qwen3.8 ReasoningEffortFor(whitespace) = %q, want \"low\" (trimmed)", got)
	}
	if got := q.ReasoningEffortFor("high"); got != "" {
		t.Fatalf("qwen3.8 ReasoningEffortFor(high) = %q, want \"\" (not a supported level)", got)
	}
	if got := q.ReasoningEffortFor(""); got != "" {
		t.Fatalf("qwen3.8 ReasoningEffortFor(\"\") = %q, want \"\" (nothing stored)", got)
	}
}
