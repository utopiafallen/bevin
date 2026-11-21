package agent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/utopiafallen/bevin/internal/store"
)

// Turn-start awareness (concurrent-instances design, layers 2+4): the roster
// (who is active, what each sibling conversation is doing), the purpose
// nudge (the sole updater of the rolling purpose), and the delivery of
// queued sibling messages — announced as ONE UI-hidden checkin row (the
// existing check-in pattern: user-role on the wire, filtered from the UI).
//
// The coordination machinery is AWARENESS, not exclusion: no leases, no
// claims, no tracking of what the harness can't see. The in-memory state
// below (in-flight set, last-announced snapshots, nudge counters) is
// deliberately ephemeral — a restart re-announces the full roster, which is
// correct: a cold start knows nothing.
const (
	// purposeNudgeCadence: the purpose nudge fires at most once this many
	// user turns apart per conversation (the design's ~5-10 turn tunable,
	// the sole updater of the rolling purpose — a background whisper, not a
	// per-turn demand).
	purposeNudgeCadence = 7
)

// rosterQuietGrace: a sibling whose last activity is within this window is
// "active" on the roster (presence-only: the conversation list is unbounded
// in theory, so the roster shows who is RUNNING or recently ACTIVE — never
// the long tail of idle conversations). A var, not a const, as a test seam
// (tests shrink it to 0 to age a sibling into idle deterministically).
var rosterQuietGrace = 15 * time.Minute

// rosterState is the in-memory awareness state (all maps keyed per
// conversation; guarded by one mutex — the operations are short and the
// contention is one announcement per turn per conversation).
type rosterState struct {
	mu sync.Mutex
	// inflight: handle -> convID -> a turn is running right now (a turn
	// marks itself on entry and clears on exit — the "running" roster
	// state, the only live signal a sibling can get).
	inflight map[string]map[string]bool
	// snap: convID -> (other convID -> the line last announced to it).
	// Delta-only announcements: a line is re-announced only when it
	// changed since this conversation last saw the roster.
	snap map[string]map[string]string
	// purposeTurns: convID -> user turns since the last set_purpose (the
	// nudge cadence counter; reset by the tool call itself).
	purposeTurns map[string]int
}

func newRosterState() *rosterState {
	return &rosterState{
		inflight:     map[string]map[string]bool{},
		snap:         map[string]map[string]string{},
		purposeTurns: map[string]int{},
	}
}

// enterInflight / leaveInflight bracket a turn: the conversation shows as
// "running" to its siblings for the turn's whole duration (a concurrent
// turn in a sibling sees this one's live state).
func (a *Agent) enterInflight(handle, convID string) {
	a.roster.mu.Lock()
	defer a.roster.mu.Unlock()
	if a.roster.inflight[handle] == nil {
		a.roster.inflight[handle] = map[string]bool{}
	}
	a.roster.inflight[handle][convID] = true
}

func (a *Agent) leaveInflight(handle, convID string) {
	a.roster.mu.Lock()
	defer a.roster.mu.Unlock()
	if set := a.roster.inflight[handle]; set != nil {
		delete(set, convID)
	}
}

func (a *Agent) isInflight(handle, convID string) bool {
	a.roster.mu.Lock()
	defer a.roster.mu.Unlock()
	return a.roster.inflight[handle][convID]
}

// purposeNudgeDue counts the turn and reports whether the purpose nudge is
// due (and how many turns since the last set_purpose, for the "N turns old"
// wording — staleness visible, never silent).
func (a *Agent) purposeNudgeDue(convID string) (due bool, turns int) {
	a.roster.mu.Lock()
	defer a.roster.mu.Unlock()
	a.roster.purposeTurns[convID]++
	n := a.roster.purposeTurns[convID]
	return n >= purposeNudgeCadence, n
}

// purposeSet is called by the set_purpose tool: the cadence restarts.
func (a *Agent) purposeSet(convID string) {
	a.roster.mu.Lock()
	defer a.roster.mu.Unlock()
	a.roster.purposeTurns[convID] = 0
}

// rosterStateWord is the roster's state word for a sibling conversation +
// whether it is IN the roster at all. The roster is presence-only: running
// (a turn in-flight right now — the turn bracket) or active (activity
// within the quiet grace). Idle siblings are not roster entries: the user's
// conversation list is unbounded in theory, and the announcement must stay
// bounded to who is actually around. A lazy classification (no timers, no
// events): "no LLM work observed recently" is what "active" actually means.
// An unparseable activity timestamp is treated as active (presence errs
// toward showing, not hiding, a sibling).
func rosterStateWord(c store.Conversation, inFlight bool) (state string, present bool) {
	if inFlight {
		return "running", true
	}
	t, err := time.Parse(time.RFC3339, c.LastActivityAt)
	if err != nil {
		return "active", true
	}
	if time.Since(t) <= rosterQuietGrace {
		return "active", true
	}
	return "", false
}

// rosterLine is one roster entry: state word + title + purpose + the
// conversation id (the id is what agent_message takes — the model needs no
// other lookup). No purpose = title only (the visible fallback: an
// unstated purpose degrades to the title, never asserts a dead world).
func rosterLine(c store.Conversation, state string) string {
	line := fmt.Sprintf("%s · %q", state, c.Title)
	if c.Purpose != "" {
		line += " — " + c.Purpose
	}
	return line + fmt.Sprintf(" (conv %s)", c.ID)
}

// announceTurnStart runs at the start of every user turn, after the user
// message is persisted: it computes (a) the roster delta — which sibling
// lines changed since this conversation's last announcement, (b) the
// messages the siblings queued to this conversation (delivered NOW, at this
// user turn — v1 never spawns work for an idle recipient), and (c) the
// purpose nudge (when due). If any part is non-empty, ONE roleCheckin row
// is appended (in the model's request, filtered from the UI); the roster
// snapshot updates when the roster part was announced, and the queued
// messages are marked delivered only after the append succeeded (a failed
// append leaves them queued for the next turn).
func (a *Agent) announceTurnStart(us *store.UserStore, handle, convID string, self *store.Conversation) error {
	convs, err := us.ListConversations()
	if err != nil {
		return err
	}
	pending, err := us.PendingAgentMessages(convID)
	if err != nil {
		return err
	}

	// The roster: the siblings PRESENT right now (running or active —
	// presence-only: the conversation list is unbounded in theory, so the
	// announcement is bounded to who is actually around; see
	// rosterStateWord).
	newSnap := map[string]string{}
	convExists := map[string]bool{}
	var lines []string
	a.roster.mu.Lock()
	oldSnap := a.roster.snap[convID]
	firstAnnounce := oldSnap == nil
	a.roster.mu.Unlock()
	for _, c := range convs {
		convExists[c.ID] = true
		if c.ID == convID {
			continue
		}
		state, present := rosterStateWord(c, a.isInflight(handle, c.ID))
		if !present {
			continue
		}
		line := rosterLine(c, state)
		newSnap[c.ID] = line
		switch {
		case firstAnnounce:
			lines = append(lines, line)
		case oldSnap[c.ID] == "":
			lines = append(lines, "+ "+line)
		case oldSnap[c.ID] != line:
			lines = append(lines, "~ "+line)
		}
	}
	// Departures (in the last snapshot, not present now): deleted
	// ("no longer exists") vs simply went idle ("went idle" — it still
	// exists, it is just no longer around). Each is announced once, at the
	// departure; nothing is tracked beyond that.
	if !firstAnnounce {
		for id, line := range oldSnap {
			if _, ok := newSnap[id]; ok {
				continue
			}
			if convExists[id] {
				lines = append(lines, "- "+line+" (went idle)")
			} else {
				lines = append(lines, "- "+line+" (no longer exists)")
			}
		}
	}

	// The queued sibling messages, addressed by sender (a deleted sender
	// degrades to its id — the message itself is never lost).
	var msgLines []string
	for _, m := range pending {
		sender, err := us.GetConversation(m.FromConversation)
		who := "conv " + m.FromConversation
		if err == nil {
			who = fmt.Sprintf("%q (conv %s)", sender.Title, sender.ID)
		}
		msgLines = append(msgLines, fmt.Sprintf("- from %s: %s", who, m.Text))
	}

	// The rolling-purpose nudge (the sole updater; meaningful only while
	// there are siblings to see the purpose).
	var nudgeLine string
	if len(newSnap) > 0 {
		if due, turns := a.purposeNudgeDue(convID); due {
			if self.Purpose == "" {
				nudgeLine = "PURPOSE CHECK: this conversation has no stated purpose — call set_purpose to state it (one line), so your siblings can see what you are doing."
			} else {
				nudgeLine = fmt.Sprintf("PURPOSE CHECK: the stated purpose is %d turns old — if what this conversation is doing has drifted, call set_purpose to refresh it.", turns)
			}
		}
	}

	if len(lines) == 0 && len(msgLines) == 0 && nudgeLine == "" {
		return nil // nothing new — the turn runs without an announcement
	}
	var b strings.Builder
	if len(lines) > 0 {
		if firstAnnounce {
			b.WriteString("Other conversations of yours (who is around right now — running = a turn is in progress, active = recent activity):\n")
		} else {
			b.WriteString("Changes among your other conversations since your last turn (+ new, ~ changed, - gone):\n")
		}
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
		b.WriteString("\n")
	}
	if len(msgLines) > 0 {
		b.WriteString("Messages from your sibling conversations:\n")
		for _, l := range msgLines {
			b.WriteString(l + "\n")
		}
		b.WriteString("\n")
	}
	if nudgeLine != "" {
		b.WriteString(nudgeLine + "\n")
	}
	text := strings.TrimRight(b.String(), "\n")
	if _, err := us.AppendMessage(convID, roleCheckin, text, "", "", nil); err != nil {
		return err
	}
	// The snapshot tracks what was ANNOUNCED about the siblings (the
	// message/nudge sections don't change it).
	if len(lines) > 0 {
		a.roster.mu.Lock()
		a.roster.snap[convID] = newSnap
		a.roster.mu.Unlock()
	}
	// The messages are in this conversation's history now — mark them
	// delivered (a failed append returned before this point).
	if len(msgLines) > 0 {
		return us.MarkAgentMessagesDelivered(convID)
	}
	return nil
}
