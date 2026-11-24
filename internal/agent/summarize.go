package agent

import (
	"context"
	"strings"

	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// summarizationSystemPrompt is the system prompt for the compaction call — a
// meta-call to the conversation's active model that produces the new context
// summary. It is separate from the conversation's frozen system prompt.
const summarizationSystemPrompt = `You are producing a context summary of a conversation so it can be continued. Write a concise but complete summary that preserves the key facts, decisions, tasks in flight, constraints, and open threads the user has. Fold in the previous summary (if provided) AND the new turns. Output ONLY the summary text — no preamble, no questions, no commentary.`

// summarize calls the conversation's active model to produce a new context
// summary that folds in the previous summary (if any) + the raw messages
// since it. The result is the content of the next role=summary message
// (chained: each new summary folds in the previous, so the latest alone
// carries the history).
// The summarizer sees the turns in the same model-facing form as the chat
// request builder (user turns stamped with their timestamp, assistant turns
// plain + stored reasoning replayed in the API's native field when the
// profile sends reasoning back). effort is the conversation's stored
// reasoning-effort choice, already profile-validated ("" = none) — the
// internal call runs under the same "how hard to think" setting as the chat
// turns.
//
// The request deliberately carries NO ConversationID: this is a meta-call
// with its OWN system prompt, so its prompt is not a prefix of the
// conversation's prompt — a KV cache proxy keyed on the conversation id
// would either load a mismatched cache or save this short-lived prompt's
// KV over the conversation's reusable one.
func (a *Agent) summarize(ctx context.Context, p provider.Provider, modelID, prevSummary, effort string, prof profile.Profile, turns []store.Message, tcMap map[int64][]store.ToolCall) (string, error) {
	msgs := make([]provider.Message, 0, len(turns)+len(tcMap)+1)
	if prevSummary != "" {
		msgs = append(msgs, provider.Message{Role: provider.RoleUser, Text: "Previous summary (fold this in):\n" + prevSummary})
	}
	// The SAME wire rendering as the chat request (wireTurnMessages): a
	// tool-using turn must reach the summarizer with its tool calls AND
	// results paired — the summarizer needs to know what the tools returned
	// to fold the turn in accurately, and an unpaired tool_calls message is
	// wire-illegal on the same grounds as in the chat request.
	msgs = append(msgs, wireTurnMessages(turns, tcMap, prof.ReasoningInTranscript)...)
	res, err := p.Chat(ctx, modelID, provider.ChatRequest{
		System:          []provider.ContentPart{{Text: summarizationSystemPrompt}},
		Messages:        msgs,
		ReasoningEffort: effort,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Text), nil
}

// compactIfNeeded checks whether the conversation's prompt (frozen system
// prompt + latest summary + raw messages since it) exceeds the
// summarization budget, and if so produces + stores a new chained summary.
// The summary folds in the previous summary + the older raw messages —
// EXCEPT the last KeepLastMessages messages (the profile's), which are
// preserved verbatim so the chat resumes exactly where it left off. Full
// history is retained in the store; the summary only changes what the
// request builder sends. It is a no-op when the prompt is under budget or
// there is no older message to fold (only the last KeepLastMessages
// remain).
//
// prof is the conversation's active model's model profile — the budget
// fraction, the verbatim tail, and the reasoning replay in the summarization
// call all come from it.
//
// actualTokens, when > 0, is the API-reported prompt size for the most recent
// call (used in preference to the char/4 estimate) — the provider's usage
// stats when the backend returned them. Pass 0 to fall back to the estimate
// (e.g. before the first call, or a backend that omits usage).
func (a *Agent) compactIfNeeded(ctx context.Context, us *store.UserStore, conv *store.Conversation, p provider.Provider, actualTokens int, prof profile.Profile) error {
	hist, err := us.ListMessages(conv.ID)
	if err != nil {
		return err
	}
	tcMap, err := toolCallsForMessages(us, hist)
	if err != nil {
		return err
	}
	tokens := actualTokens
	if tokens <= 0 {
		_, tokens = buildRequest(conv, hist, prof, tcMap)
	}
	if tokens <= summarizeBudget(a.modelMaxContext(conv.ActiveModelID), prof) {
		return nil
	}
	prevSummary, messages := latestSummaryAndMessages(hist)
	if len(messages) <= prof.KeepLastMessages {
		// Nothing older to fold — the last KeepLastMessages messages are
		// always kept verbatim (the chat resumes exactly where it left
		// off). Leave it.
		return nil
	}
	// Summarize the older messages; the last KeepLastMessages stay as raw
	// messages so the chat resumes exactly where it left off. The
	// summarization call is a provider call too — the wire request carries
	// the provider-side model name (the internal ID would be rejected for
	// slug-mangled upstream names).
	toSummarize := messages[:len(messages)-prof.KeepLastMessages]
	// The tool-call rows of the FOLDED assistant messages ride along with
	// their messages (pair-safe: the builder derives both sides from the
	// same row set).
	summary, err := a.summarize(ctx, p, a.modelAPIName(conv.ActiveModelID), prevSummary, prof.ReasoningEffortFor(conv.ReasoningEffort), prof, toSummarize, toolCallsSubset(tcMap, toSummarize))
	if err != nil {
		return err
	}
	if summary == "" {
		return nil
	}
	modelID := conv.ActiveModelID
	_, err = us.AppendMessage(conv.ID, roleSummary, summary, "", "", &modelID)
	return err
}

// modelMaxContext returns the model's max context in tokens (0 if unknown).
func (a *Agent) modelMaxContext(modelID string) int {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, m := range a.cfg.Models {
		if m.ID == modelID {
			return m.MaxContext
		}
	}
	return 0
}

// toolCallsForMessages fetches the P7 tool-call rows for every assistant
// message in hist (one query; empty map when the conversation has no tool
// activity).
func toolCallsForMessages(us *store.UserStore, hist []store.Message) (map[int64][]store.ToolCall, error) {
	ids := make([]int64, 0)
	for _, m := range hist {
		if m.Role == roleAssistant {
			ids = append(ids, m.ID)
		}
	}
	return us.ToolCallsForMessages(ids)
}

// toolCallsSubset narrows a tool-call map to the messages in msgs (the
// summarization call folds a PREFIX of the turns — only those assistant
// messages' rows are needed).
func toolCallsSubset(tcMap map[int64][]store.ToolCall, msgs []store.Message) map[int64][]store.ToolCall {
	if len(tcMap) == 0 {
		return tcMap
	}
	out := map[int64][]store.ToolCall{}
	for _, m := range msgs {
		if tcs, ok := tcMap[m.ID]; ok {
			out[m.ID] = tcs
		}
	}
	return out
}
