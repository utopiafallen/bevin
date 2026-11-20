package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// The idle learning loop (the memory model's "idle reflection"): when a
// conversation has been idle long enough AND no other LLM work is running
// for that user, a learning pass runs — a meta call on the conversation's
// active model that extracts durable memories from the new turns and
// persists them with provenance. A new turn cancels the pending pass.
// Parameters are internal constants for now (a [learning] config section is
// later — Open).

const (
	learningIdleDelay    = time.Hour        // the DECIDED "conversation idle >= 1 h"
	learningBusyDeferral = 10 * time.Minute // the user is still working: re-check
	learningMinUserTurns = 2                // assume: skip one-off exchanges (confirm)
	learningMaxTurns     = 40               // pass-input caps (keep the meta call small)
	learningMaxTurnChars = 2000
	learningMaxMemories  = 50
)

// llmWork tracks, per user, how many LLM works are running in the agent
// (turns). The scheduler pools are per-model and shared across users, so
// the per-user "no other LLM work running" gate lives here, where the
// turn-completion hook is. The learning pass is a meta call, not a turn —
// it is not counted (it only runs while the counter is 0 anyway).
type llmWorkTracker struct {
	mu sync.Mutex
	n  map[string]int
}

func newLLMWorkTracker() *llmWorkTracker {
	return &llmWorkTracker{n: map[string]int{}}
}

func (t *llmWorkTracker) enter(handle string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n[handle]++
}

func (t *llmWorkTracker) leave(handle string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n[handle]--
	if t.n[handle] <= 0 {
		delete(t.n, handle)
	}
}

func (t *llmWorkTracker) idle(handle string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n[handle] == 0
}

// learningState is one conversation's pass state: how far learning has
// covered (lastMsgID) and the pending idle timer. In agent memory ONLY — a
// restart loses pending passes (they re-arm at the next turn) and re-covers
// older turns (on-write consolidation dedupes). No schema change: the
// no-migrations rule forbids a column existing stores would lack.
type learningState struct {
	lastMsgID   int64
	passRunning bool
	timer       *time.Timer
}

// learningMgr is the per-user idle learning loop (the agent is multi-user;
// state keys on handle+conversation). delay/busyDeferral default to the
// constants (tests shorten them).
type learningMgr struct {
	a            *Agent
	mu           sync.Mutex
	st           map[string]*learningState
	delay        time.Duration
	busyDeferral time.Duration
}

func newLearningMgr(a *Agent) *learningMgr {
	return &learningMgr{a: a, st: map[string]*learningState{}, delay: learningIdleDelay, busyDeferral: learningBusyDeferral}
}

func learningKey(handle, convID string) string { return handle + "\x00" + convID }

// onTurnComplete is the hook (runScheduled, after a clean turn): cancel any
// pending pass and arm a fresh idle timer for this conversation.
func (m *learningMgr) onTurnComplete(handle, convID string) {
	key := learningKey(handle, convID)
	m.mu.Lock()
	st, ok := m.st[key]
	if !ok {
		st = &learningState{}
		m.st[key] = st
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(m.delay, func() { m.runPass(handle, convID) })
	m.mu.Unlock()
}

// drop forgets a conversation's state (it was deleted).
func (m *learningMgr) drop(handle, convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.st[learningKey(handle, convID)]; st != nil {
		if st.timer != nil {
			st.timer.Stop()
		}
	}
	delete(m.st, learningKey(handle, convID))
}

// runPass is one learning pass: gate on the user's idleness, gather the new
// turns + existing memories, ask the active model for the durable facts,
// persist them with provenance, and advance lastMsgID.
func (m *learningMgr) runPass(handle, convID string) {
	key := learningKey(handle, convID)
	m.mu.Lock()
	st, ok := m.st[key]
	if !ok || st.passRunning {
		m.mu.Unlock()
		return
	}
	st.passRunning = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		st.passRunning = false
		m.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Gate: no other LLM work running for this user (the DECIDED condition).
	if !m.a.llmWork.idle(handle) {
		m.armLater(handle, convID, m.busyDeferral)
		return
	}
	us, err := m.a.userStore(handle)
	if err != nil {
		return
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		m.drop(handle, convID)
		return
	}
	hist, err := us.ListMessages(convID)
	if err != nil {
		return
	}
	var newMsgs []store.Message
	for _, msg := range hist {
		if msg.ID > st.lastMsgID {
			newMsgs = append(newMsgs, msg)
		}
	}
	newUser := 0
	for _, msg := range newMsgs {
		if msg.Role == "user" {
			newUser++
		}
	}
	if len(newMsgs) == 0 || newUser < learningMinUserTurns {
		return // nothing durable to learn yet; a later turn re-arms
	}
	// The active model's provider (the pass runs under it, like compaction).
	p, ok := m.a.providerFor(conv.ActiveModelID)
	if !ok {
		log.Printf("learning pass %s/%s: model %s unavailable, skipping (retries at the next idle window)", handle, convID, conv.ActiveModelID)
		return
	}
	mems, err := us.ListActiveMemories()
	if err != nil {
		return
	}

	extracted, err := m.learningCall(ctx, p, conv, newMsgs, mems)
	if err != nil {
		log.Printf("learning pass %s/%s: %v (skipped; the window retries)", handle, convID, err)
		return
	}
	maxID := st.lastMsgID
	for _, msg := range newMsgs {
		if msg.ID > maxID {
			maxID = msg.ID
		}
	}
	m.a.persistLearned(ctx, us, handle, convID, newMsgs, extracted)
	m.mu.Lock()
	st.lastMsgID = maxID
	m.mu.Unlock()
}

// armLater defers a pass (the user was still working at fire time).
func (m *learningMgr) armLater(handle, convID string, d time.Duration) {
	key := learningKey(handle, convID)
	m.mu.Lock()
	st, ok := m.st[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(d, func() { m.runPass(handle, convID) })
	m.mu.Unlock()
}

// learningSystemPrompt is the pass's own system prompt (separate from the
// conversation's frozen one — the meta-call rule).
const learningSystemPrompt = `You are bevin's memory-learning pass. From the new conversation turns, extract the DURABLE, user-relevant facts worth remembering in future conversations: facts about the user (preferences, people, projects, commitments, plans), decisions that were made, and corrections to earlier facts. Be conservative: 0 to 3 memories is typical; output NONE when nothing durable happened. Do not duplicate the existing memories. A "facet" is a short label for the fact's topic (e.g. "work", "preferences", "family"). A "subject" is the person or thing the fact is mainly about, when there is one (e.g. the user's name or a project). "confidence" is 0.0-1.0 (how sure you are the fact is true and durable). If the new turn CORRECTS an existing memory, include that memory's id in "supersede_id". Reply with JSON ONLY: {"memories":[{"facet":"...","statement":"...","subject":"...","confidence":0.9,"supersede_id":7}]}`

// learningCall is the meta call: the conversation's active model extracts
// the durable facts from the new turns. It carries NO conversation_id on the
// wire (its prompt is not a prefix of the conversation's — the KV-cache
// rule from compaction), and no reasoning effort (a light extraction job).
func (m *learningMgr) learningCall(ctx context.Context, p provider.Provider, conv *store.Conversation, newMsgs []store.Message, mems []store.Memory) ([]learnedMemory, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Conversation: %s\n\nNew turns (oldest first):\n", conv.Title)
	if len(newMsgs) > learningMaxTurns {
		newMsgs = newMsgs[len(newMsgs)-learningMaxTurns:]
	}
	for _, msg := range newMsgs {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		text := msg.Content
		if len(text) > learningMaxTurnChars {
			text = text[:learningMaxTurnChars] + "…"
		}
		fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(msg.Role), text)
	}
	b.WriteString("\nExisting memories (do not duplicate these):\n")
	if len(mems) == 0 {
		b.WriteString("(none)\n")
	}
	for i, mem := range mems {
		if i >= learningMaxMemories {
			break
		}
		fmt.Fprintf(&b, "#%d [%s] %s\n", mem.ID, mem.Facet, mem.Statement)
	}
	b.WriteString("\nExtract the durable facts now (JSON only).")
	res, err := p.Chat(ctx, m.a.modelAPIName(conv.ActiveModelID), provider.ChatRequest{
		System:   []provider.ContentPart{{Text: learningSystemPrompt}},
		Messages: []provider.Message{{Role: provider.RoleUser, Text: b.String()}},
	})
	if err != nil {
		return nil, err
	}
	return parseLearningReply(res.Text)
}

// parseLearningReply parses the pass's JSON reply (tolerating a code fence
// around it — models love fences). A non-object or unparseable reply is an
// error (the window retries; lastMsgID is not advanced).
func parseLearningReply(text string) ([]learnedMemory, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = text[3:]
		if i := strings.Index(text, "\n"); i >= 0 {
			text = text[i+1:]
		}
		if i := strings.LastIndex(text, "```"); i >= 0 {
			text = text[:i]
		}
		text = strings.TrimSpace(text)
	}
	var doc struct {
		Memories []learnedMemory `json:"memories"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("the learning reply was not valid JSON: %v", err)
	}
	return doc.Memories, nil
}

// persistLearned stores the pass's extracted memories (consolidating against
// the user's existing set + the batch itself) with provenance. Failures are
// logged, never fatal — the pass's window already advanced.
func (a *Agent) persistLearned(ctx context.Context, us *store.UserStore, handle, convID string, newMsgs []store.Message, extracted []learnedMemory) {
	var msgIDs []int64
	for _, msg := range newMsgs {
		msgIDs = append(msgIDs, msg.ID)
	}
	seen := map[string]bool{}
	for _, lm := range extracted {
		facet := strings.TrimSpace(lm.Facet)
		statement := strings.TrimSpace(lm.Statement)
		if facet == "" || statement == "" {
			continue
		}
		if len(facet) > 64 {
			facet = facet[:64]
		}
		if len(statement) > 1000 {
			statement = statement[:1000]
		}
		key := strings.ToLower(facet + "\x00" + statement)
		if seen[key] {
			continue
		}
		seen[key] = true
		conf := lm.Confidence
		if conf <= 0 {
			conf = 0.8
		}
		if conf > 1 {
			conf = 1
		}
		var emb []float32
		if a.vec != nil {
			if v, err := a.vec.embedInputs(ctx, []string{statement}); err == nil {
				emb = v
			}
		}
		created, err := us.CreateMemory(facet, statement, lm.Subject, conf, "learn:"+convID, emb)
		if err != nil {
			log.Printf("learning persist %s: %v", convID, err)
			continue
		}
		if err := us.SetMemoryProvenance(created.ID, msgIDs); err != nil {
			log.Printf("learning provenance %s/#%d: %v", convID, created.ID, err)
		}
		if lm.SupersedeID != nil {
			if old, err := us.GetMemory(*lm.SupersedeID); err == nil && old.Status == "active" {
				if err := us.SupersedeMemory(old.ID, created.ID); err != nil {
					log.Printf("learning supersede %s/#%d: %v", convID, old.ID, err)
				}
			}
		}
	}
}

// learnedMemory is one fact the pass extracted (the model's JSON shape).
type learnedMemory struct {
	Facet       string  `json:"facet"`
	Statement   string  `json:"statement"`
	Subject     *string `json:"subject,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
	SupersedeID *int64  `json:"supersede_id,omitempty"`
}
