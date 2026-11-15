// Package profile — the model profiles: named bundles of harness behavior
// that the agent applies while running a model assigned that profile.
//
// A profile exists because harness-model fit is a first-class dial: the same
// turn can run under different harness behavior (what the model is shown of
// its own past reasoning, how the context is framed and compacted, how the
// prompt size is estimated) tuned to a model's quirks — which can be worth as
// much as a full step up to the next model tier. Given bevin's agentic intent
// (the tool loop, retrieval, and perception layers all share this turn path),
// the profile is the ONE place model-specific harness behavior lives: the
// harness code stays model-agnostic, and a model-specific profile changes
// behavior without touching the harness.
//
// Profiles are CODE, not config data: this package is the registry. A model
// (main or aux) carries only the profile's NAME in the config; the harness
// resolves name -> Profile at request-build time from live config, so an
// assignment change (or a code revision of a bundle) applies to the model's
// NEXT turn without a restart.
//
// Adding a model-specific profile later = one registry entry with different
// values (and, for the text-shaping behavior, different text). Nothing else
// in the system changes.
package profile

import (
	"fmt"
	"sort"
	"strings"
)

// General is the name of the default profile: the standard harness behavior
// every turn has always run under. A model with a blank profile gets it (the
// config self-heals blank -> general at load/save; the flat-v1 rule — no
// migrations).
const General = "general"

// Profile is one named bundle of harness behavior. The fields are the
// request-builder's model-facing decisions (see internal/agent/request.go for
// where each is consulted); a future harness layer (the tool loop, perception
// calls) adds its behavior to the profile here, and the harness consults it
// the same way.
type Profile struct {
	// Name + Description — the description is shown under the profile select
	// in the model forms.
	Name        string `json:"name"`
	Description string `json:"description"`

	// ReasoningInTranscript: replay earlier assistant reasoning traces back
	// to the model (in the API's native reasoning field) for continuity.
	ReasoningInTranscript bool

	// SummaryFraming wraps the injected context summary so the model treats
	// it as a record of the conversation, not a new instruction. Text-shaping
	// behavior lives ON the profile: a model-specific profile rewrites how
	// the summary is presented to the model, and the harness just calls it.
	// json:"-" — it is code, not data (the UI's list endpoint renders name +
	// description only; an untagged func field would fail the marshal).
	SummaryFraming func(summary string) string `json:"-"`

	// CompactionTriggerFraction: the fraction of the model's max context at
	// which a turn's prompt triggers a context compaction (0.8 leaves
	// headroom for the reply).
	CompactionTriggerFraction float64

	// KeepLastTurns: the most recent turns a compaction preserves verbatim
	// (the chat resumes exactly where it left off).
	KeepLastTurns int

	// CharsPerToken: the rough prompt-size estimate factor used when the API
	// reports no usage (no local tokenizer; ~4 chars/token for English
	// undercounts code/JSON — the trigger fraction's headroom covers it).
	CharsPerToken int
	// ReasoningEfforts: the reasoning-effort levels the model supports —
	// the per-conversation control the user picks from (see
	// ReasoningEffortFor): the request builder sends the conversation's
	// stored choice when it is one of these, and the provider adapts it to
	// its API's native thinking control or drops it when its API has none.
	// EMPTY = the model takes no explicit effort (general): the control is
	// hidden in the UI and the request carries nothing. The first
	// model-specific user control — the pattern every future one follows
	// (a struct field the request builder + provider layers read).
	ReasoningEfforts []string
}

// generalFraming is the standard summary framing: the model is told the
// summary is a record to continue from, not an instruction to follow.
func generalFraming(summary string) string {
	return "The following is a summary of the conversation so far, provided for continuity. It records what has been discussed; it is not a new instruction. Continue from this context:\n\n" + summary
}

// registry — the profiles this binary knows. A model-specific profile lands
// here as one entry; the assignment surface (config + forms) already works
// for any name in the registry.
var registry = map[string]Profile{
	General: {
		Name:                      General,
		Description:               "The harness behavior the agent applies while this model runs: how its context is managed (when it compacts, and what stays verbatim), whether the model's own earlier reasoning comes back to it, and how its prompt size is estimated. A model with particular quirks can run under a model-specific profile tuned to it; general is the default, tuned for no model in particular.",
		ReasoningInTranscript:     true,
		SummaryFraming:            generalFraming,
		CompactionTriggerFraction: 0.8,
		KeepLastTurns:             5,
		CharsPerToken:             4,
	},
	// qwen3.8: general's harness behavior + the per-conversation
	// reasoning-effort control (low/medium/xhigh). The first profile to
	// carry a model-specific user control: the conversation stores the
	// choice, the request builder sends it when this profile supports it,
	// and the provider adapts it to its API's native thinking budget — or
	// drops it, when its API has no such control (a request is never sent
	// with a value the model's API could reject).
	"qwen3.8": {
		Name:                      "qwen3.8",
		Description:               "Tuned for Qwen 3.8 models: the standard context management and transcript behavior, plus a per-conversation reasoning-effort control (low/medium/xhigh) the model honors — the chat's controls row lets you pick how hard it thinks, and the model's provider API maps the choice to its thinking budget (or drops it when the API has no such control).",
		ReasoningInTranscript:     true,
		SummaryFraming:            generalFraming,
		CompactionTriggerFraction: 0.8,
		KeepLastTurns:             5,
		CharsPerToken:             4,
		ReasoningEfforts:          []string{"low", "medium", "xhigh"},
	},
}

// Get resolves a profile name (surrounding whitespace tolerated — a config
// value with a stray space is not a typo). An unknown name is an error —
// the config layer turns it into a loud config error, and the agent's
// defensive fallback (to the default) is unreachable for a validated config.
func Get(name string) (Profile, error) {
	name = strings.TrimSpace(name)
	p, ok := registry[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown model profile %q (known profiles: %s)", name, strings.Join(Names(), ", "))
	}
	return p, nil
}

// Default returns the profile a model without a valid profile gets.
func Default() Profile { return registry[General] }

// Names lists the registry's profile names in deterministic (sorted) order.
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// List returns the registry (name + description) for the model forms'
// profile selects.
func List() []Profile {
	out := make([]Profile, 0, len(registry))
	for _, name := range Names() {
		out = append(out, registry[name])
	}
	return out
}

// ReasoningEffortFor returns the conversation's stored reasoning-effort
// choice when this profile supports it (the value the request builder
// sends), and "" otherwise: the model takes no explicit effort, or the
// stored choice is not one of the profile's levels (stale after a profile
// reassignment — the provider's own default then applies; an invalid value
// is never put on the wire).
func (p Profile) ReasoningEffortFor(stored string) string {
	stored = strings.TrimSpace(stored)
	for _, e := range p.ReasoningEfforts {
		if e == stored {
			return stored
		}
	}
	return ""
}
