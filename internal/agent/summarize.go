package agent

import (
	"context"
	"strings"

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
func (a *Agent) summarize(ctx context.Context, p provider.Provider, modelID, prevSummary string, turns []store.Message) (string, error) {
	msgs := make([]provider.Message, 0, len(turns)+1)
	if prevSummary != "" {
		msgs = append(msgs, provider.Message{Role: provider.RoleUser, Text: "Previous summary (fold this in):\n" + prevSummary})
	}
	for _, m := range turns {
		msgs = append(msgs, provider.Message{Role: provider.Role(m.Role), Text: m.Content})
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
// budget, and if so produces + stores a new chained summary (folding in the
// previous summary + the raw turns since it). Full history is retained in the
// store; the summary only changes what the request builder sends. It is a no-op
// when the prompt is under budget or there is no older turn to fold.
func (a *Agent) compactIfNeeded(ctx context.Context, us *store.UserStore, conv *store.Conversation, p provider.Provider) error {
	hist, err := us.ListMessages(conv.ID)
	if err != nil {
		return err
	}
	_, tokens := buildRequest(conv, hist)
	if tokens <= summarizeBudget(a.modelMaxContext(conv.ActiveModelID)) {
		return nil
	}
	prevSummary, turns := latestSummaryAndTurns(hist)
	if len(turns) == 0 {
		// Over budget only because of the system prompt + summary (nothing to
		// fold). Leave it — there is no older turn to compact.
		return nil
	}
	summary, err := a.summarize(ctx, p, conv.ActiveModelID, prevSummary, turns)
	if err != nil {
		return err
	}
	if summary == "" {
		return nil
	}
	modelID := conv.ActiveModelID
	_, err = us.AppendMessage(conv.ID, roleSummary, summary, &modelID, nil)
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
