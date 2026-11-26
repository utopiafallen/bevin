// Package prompt holds the text bevin freezes into conversations. The base
// system prompt (Base) is frozen at conversation creation and never changes
// for the life of the conversation — when the base text itself changes,
// existing conversations keep their frozen text (there is NO re-freeze
// migration path; only new conversations get the new text). The one part
// that refreshes is the user-profile section: it is baked in at creation
// from the user's current profile and re-baked at every compaction event
// (design log 2026-11-25 "Profile in the system prompt") — the core bytes
// are untouched, so the refresh is a trailing-section swap that a
// content-matched KV cache proxy rides for free (the compaction already
// breaks the prefix).
package prompt

import "strings"

// ProfileSectionHeader frames the user-profile section of the system prompt:
// the profile is CURRENT STATE about the user — on any conflict with older
// profile-derived content (in the chained summary or the transcript), the
// profile wins.
const ProfileSectionHeader = "About the user (current):"

// Base is the frozen core of the system prompt for conversations.
//
// The transcript guidance matters: the model receives the USER's messages
// prefixed with a bracketed UTC timestamp (the model's own earlier replies
// are NOT stamped — the model knows its own reply order, and the stamped
// replies read as transcript noise that can be echoed or misread as content).
// Without explicit framing a reasoning model re-derives the transcript
// structure (which lines are markers, which are content) instead of
// answering. Earlier assistant replies that had reasoning replay that
// reasoning in the API's native field (OAI wire: reasoning_content — see
// agent.assistantFacing / the profile's ReasoningInTranscript), NOT in the
// message text, so the prompt has nothing to say about thinking blocks: the
// model simply sees its own earlier thinking in the channel it produced it in.
//
// The memory policy lives here, not in the remember/recall descriptions
// (user directive: descriptions stay functional, policy is the prompt's
// job): it is the capture standard for mid-turn remembering — selective,
// user-understanding or short-term-durable world state, never a transcript.
// The recurring memory-curation task (design log) enforces the same standard
// on what has accumulated.
const Base = `You are Bevin, a personal assistant. Be direct and concise.

The messages you receive are the conversation so far, in chronological order. Your user's messages are prefixed with their UTC timestamp in brackets, for example [2026-11-13T23:23:55Z]; your earlier replies have no such prefix. The bracketed timestamps are transcript markers only, not part of the message content. Never include one in your reply.

Use remember selectively: save only facts that deepen your understanding of the user (preferences, people, projects, commitments) or that are durable world state likely to hold for the short term — a handful of such memories beats a running transcript, and one-off task context never earns a memory.`

// SystemPrompt assembles a conversation's frozen system prompt: the core
// base text + the user-profile section (absent when profileText is empty —
// a profile-less conversation's prompt is exactly Base). The core is a
// constant, so re-baking the profile never rewrites a core byte: old and
// new prompts share the whole core as a byte prefix (see
// prompt.ProfileSectionHeader for the section's framing).
func SystemPrompt(profileText string) string {
	profileText = strings.TrimSpace(profileText)
	if profileText == "" {
		return Base
	}
	return Base + "\n\n" + ProfileSectionHeader + "\n" + profileText
}
