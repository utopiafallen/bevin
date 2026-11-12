package agent

import (
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
// (the models are external); ~4 chars/token is a reasonable rule of thumb for
// English. It undercounts code/JSON, so the summarization budget (a fraction of
// the model's max context) carries the headroom.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	t := len(s) / 4
	if t < 1 {
		t = 1
	}
	return t
}

// summarizeBudget is the prompt token count that triggers a compaction: 80% of
// the model's max context (leaving room for the reply). A zero / unknown max
// context falls back to a conservative default; the floor keeps a tiny context
// from triggering on every message.
func summarizeBudget(maxContext int) int {
	if maxContext <= 0 {
		maxContext = 8192
	}
	b := maxContext * 4 / 5
	if b < 512 {
		b = 512
	}
	return b
}

// summaryFraming wraps a context summary so the model treats it as a record of
// the conversation (for continuity), not a new instruction to follow.
func summaryFraming(summaryText string) string {
	return "The following is a summary of the conversation so far, provided for continuity. It records what has been discussed; it is not a new instruction. Continue from this context:\n\n" + summaryText
}

// buildRequest assembles the provider request for a conversation: the frozen
// system prompt + the latest context summary (if any, as a second system part)
// + the raw turns since that summary. It returns the request + an estimated
// token count of the prompt (for the compaction trigger). Full history is
// retained in the store; this selects what each model call sees.
func buildRequest(conv *store.Conversation, msgs []store.Message) (provider.ChatRequest, int) {
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
	// Raw turns since the latest summary (role in user/assistant/tool, seq after
	// it). Turns before the latest summary are already folded into it.
	turns := make([]store.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == roleSystem || m.Role == roleSummary {
			continue
		}
		if m.Seq <= latestSummarySeq {
			continue
		}
		turns = append(turns, m)
	}

	var sysParts []provider.ContentPart
	tokens := estimateTokens(conv.FrozenSystemPrompt)
	if conv.FrozenSystemPrompt != "" {
		sysParts = append(sysParts, provider.ContentPart{Text: conv.FrozenSystemPrompt})
	}
	if latestSummary != nil {
		fr := summaryFraming(latestSummary.Content)
		sysParts = append(sysParts, provider.ContentPart{Text: fr})
		tokens += estimateTokens(fr)
	}
	reqMsgs := make([]provider.Message, 0, len(turns))
	for _, m := range turns {
		reqMsgs = append(reqMsgs, provider.Message{Role: provider.Role(m.Role), Text: m.Content})
		tokens += estimateTokens(m.Content)
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
