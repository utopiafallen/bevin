package agent

import (
	"regexp"
	"time"

	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// Bevin-level message roles (stored as Message.Role in the per-user store). The
// provider roles are a subset (system/user/assistant/tool); the summary role is
// bevin-specific — the request builder folds it into the system context.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
	roleSummary   = "summary"
)

// estimateTokens is a rough prompt-size estimate. There is no local tokenizer
// (the models are external); the profile's CharsPerToken factor (general:
// ~4 chars/token, a reasonable rule of thumb for English) undercounts
// code/JSON, so the compaction budget (a fraction of the model's max context)
// carries the headroom.
func estimateTokens(s string, prof profile.Profile) int {
	if s == "" {
		return 0
	}
	t := len(s) / prof.CharsPerToken
	if t < 1 {
		t = 1
	}
	return t
}

// budgetFloor is the minimum compaction budget: below it a prompt would
// trigger a compaction on every message (a degenerate context config). A
// universal guard, not model behavior — profiles set the FRACTION, the floor
// is the harness refusing to run in a broken state.
const budgetFloor = 512

// summarizeBudget is the prompt token count that triggers a compaction: the
// profile's trigger fraction of the model's max context (general: 80%,
// leaving room for the reply). A zero / unknown max context falls back to a
// conservative default; the floor keeps a tiny context from triggering on
// every message.
func summarizeBudget(maxContext int, prof profile.Profile) int {
	if maxContext <= 0 {
		maxContext = 8192
	}
	b := int(float64(maxContext) * prof.CompactionTriggerFraction)
	if b < budgetFloor {
		b = budgetFloor
	}
	return b
}

// timestampMarker returns the bracket-ready UTC timestamp for a message
// (RFC3339; the raw CreatedAt when it doesn't parse). Empty when the message
// has no timestamp (tests).
func timestampMarker(m store.Message) string {
	if m.CreatedAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
	if err != nil {
		return m.CreatedAt
	}
	return t.UTC().Format(time.RFC3339)
}

// withTimestamp prepends the message's UTC timestamp (RFC3339) to its content,
// so the model knows when each message was sent. The stored content is
// untouched — the timestamp is only added to what the request sends. A message
// with no CreatedAt (tests) is returned as-is.
func withTimestamp(m store.Message) string {
	marker := timestampMarker(m)
	if marker == "" {
		return m.Content
	}
	return "[" + marker + "] " + m.Content
}

// assistantFacing renders one stored message as the neutral provider message
// the model receives: the UTC timestamp marker + the reply in the text, and —
// for an assistant message that carries a stored reasoning trace (when
// includeReasoning is set, see reasoningInTranscript) — the trace in the
// message's Reasoning field, which the provider encodes in the API's native
// field (OAI wire: reasoning_content; the Anthropic encoder deliberately
// ignores it — see provider.decodeAntResponse). Stored content is untouched.
// A trace is empty when the model didn't think (or thinking was disabled), in
// which case the field is simply absent.
func assistantFacing(m store.Message, includeReasoning bool) provider.Message {
	pm := provider.Message{Role: provider.Role(m.Role), Text: withTimestamp(m)}
	if m.Role == roleAssistant && includeReasoning && m.Reasoning != "" {
		pm.Reasoning = m.Reasoning
		pm.ReasoningSignature = m.ReasoningSignature
	}
	return pm
}

// leadingTranscriptTimestamp matches the transcript marker that withTimestamp
// prepends to every message. Models occasionally echo that marker into the
// start of their own reply — the system prompt forbids it, but during a long
// reasoning phase the rule can be lost, so the reply is also sanitized
// deterministically at the persistence boundary.
var leadingTranscriptTimestamp = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\][ 	]?`)

// stripLeadingTranscriptTimestamp removes a transcript timestamp marker from
// the start of a reply if the model echoed one. Only the leading marker in
// the exact transcript format (UTC, seconds, Z) is removed; a timestamp
// anywhere else in the text, or in any other format, is left alone.
func stripLeadingTranscriptTimestamp(s string) string {
	return leadingTranscriptTimestamp.ReplaceAllString(s, "")
}

// (The per-model "does this model receive earlier reasoning traces" decision
// was the free function reasoningInTranscript; it is now the profile's
// ReasoningInTranscript — the model's assigned profile is the harness's ONE
// source of per-model behavior.)

// summaryFraming moved to the model profile (Profile.SummaryFraming): the
// text the model is given around its context summary is harness MODEL behavior
// (a model-specific profile may frame it differently), so the general
// profile owns the standard wording — see internal/profile.

// buildRequest assembles the provider request for a conversation: the frozen
// system prompt + the latest context summary (if any, as a second system part)
// + the raw turns to send. When there's a summary, it covers the older turns
// and the last KeepLastTurns messages (the profile's) are sent verbatim (the
// chat resumes exactly where it left off); when there's no summary, all the
// raw turns are sent (up to the compaction budget). It returns the request +
// an estimated token count of the prompt (for the compaction trigger). Full
// history is retained in the store; this selects what each model call sees.
//
// prof is the conversation's active model's model profile — the request
// builder's model-facing decisions (what the model gets back of its own
// reasoning, how the summary is framed, the estimate factor, the verbatim
// tail) all come from it, so a model-specific profile changes the request
// without the harness special-casing the model.
func buildRequest(conv *store.Conversation, msgs []store.Message, prof profile.Profile) (provider.ChatRequest, int) {
	// Latest summary (role=summary) — the last one in seq order (summaries are
	// chained; the builder uses only the latest, which already folds in the rest).
	latestSummarySeq := -1
	var latestSummary *store.Message
	for i := range msgs {
		if msgs[i].Role == roleSummary {
			latestSummary = &msgs[i]
			latestSummarySeq = msgs[i].Seq
		}
	}
	// Raw turns for the request (role in user/assistant/tool, seq after the
	// latest summary). When the summary is the latest (stored after all the raw
	// turns), there are no raw turns since it, so the last keepLastTurns
	// messages are sent verbatim (the chat resumes exactly where it left off).
	// When there's no summary, all the raw turns are sent.
	turns := make([]store.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == roleSystem || m.Role == roleSummary {
			continue
		}
		if m.Seq > latestSummarySeq {
			turns = append(turns, m)
		}
	}
	if latestSummary != nil && len(turns) == 0 {
		// The summary is the latest (stored after all the raw turns). Send the
		// last keepLastTurns verbatim (the chat resumes exactly where it left
		// off).
		for _, m := range msgs {
			if m.Role == roleSystem || m.Role == roleSummary {
				continue
			}
			turns = append(turns, m)
		}
		if len(turns) > prof.KeepLastTurns {
			turns = turns[len(turns)-prof.KeepLastTurns:]
		}
	}

	var sysParts []provider.ContentPart
	tokens := estimateTokens(conv.FrozenSystemPrompt, prof)
	if conv.FrozenSystemPrompt != "" {
		sysParts = append(sysParts, provider.ContentPart{Text: conv.FrozenSystemPrompt})
	}
	if latestSummary != nil {
		fr := prof.SummaryFraming(latestSummary.Content)
		sysParts = append(sysParts, provider.ContentPart{Text: fr})
		tokens += estimateTokens(fr, prof)
	}
	reqMsgs := make([]provider.Message, 0, len(turns))
	// Whether this model receives earlier assistant reasoning traces — the
	// profile's decision (general: yes — the model sees its own earlier
	// reasoning for continuity).
	includeReasoning := prof.ReasoningInTranscript
	for _, m := range turns {
		// Prepend the message's UTC timestamp (and replay the assistant's
		// stored reasoning, when sent back) — the stored content is untouched.
		pm := assistantFacing(m, includeReasoning)
		reqMsgs = append(reqMsgs, pm)
		tokens += estimateTokens(pm.Text+pm.Reasoning, prof)
	}
	return provider.ChatRequest{System: sysParts, Messages: reqMsgs}, tokens
}

// latestSummaryAndTurns returns the latest summary's content + the raw turns
// since it (role in user/assistant/tool, seq after the summary's seq).
func latestSummaryAndTurns(msgs []store.Message) (prevSummary string, turns []store.Message) {
	latestSummarySeq := -1
	for _, m := range msgs {
		if m.Role == roleSummary {
			latestSummarySeq = m.Seq
			prevSummary = m.Content
		}
	}
	turns = make([]store.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == roleSummary || m.Role == roleSystem {
			continue
		}
		if m.Seq > latestSummarySeq {
			turns = append(turns, m)
		}
	}
	return prevSummary, turns
}
