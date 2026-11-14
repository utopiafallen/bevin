// Package prompt holds the text bevin freezes into conversations. The base
// system prompt is frozen at conversation creation (stable behavior: later
// profile/input changes never mutate it). When the base text itself changes,
// existing conversations keep their frozen text — by design there is NO
// re-freeze migration path (no live data worth migrating, user directive);
// only new conversations get the new text.
package prompt

// Base is the frozen system prompt for new conversations.
//
// The transcript guidance matters: the model receives every message
// prefixed with a bracketed UTC timestamp. Without explicit framing a
// reasoning model re-derives the transcript structure (which lines are
// markers, which are content) instead of answering. Earlier assistant
// replies that had reasoning replay that reasoning in the API's native
// field (OAI wire: reasoning_content — see agent.assistantFacing /
// agent.reasoningInTranscript), NOT in the message text, so the prompt has
// nothing to say about thinking blocks: the model simply sees its own
// earlier thinking in the channel it produced it in.
const Base = `You are Bevin, a personal assistant. Be direct and concise.

The messages you receive are the conversation so far, in chronological order. Each message is prefixed with its UTC timestamp in brackets, for example [2026-11-13T23:23:55Z]. Your user's messages have role "user"; your earlier replies have role "assistant".

The bracketed timestamps are transcript markers only, not part of the message content. Never include one in your reply.`
