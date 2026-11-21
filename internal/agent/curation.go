// The recurring memory-curation task (the design log's CURATION TASK
// section): growth-triggered, closed-range, the user profile's sole writer.
//
// Mid-turn remember is the CAPTURE path (opportunistic, in-context); this
// task is the MAINTENANCE path — it enforces the prompt's capture standard
// on what has accumulated, and distills the curated store into the user
// profile as its final step. No clock: it spawns only when the store grows
// by a meaty threshold past the last completed curation, and the per-model
// slot pools + FIFO handle WHEN it runs (idle-slot scheduling).
package agent

import (
	"context"
	"fmt"
	"strings"
)

const (
	// CurationTaskTitle: the curation task's title (the owned conversation
	// is "task: " + this; the wire filter grants set_profile to exactly
	// this task's conversation).
	CurationTaskTitle = "memory curation"
	// curationGrowthThreshold: the meaty growth that earns a curation pass
	// (the user's ~10 — a trickle of memories isn't worth a model turn).
	curationGrowthThreshold = 10
)

// maybeSpawnCuration is the growth trigger, called from the remember
// handler after a memory persists (the only live growth path — the learning
// pass is disabled). When the store has grown by >= curationGrowthThreshold
// rows past the last completed curation's end, it spawns a curation task
// over the CLOSED [watermark+1, maxID] range. Skipped when:
//   - the growth hasn't reached the threshold,
//   - a curation task is already queued/running (one at a time; a BLOCKED
//     one is terminal, so the next crossing spawns a fresh attempt),
//   - the remember came from a curation conversation (the curator's own
//     supersede-creates are curated output, not user growth — they must not
//     re-trigger the very pass they belong to).
//
// Never errors into the caller: a spawn failure just means the next growth
// event retries.
func (a *Agent) maybeSpawnCuration(ctx context.Context, handle string) {
	us, err := a.userStore(handle)
	if err != nil {
		return
	}
	// The curator's own writes don't count as growth (see the doc).
	if conv := conversationFromContext(ctx); conv != "" {
		c, err := us.GetConversation(conv)
		if err == nil && c.OwnerTaskID != nil {
			if t, err := us.GetTask(*c.OwnerTaskID); err == nil && t.Title == CurationTaskTitle {
				return
			}
		}
	}
	wm, err := us.CurationWatermark(CurationTaskTitle)
	if err != nil {
		return
	}
	end, err := us.MaxMemoryID()
	if err != nil {
		return
	}
	if end-wm < curationGrowthThreshold {
		return
	}
	busy, err := us.HasActiveCuration(CurationTaskTitle)
	if err != nil || busy {
		return
	}
	_, _ = a.SpawnTask(ctx, handle, TaskSpec{
		Title:       CurationTaskTitle,
		Description: curationDescription(wm+1, end),
		MemStart:    wm + 1,
		MemEnd:      end,
	})
}

// curationDescription is the curation task's work framing (the v1 rule: no
// per-task prompt override — the framing lives in the task description; the
// capture policy itself is the one sentence in the system prompt).
func curationDescription(start, end int64) string {
	var b strings.Builder
	b.WriteString("You are curating the user's memory store — reviewing what the model has saved, not capturing anything new.\n\n")
	fmt.Fprintf(&b, "Review the memories with ids from %d to %d (INCLUSIVE) — nothing outside that range. The store may grow while you work; memories outside the range belong to a later pass, so do not chase them.\n\n", start, end)
	b.WriteString("For each memory, judge from the STATEMENT ALONE (you have no access to the conversations it came from): does it pass the memory policy in your system prompt — a fact that deepens your understanding of the user, or durable world state likely to hold for the short term? Trivia, one-off task context, stale or contradicted facts: forget them (the store supersession handles it — nothing is ever deleted). Two memories that are really one fact: consolidate them by supersede. Do NOT add new memories — capturing is the mid-turn remember's job, not this pass's.\n\n")
	b.WriteString("As your FINAL step, update the user profile: the distilled, durable key: value notes that are bevin's understanding of the user (a handful of small entries — preferences, people, projects, stable facts — not a transcript). Then call advance_task to report done (or blocked, with a note), and make your final reply a concise summary: what you pruned, merged and kept, and what the profile now says.")
	return b.String()
}
