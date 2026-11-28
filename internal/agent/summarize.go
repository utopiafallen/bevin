package agent

import (
	"context"
	"log"
	"strings"

	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/prompt"
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
func (a *Agent) summarize(ctx context.Context, p provider.Provider, modelID, prevSummary, effort string, prof profile.Profile, turns []store.Message, tcMap map[int64][]store.ToolCall, emit func(provider.Chunk) error) (string, error) {
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
	// Stream (not Chat): the summarizer may think before it writes the
	// summary, and its reasoning is forwarded to the viewer (emit) as the
	// compaction row's thinking block — the same behavior as a regular
	// message's thinking. The aggregated Result.Text is the summary. emit is
	// nil for non-streaming turns (there is no viewer for the indicator).
	res, err := p.Stream(ctx, modelID, provider.ChatRequest{
		System:          []provider.ContentPart{{Text: summarizationSystemPrompt}},
		Messages:        msgs,
		ReasoningEffort: effort,
	}, func(c provider.Chunk) error {
		if emit != nil && c.ReasoningDelta != "" {
			// A failed write (the viewer left) must never abort the
			// compaction — it completes and is persisted regardless.
			_ = emit(provider.Chunk{CompactionPhase: "reasoning", ReasoningDelta: c.ReasoningDelta})
		}
		return nil
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
// Every compaction event ALSO re-bakes the profile section of the
// conversation's frozen system prompt with the user's current profile KVs
// (design log 2026-11-25 "Profile in the system prompt"): the compaction
// already breaks the prompt prefix, so the refresh is cache-neutral against
// a content-matched KV proxy, and it lands at the moment the prompt is at
// its smallest. The core base text is never rewritten — only the trailing
// section — and the in-memory conv row is synced so the rebuilt request of
// the same turn carries the fresh prompt.
//
// prof is the conversation's active model's model profile — the budget
// fraction, the verbatim tail, and the reasoning replay in the summarization
// call all come from it.
//
// actualTokens, when > 0, is the API-reported prompt size for the most recent
// call (used in preference to the char/4 estimate) — the provider's usage
// stats when the backend returned them. Pass 0 to fall back to the estimate
// (e.g. before the first call, or a backend that omits usage).
//
// force skips the budget gate: the prompt is already over the model's
// context (the model call returned a context-overflow error) and the older
// turns must be folded no matter what the estimate or usage say. The
// "nothing older to fold" check still applies.
//
// emit, when non-nil, is the turn's SSE writer: it receives a live
// "Compacting context..." chunk (CompactionPhase "start") before the
// summarizer runs, then a "done" chunk (CompactionPhase "done", carrying the
// from/to token counts) once the summary is stored — so the UI shows an
// animated indicator that settles into a persistent marker. nil for
// non-streaming turns (there is no viewer for the indicator).
func (a *Agent) compactIfNeeded(ctx context.Context, us *store.UserStore, handle string, conv *store.Conversation, p provider.Provider, actualTokens int, prof profile.Profile, force bool, emit func(provider.Chunk) error) error {
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
		// No API usage — fall back to the buildRequest char/4 estimate
		// (the live request carries the skill block — count it).
		_, tokens = buildRequest(conv, hist, prof, tcMap, a.activeSkillBlock(us, handle, conv.ID))
	}
	if !force && tokens <= summarizeBudget(a.modelMaxContext(conv.ActiveModelID), prof) {
		// Under budget (and not a forced overflow backstop) — nothing to do.
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
	// The UI sees the compaction as a live event: an animated "Compacting…"
	// row while the summarizer runs, then a persistent "Context compacted
	// from X to Y tokens" marker once it is stored. (emit is nil for
	// non-streaming turns — there is no viewer for the indicator.)
	from := tokens
	if emit != nil {
		_ = emit(provider.Chunk{CompactionPhase: "start"})
	}
	summary, err := a.summarize(ctx, p, a.modelAPIName(conv.ActiveModelID), prevSummary, prof.ReasoningEffortFor(conv.ReasoningEffort), prof, toSummarize, toolCallsSubset(tcMap, toSummarize), emit)
	if err != nil {
		return err
	}
	if summary == "" {
		return nil
	}
	modelID := conv.ActiveModelID
	msg, err := us.AppendMessage(conv.ID, roleSummary, summary, "", "", &modelID)
	if err != nil {
		return err
	}
	// Re-bake the profile section of the frozen system prompt (see the
	// func doc): current profile KVs, core bytes untouched. The store is
	// written first, then the in-memory row mirrors it — the turn's rebuilt
	// request (and the marker's "to" estimate below) must carry the fresh
	// prompt, not the stale one this conv was created with.
	ptxt, err := us.ProfileText()
	if err != nil {
		return err
	}
	if fresh := prompt.SystemPrompt(ptxt); fresh != conv.FrozenSystemPrompt {
		if err := us.SetFrozenSystemPrompt(conv.ID, fresh); err != nil {
			return err
		}
		conv.FrozenSystemPrompt = fresh
	}
	// Re-estimate the post-compaction prompt size (frozen system prompt +
	// the new summary + the verbatim tail) so the marker can show "from X to
	// Y". Best-effort: a failure falls back to 0 (the marker then shows the
	// "from" size only).
	to := 0
	if newHist, e := us.ListMessages(conv.ID); e == nil {
		if newTcMap, e := toolCallsForMessages(us, newHist); e == nil {
			_, to = buildRequest(conv, newHist, prof, newTcMap, a.activeSkillBlock(us, handle, conv.ID))
		}
	}
	// Stamp the persistent marker on the summary row (survives a reload —
	// the chat history shows every compaction event in flow order).
	if e := us.SetMessageCompaction(msg.ID, from, to); e != nil {
		log.Printf("compaction marker: %v", e)
	}
	if emit != nil {
		_ = emit(provider.Chunk{CompactionPhase: "done", CompactedFrom: from, CompactedTo: to, CompactedSummary: summary})
	}
	return nil
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
