package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// newRosterTestAgent: a test agent whose m1 provider is the fake (so real
// turns run without a model backend) + the user's store.
func newRosterTestAgent(t *testing.T) (*Agent, *store.UserStore, *fakeProvider) {
	t.Helper()
	a, _, _ := newTestAgent(t)
	t.Cleanup(a.Close)
	fake := &fakeProvider{}
	a.mu.Lock()
	a.providers["m1"] = fake
	a.mu.Unlock()
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	return a, us, fake
}

// lastCheckin returns the content of the conversation's latest roleCheckin
// row ("" when it has none).
func lastCheckin(t *testing.T, us *store.UserStore, convID string) string {
	t.Helper()
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == roleCheckin {
			return msgs[i].Content
		}
	}
	return ""
}

// newRosterConversation creates a conversation through the AGENT-level path
// (the store's CreateConversation leaves the active model blank; the agent's
// resolves the default — m1 here, the fake).
func newRosterConversation(t *testing.T, a *Agent, title string) *store.Conversation {
	t.Helper()
	id, err := a.CreateConversation(context.Background(), "u1", title, "", "")
	if err != nil {
		t.Fatal(err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := us.GetConversation(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func countCheckins(t *testing.T, us *store.UserStore, convID string) int {
	t.Helper()
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.Role == roleCheckin {
			n++
		}
	}
	return n
}

// TestRosterFirstTurnAndDelta: the first turn announces the full roster;
// unchanged turns announce nothing; a changed line announces only the delta;
// a deleted conversation announces its departure. The announcement is a
// roleCheckin row (in the model's request, filtered from the UI).
func TestRosterFirstTurnAndDelta(t *testing.T) {
	a, us, fake := newRosterTestAgent(t)
	ca := newRosterConversation(t, a, "Alpha")
	cb := newRosterConversation(t, a, "Beta")
	if err := us.SetConversationSummary(cb.ID, "writing the Q3 report", "reports"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Turn 1: the full roster (Beta with its purpose + conv id).
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "hi", nil); err != nil {
		t.Fatal(err)
	}
	ci := lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "Other conversations of yours") {
		t.Fatalf("first turn: want the full roster header, got %q", ci)
	}
	if !strings.Contains(ci, `"Beta"`) || !strings.Contains(ci, "writing the Q3 report") || !strings.Contains(ci, "conv "+cb.ID) {
		t.Fatalf("first turn: Beta's line missing title/purpose/id: %q", ci)
	}
	// The model saw it (a user-role message in the request — the check-in
	// pattern) and the UI filtered it.
	var sawCheckin bool
	for _, m := range fake.lastCall().Messages {
		if m.Role == provider.RoleUser && strings.Contains(m.Text, "Other conversations of yours") {
			sawCheckin = true
		}
	}
	if !sawCheckin {
		t.Fatal("the roster announcement must be in the model's request")
	}
	visible, err := a.ConversationMessages("u1", ca.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range visible {
		if m.Role != "user" && m.Role != "assistant" {
			t.Fatalf("the checkin row leaked into the UI history: %+v", m)
		}
	}

	// Turn 2: nothing changed — no new announcement.
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "hi again", nil); err != nil {
		t.Fatal(err)
	}
	if n := countCheckins(t, us, ca.ID); n != 1 {
		t.Fatalf("unchanged turn announced again (%d checkins)", n)
	}

	// Delta: Beta's purpose changes — only the changed line is announced.
	if err := us.SetConversationSummary(cb.ID, "refactoring the billing module", "billing"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "third", nil); err != nil {
		t.Fatal(err)
	}
	ci = lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "Changes among your other conversations") || !strings.Contains(ci, "~ ") || !strings.Contains(ci, "refactoring the billing module") {
		t.Fatalf("delta turn: want the ~ line with the new purpose, got %q", ci)
	}

	// Departure: Beta is deleted — its line is announced as gone.
	if err := us.DeleteConversation(cb.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "fourth", nil); err != nil {
		t.Fatal(err)
	}
	ci = lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "- ") || !strings.Contains(ci, "no longer exists") || !strings.Contains(ci, "refactoring the billing module") {
		t.Fatalf("departure turn: want the - line for the deleted conversation, got %q", ci)
	}
}

// TestRosterInflightState: a sibling mid-turn shows as "running"; after its
// own turn ends, a conversation is no longer in-flight (the bracket covers
// the whole turn, including early errors).
func TestRosterInflightState(t *testing.T) {
	a, us, _ := newRosterTestAgent(t)
	ca := newRosterConversation(t, a, "Alpha")
	cb := newRosterConversation(t, a, "Beta")

	// Beta is mid-turn (a concurrent runTurn in flight).
	a.enterInflight("u1", cb.ID)
	if _, _, err := a.Turn(context.Background(), "u1", ca.ID, "hi", nil); err != nil {
		t.Fatal(err)
	}
	ci := lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "running · \"Beta\"") {
		t.Fatalf("want Beta's line as running, got %q", ci)
	}
	a.leaveInflight("u1", cb.ID)

	// Alpha's own turn brackets it: in-flight during, cleared after.
	if a.isInflight("u1", ca.ID) {
		t.Fatal("Alpha must not be in-flight before its turn")
	}
	if _, _, err := a.Turn(context.Background(), "u1", ca.ID, "again", nil); err != nil {
		t.Fatal(err)
	}
	if a.isInflight("u1", ca.ID) {
		t.Fatal("Alpha must not be in-flight after its turn (the defer must have cleared it)")
	}
}

// TestPurposeNudge: the purpose nudge fires once per cadence (the sole
// updater), distinguishes "no purpose yet" from "N turns old", and the
// set_purpose tool resets the cadence + lands in the siblings' roster.
func TestPurposeNudge(t *testing.T) {
	a, us, _ := newRosterTestAgent(t)
	ca := newRosterConversation(t, a, "Alpha")
	newRosterConversation(t, a, "Beta")
	ctx := context.Background()

	// Turns 1-6: no nudge (turn 1's announcement is the roster, nothing
	// else — a NEW announcement with a purpose check is the failure).
	for i := 1; i <= 6; i++ {
		before := countCheckins(t, us, ca.ID)
		if _, _, err := a.Turn(ctx, "u1", ca.ID, fmt.Sprintf("t%d", i), nil); err != nil {
			t.Fatal(err)
		}
		if countCheckins(t, us, ca.ID) > before {
			ci := lastCheckin(t, us, ca.ID)
			if strings.Contains(ci, "PURPOSE CHECK") {
				t.Fatalf("turn %d: the nudge fired early: %q", i, ci)
			}
		}
	}
	// Turn 7: due — no purpose yet.
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "t7", nil); err != nil {
		t.Fatal(err)
	}
	ci := lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "PURPOSE CHECK") || !strings.Contains(ci, "no stated purpose") {
		t.Fatalf("turn 7: want the no-purpose nudge, got %q", ci)
	}

	// set_purpose (the tool, with the conversation context): the cadence
	// resets + the purpose lands.
	tl, ok := a.tools.Get("set_purpose")
	if !ok {
		t.Fatal("set_purpose not registered")
	}
	tctx := context.WithValue(ctx, ctxConversationKey, ca.ID)
	out, err := tl.Execute(tctx, "u1", []byte(`{"purpose":"debugging the login 500"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "purpose updated") {
		t.Fatalf("set_purpose result = %q", out)
	}
	got, err := us.GetConversation(ca.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Purpose != "debugging the login 500" || got.PurposeUpdatedAt == "" {
		t.Fatalf("purpose row = %+v", got)
	}
	// Bad args are results, not errors.
	if out, _ := tl.Execute(tctx, "u1", []byte(`{"purpose":"  "}`)); !strings.Contains(out, "set_purpose needs") {
		t.Fatalf("blank purpose result = %q", out)
	}
	if out, _ := tl.Execute(tctx, "u1", []byte(`{"purpose":"`+strings.Repeat("x", 201)+`"}`)); !strings.Contains(out, "too long") {
		t.Fatalf("long purpose result = %q", out)
	}

	// Turns 8-13: no announcement at all (the cadence restarted and the
	// roster is unchanged); turn 14: due again, worded by staleness.
	for i := 8; i <= 13; i++ {
		before := countCheckins(t, us, ca.ID)
		if _, _, err := a.Turn(ctx, "u1", ca.ID, fmt.Sprintf("t%d", i), nil); err != nil {
			t.Fatal(err)
		}
		if after := countCheckins(t, us, ca.ID); after != before {
			t.Fatalf("turn %d: unexpected announcement: %q", i, lastCheckin(t, us, ca.ID))
		}
	}
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "t14", nil); err != nil {
		t.Fatal(err)
	}
	ci = lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "PURPOSE CHECK") || !strings.Contains(ci, "7 turns old") {
		t.Fatalf("turn 14: want the staleness nudge, got %q", ci)
	}

	// The sibling's roster now carries Alpha's purpose.
	convs, err := us.ListConversations()
	if err != nil {
		t.Fatal(err)
	}
	var betaID string
	for _, c := range convs {
		if c.Title == "Beta" {
			betaID = c.ID
		}
	}
	if _, _, err := a.Turn(ctx, "u1", betaID, "beta turn", nil); err != nil {
		t.Fatal(err)
	}
	ci = lastCheckin(t, us, betaID)
	if !strings.Contains(ci, `"Alpha"`) || !strings.Contains(ci, "debugging the login 500") {
		t.Fatalf("Beta's roster must carry Alpha's purpose: %q", ci)
	}
}

// TestPurposeNudgeNoSiblings: the nudge fires with NO sibling present —
// the purpose also labels the conversation in the user's chat list, so the
// nudge is not gated on siblings. Solo conversation: quiet turns, a
// nudge-only checkin on the due turn (no roster section), set_purpose
// resets, staleness wording on the next due turn.
func TestPurposeNudgeNoSiblings(t *testing.T) {
	a, us, _ := newRosterTestAgent(t)
	ca := newRosterConversation(t, a, "Solo")
	ctx := context.Background()

	// Turns 1-6: nothing at all (no siblings, nudge not yet due).
	for i := 1; i <= 6; i++ {
		if _, _, err := a.Turn(ctx, "u1", ca.ID, fmt.Sprintf("t%d", i), nil); err != nil {
			t.Fatal(err)
		}
		if ci := lastCheckin(t, us, ca.ID); ci != "" {
			t.Fatalf("turn %d: unexpected announcement: %q", i, ci)
		}
	}
	// Turn 7: the nudge, ALONE (no siblings → no roster section).
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "t7", nil); err != nil {
		t.Fatal(err)
	}
	ci := lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "PURPOSE CHECK") || !strings.Contains(ci, "no stated purpose") {
		t.Fatalf("turn 7: want the no-purpose nudge, got %q", ci)
	}
	if strings.Contains(ci, "conversations of yours") || strings.Contains(ci, "Changes among") {
		t.Fatalf("a nudge-only announcement must not carry a roster section: %q", ci)
	}

	// set_purpose resets the cadence.
	tl, ok := a.tools.Get("set_purpose")
	if !ok {
		t.Fatal("set_purpose not registered")
	}
	tctx := context.WithValue(ctx, ctxConversationKey, ca.ID)
	if out, err := tl.Execute(tctx, "u1", []byte(`{"purpose":"solo research"}`)); err != nil || !strings.Contains(out, "purpose updated") {
		t.Fatalf("set_purpose = %q, %v", out, err)
	}

	// Turns 8-13: quiet again; turn 14: the staleness nudge, alone.
	for i := 8; i <= 13; i++ {
		before := countCheckins(t, us, ca.ID)
		if _, _, err := a.Turn(ctx, "u1", ca.ID, fmt.Sprintf("t%d", i), nil); err != nil {
			t.Fatal(err)
		}
		if after := countCheckins(t, us, ca.ID); after != before {
			t.Fatalf("turn %d: unexpected announcement: %q", i, lastCheckin(t, us, ca.ID))
		}
	}
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "t14", nil); err != nil {
		t.Fatal(err)
	}
	ci = lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "PURPOSE CHECK") || !strings.Contains(ci, "7 turns old") {
		t.Fatalf("turn 14: want the staleness nudge, got %q", ci)
	}
}

// TestAgentMessageQueueAndDelivery: agent_message queues (never pushes), the
// recipient gets it at its next user turn (marked delivered), self/unknown
// sends are refused, and the per-pair cap refuses a sender that keeps
// replying.
func TestAgentMessageQueueAndDelivery(t *testing.T) {
	a, us, _ := newRosterTestAgent(t)
	ca := newRosterConversation(t, a, "Alpha")
	cb := newRosterConversation(t, a, "Beta")
	tl, ok := a.tools.Get("agent_message")
	if !ok {
		t.Fatal("agent_message not registered")
	}
	ctx := context.Background()
	tctx := context.WithValue(ctx, ctxConversationKey, ca.ID)
	send := func(to, msg string) string {
		out, err := tl.Execute(tctx, "u1", []byte(fmt.Sprintf(`{"to":%q,"message":%q}`, to, msg)))
		if err != nil {
			t.Fatalf("agent_message errored (it must be a result): %v", err)
		}
		return out
	}

	// Self + unknown recipients.
	if out := send(ca.ID, "hey me"); !strings.Contains(out, "your own conversation") {
		t.Fatalf("self-send result = %q", out)
	}
	if out := send("zz", "hi"); !strings.Contains(out, "no conversation") {
		t.Fatalf("unknown-recipient result = %q", out)
	}
	// Empty message.
	if out := send(cb.ID, "  "); !strings.Contains(out, "non-empty") {
		t.Fatalf("empty-message result = %q", out)
	}

	// A real send: queued, not delivered.
	out := send(cb.ID, "the 500 is a nil deref in session.go")
	if !strings.Contains(out, "queued to") || !strings.Contains(out, `"Beta"`) {
		t.Fatalf("send result = %q", out)
	}
	pending, err := us.PendingAgentMessages(cb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].FromConversation != ca.ID || pending[0].DeliveredAt != "" {
		t.Fatalf("pending = %+v", pending)
	}
	// v1 never spawns work for an idle recipient: no Beta turn = nothing
	// delivered.
	if n := countCheckins(t, us, cb.ID); n != 0 {
		t.Fatalf("an idle recipient got %d announcements without a user turn", n)
	}

	// Beta's next user turn: the message is delivered in the announcement
	// (the delivery section IS the never-silent guarantee — the roster is
	// presence-only, no per-line pending notes) + marked delivered.
	if _, _, err := a.Turn(ctx, "u1", cb.ID, "hi", nil); err != nil {
		t.Fatal(err)
	}
	ci := lastCheckin(t, us, cb.ID)
	if !strings.Contains(ci, "Messages from your sibling") || !strings.Contains(ci, `"Alpha"`) || !strings.Contains(ci, "the 500 is a nil deref in session.go") {
		t.Fatalf("delivery missing from the announcement: %q", ci)
	}
	pending, _ = us.PendingAgentMessages(cb.ID)
	if len(pending) != 0 {
		t.Fatalf("messages not marked delivered: %+v", pending)
	}

	// The per-pair cap: three queued from Alpha to Beta, the fourth refused.
	for i := 1; i <= 3; i++ {
		if out := send(cb.ID, fmt.Sprintf("note %d", i)); !strings.Contains(out, "queued to") {
			t.Fatalf("send %d refused early: %q", i, out)
		}
	}
	out = send(cb.ID, "note 4")
	if !strings.Contains(out, "already 3 queued") || !strings.Contains(out, "don't keep replying") {
		t.Fatalf("cap result = %q", out)
	}
	if n, err := us.CountPendingAgentMessages(ca.ID, cb.ID); err != nil || n != 3 {
		t.Fatalf("pending count = %d, %v (want 3 — the refused send must not queue)", n, err)
	}
}

// TestRosterPresenceOnly: the conversation list is unbounded in theory, so
// the roster is presence-only — running or active (within the quiet grace)
// siblings are announced; idle ones are not. A sibling that went idle is
// announced once as a departure ("went idle"), a deleted one as
// "no longer exists", and a returning sibling comes back with "+".
func TestRosterPresenceOnly(t *testing.T) {
	a, us, _ := newRosterTestAgent(t)
	ca := newRosterConversation(t, a, "Alpha")
	cb := newRosterConversation(t, a, "Beta")
	ctx := context.Background()
	t.Cleanup(func() { rosterQuietGrace = 15 * time.Minute })

	// Grace 0: anything with past activity is idle → the roster is empty
	// → all idle means nothing announced.
	rosterQuietGrace = 0
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if ci := lastCheckin(t, us, ca.ID); ci != "" {
		t.Fatalf("all-idle siblings must not be announced, got: %q", ci)
	}

	// Beta is running right now → present regardless of grace (the
	// in-flight flag beats the quiet window).
	a.enterInflight("u1", cb.ID)
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "hi again", nil); err != nil {
		t.Fatal(err)
	}
	ci := lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, `running · "Beta"`) {
		t.Fatalf("the running sibling must be announced: %q", ci)
	}
	a.leaveInflight("u1", cb.ID)

	// Beta went idle → the departure is announced once as went-idle (NOT
	// no-longer-exists — the conversation still exists).
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "hi again 2", nil); err != nil {
		t.Fatal(err)
	}
	ci = lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, "(went idle)") || strings.Contains(ci, "no longer exists") {
		t.Fatalf("the idle departure must be announced as went-idle: %q", ci)
	}
	// ...and not re-announced on the next turn (still absent, nothing new).
	before := countCheckins(t, us, ca.ID)
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "hi again 3", nil); err != nil {
		t.Fatal(err)
	}
	if after := countCheckins(t, us, ca.ID); after != before {
		t.Fatalf("no re-announcement for a still-absent sibling: %q", lastCheckin(t, us, ca.ID))
	}

	// Beta is active again (grace restored + fresh activity) → returns with "+".
	rosterQuietGrace = 15 * time.Minute
	if _, err := us.AppendMessage(cb.ID, roleUser, "ping", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Turn(ctx, "u1", ca.ID, "hi again 4", nil); err != nil {
		t.Fatal(err)
	}
	ci = lastCheckin(t, us, ca.ID)
	if !strings.Contains(ci, `+ active · "Beta"`) {
		t.Fatalf("a returning sibling must be announced with +: %q", ci)
	}
}
