package agent

import (
	"fmt"
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
	// roleCheckin is the no-progress check-in: a PERSISTENT, neutral,
	// tail-appended harness instruction that REQUIRES the model to call
	// report_progress (the design log's no-progress section). It is a stored
	// transcript row (so the model sees the trigger when it responds — a
	// one-shot removed check-in would leave the model's % declaration dangling
	// with no visible cause) and is IN the model's request (buildRequest does
	// not exclude it — only roleSystem/roleSummary are excluded). It maps to a
	// provider USER message on the wire (universally supported, and it appears
	// at the tail in transcript order — prefix-safe: the prior prompt stays a
	// prefix, unlike a system-prompt CHANGE). It is filtered from the UI (like
	// summary/tool rows) — it is a harness instruction, not user content.
	roleCheckin = "checkin"
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

// withTimestamp prepends the message's UTC timestamp (RFC3339) to its content.
// Only USER messages are stamped (assistantFacing gates it): the model knows
// when the user spoke; its own earlier replies carry no prefix. The stored
// content is untouched — the timestamp is only added to what the request
// sends. A message with no CreatedAt (tests) is returned as-is.
func withTimestamp(m store.Message) string {
	marker := timestampMarker(m)
	if marker == "" {
		return m.Content
	}
	return "[" + marker + "] " + m.Content
}

// assistantFacing renders one stored message as the neutral provider message
// the model receives. Only USER messages carry the UTC timestamp marker —
// the model's own earlier replies are sent with their plain stored content
// (stamped replies confused the model; the UI still shows every message's
// timestamp from the stored created_at, so nothing is lost for the user).
// For an assistant message that carries a stored reasoning trace (when
// includeReasoning is set, see the profile's ReasoningInTranscript) — the
// trace goes in the message's Reasoning field, which the provider encodes in
// the API's native field (OAI wire: reasoning_content; the Anthropic encoder
// deliberately ignores it — see provider.decodeAntResponse). Stored content
// is untouched. A trace is empty when the model didn't think (or thinking
// was disabled), in which case the field is simply absent.
func assistantFacing(m store.Message, includeReasoning bool) provider.Message {
	text := m.Content
	if m.Role == roleUser {
		text = withTimestamp(m)
	}
	role := provider.Role(m.Role)
	if m.Role == roleCheckin {
		// The no-progress check-in is a harness instruction: sent as a USER
		// message on the wire (the only role every provider supports mid-
		// transcript, and it lands at the tail in transcript order —
		// prefix-safe). It carries no user timestamp (it is not the user
		// speaking — withTimestamp only stamps roleUser, and this is
		// roleCheckin).
		role = provider.RoleUser
	}
	pm := provider.Message{Role: role, Text: text}
	if m.Role == roleAssistant && includeReasoning && m.Reasoning != "" {
		pm.Reasoning = m.Reasoning
		pm.ReasoningSignature = m.ReasoningSignature
	}
	return pm
}

// leadingTranscriptTimestamp matches the transcript marker that withTimestamp
// prepends to USER messages. Models occasionally echo a marker they saw in a
// user message into the start of their own reply — the system prompt forbids
// it, but during a long reasoning phase the rule can be lost, so the reply is
// also sanitized deterministically at the persistence boundary.
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

// wireToolCallID derives the provider-side pairing id for a P7 tool-call row:
// the assistant message's tool_calls entry and the synthesized role=tool
// result message share exactly this id on the wire. It is derived from the
// row's id (not stored) so the P7 row stays the single immutable record and
// pairing can never drift from it.
func wireToolCallID(tc store.ToolCall) string {
	return fmt.Sprintf("tc%d", tc.ID)
}

// toolCallComplete reports whether a P7 row has an outcome to send: a row
// with result="" and success=false is IN-FLIGHT (the process died between
// the model's request and the execution) — sending the assistant's tool call
// without its result is wire-illegal, so in-flight rows are excluded from
// the wire (from BOTH the assistant's tool_calls list and the synthesized
// results — the pairing is derived from the same row set, so they can never
// orphan each other).
func toolCallComplete(tc store.ToolCall) bool {
	return tc.Result != "" || tc.Success
}

// wireTurnMessages renders the raw turns (already selected by the builder)
// into the provider wire messages. An assistant message whose P7 tool calls
// are complete is sent WITH its tool_calls list, immediately followed by one
// role=tool message per call (the pairing id derived from the P7 row). This
// is the single place the tool transcript is turned into wire messages —
// the chat request and the summarization call both go through it, so a
// tool-using turn is rendered identically in both.
func wireTurnMessages(turns []store.Message, tcMap map[int64][]store.ToolCall, includeReasoning bool) []provider.Message {
	out := make([]provider.Message, 0, len(turns)+len(tcMap))
	for _, m := range turns {
		pm := assistantFacing(m, includeReasoning)
		var complete []store.ToolCall
		if m.Role == roleAssistant {
			for _, tc := range tcMap[m.ID] {
				if toolCallComplete(tc) {
					complete = append(complete, tc)
					pm.ToolCalls = append(pm.ToolCalls, provider.ToolCall{ID: wireToolCallID(tc), Name: tc.Tool, Arguments: tc.Args})
				}
			}
		}
		out = append(out, pm)
		for _, tc := range complete {
			out = append(out, provider.Message{Role: provider.RoleTool, Text: tc.Result, ToolCallID: wireToolCallID(tc)})
		}
	}
	return out
}

// buildRequest assembles the provider request for a conversation: the frozen
// system prompt + the latest context summary (if any, as a second system
// part) + the raw turns to send. When there's a summary, it covers the older
// turns and the last KeepLastTurns messages (the profile's) are sent verbatim
// (the chat resumes exactly where it left off); when there's no summary, all
// the raw turns are sent (up to the compaction budget). It returns the
// request + an estimated token count of the prompt (for the compaction
// trigger). Full history is retained in the store; this selects what each
// model call sees. tcMap carries the P7 tool-call rows for the turns'
// assistant messages (see wireTurnMessages; an empty map = a conversation
// with no tool activity — the request is identical to the pre-tools shape).
//
// prof is the conversation's active model's model profile — the request
// builder's model-facing decisions (what the model gets back of its own
// reasoning, how the summary is framed, the estimate factor, the verbatim
// tail) all come from it, so a model-specific profile changes the request
// without the harness special-casing the model.
func buildRequest(conv *store.Conversation, msgs []store.Message, prof profile.Profile, tcMap map[int64][]store.ToolCall) (provider.ChatRequest, int) {
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
	// Whether this model receives earlier assistant reasoning traces — the
	// profile's decision (general: yes — the model sees its own earlier
	// reasoning for continuity).
	includeReasoning := prof.ReasoningInTranscript
	reqMsgs := wireTurnMessages(turns, tcMap, includeReasoning)
	for _, pm := range reqMsgs {
		tokens += estimateTokens(pm.Text+pm.Reasoning, prof)
	}
	req := provider.ChatRequest{System: sysParts, Messages: reqMsgs, ConversationID: conv.ID}
	// The conversation's reasoning-effort choice, when the active model's
	// profile supports it (see profile.ReasoningEffortFor): an unsupported
	// or stale choice is dropped — the provider's own default then applies,
	// and no value the model's API could reject is ever sent.
	if e := prof.ReasoningEffortFor(conv.ReasoningEffort); e != "" {
		req.ReasoningEffort = e
	}
	return req, tokens
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
