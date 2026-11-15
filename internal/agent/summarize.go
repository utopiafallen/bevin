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
// summary that folds in the previous summary (if any) + the raw turns since
// it. The result is the content of the next role=summary message (chained: each
// new summary folds in the previous, so the latest alone carries the history).
// The summarizer sees the turns in the same model-facing form as the chat
// request builder (user turns stamped with their timestamp, assistant turns
// plain + stored reasoning replayed in the API's native field when the
// profile sends reasoning back).
func (a *Agent) summarize(ctx context.Context, p provider.Provider, modelID, prevSummary string, prof profile.Profile, turns []store.Message) (string, error) {
	msgs := make([]provider.Message, 0, len(turns)+1)
	if prevSummary != "" {
		msgs = append(msgs, provider.Message{Role: provider.RoleUser, Text: "Previous summary (fold this in):\n" + prevSummary})
	}
	for _, m := range turns {
		msgs = append(msgs, assistantFacing(m, prof.ReasoningInTranscript))
	}
	res, err := p.Chat(ctx, modelID, provider.ChatRequest{
		System:   []provider.ContentPart{{Text: summarizationSystemPrompt}},
		Messages: msgs,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Text), nil
}

// compactIfNeeded checks whether the conversation's prompt (frozen system
// prompt + latest summary + raw turns since it) exceeds the summarization
// budget, and if so produces + stores a new chained summary. The summary
// folds in the previous summary + the older raw turns — EXCEPT the last
// KeepLastTurns turns (the profile's), which are preserved verbatim so the
// chat resumes exactly where it left off. Full history is retained in the
// store; the summary only changes what the request builder sends. It is a
// no-op when the prompt is under budget or there is no older turn to fold
// (only the last KeepLastTurns remain).
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
	tokens := actualTokens
	if tokens <= 0 {
		_, tokens = buildRequest(conv, hist, prof)
	}
	if tokens <= summarizeBudget(a.modelMaxContext(conv.ActiveModelID), prof) {
		return nil
	}
	prevSummary, turns := latestSummaryAndTurns(hist)
	if len(turns) <= prof.KeepLastTurns {
		// Nothing older to fold — the last KeepLastTurns turns are always
		// kept verbatim (the chat resumes exactly where it left off). Leave
		// it.
		return nil
	}
	// Summarize the older turns; the last KeepLastTurns stay as raw turns so
	// the chat resumes exactly where it left off. The summarization call is a
	// provider call too — the wire request carries the provider-side model
	// name (the internal ID would be rejected for slug-mangled upstream
	// names).
	toSummarize := turns[:len(turns)-prof.KeepLastTurns]
	summary, err := a.summarize(ctx, p, a.modelAPIName(conv.ActiveModelID), prevSummary, prof, toSummarize)
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
