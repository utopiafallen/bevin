package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Conversation is a P2 row.
type Conversation struct {
	ID                  string  `json:"id"`
	Title               string  `json:"title"`
	CreatedAt           string  `json:"created_at"`
	LastActivityAt      string  `json:"last_activity_at"`
	State               string  `json:"state"`
	FrozenSystemPrompt  string  `json:"frozen_system_prompt"`
	PromptInputVersions string  `json:"prompt_input_versions"`
	OwnerTaskID         *string `json:"owner_task_id,omitempty"`
	ActiveModelID       string  `json:"active_model_id"`
	// ReasoningEffort: the per-conversation reasoning-effort choice (one of
	// the active model's profile levels, e.g. low/medium/xhigh; "" = the
	// model's own default — the request builder sends nothing).
	ReasoningEffort string `json:"reasoning_effort"`
	// Purpose/Topic: the conversation's rolling self-reported summary
	// (concurrent-instances design, layer 2) — the one-liner the roster
	// shows to sibling conversations so a collision is a one-glance call.
	// Written by the summary nudge (SetConversationSummary), "" = unset.
	Purpose          string `json:"purpose"`
	Topic            string `json:"topic"`
	PurposeUpdatedAt string `json:"purpose_updated_at"`
	// WorkspaceID: the workspace LOADED into this conversation (the design
	// log's WORKSPACE LOADING section) — the pointer the conversation works
	// in. "" = no workspace. Loading is the pointer, not an event: its
	// derived effects are the workspace-local skill availability and the
	// search tool's default workspace scope.
	WorkspaceID string `json:"workspace_id"`
}

// ErrNotFound is returned when a row is missing.
var ErrNotFound = errors.New("not found")

func (s *UserStore) CreateConversation(title, activeModelID string) (*Conversation, error) {
	id := newID()
	now := nowUTC()
	_, err := s.db.Exec(
		`INSERT INTO conversations (id, title, created_at, last_activity_at, state, active_model_id) VALUES (?,?,?,?, 'active', ?)`,
		id, title, now, now, activeModelID)
	if err != nil {
		return nil, err
	}
	return &Conversation{ID: id, Title: title, CreatedAt: now, LastActivityAt: now, State: "active", ActiveModelID: activeModelID}, nil
}

const convCols = `id, title, created_at, last_activity_at, state, frozen_system_prompt, prompt_input_versions, owner_task_id, active_model_id, reasoning_effort, purpose, topic, purpose_updated_at, workspace_id`

func scanConv(rs interface{ Scan(...any) error }, c *Conversation) error {
	return rs.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.LastActivityAt, &c.State, &c.FrozenSystemPrompt, &c.PromptInputVersions, &c.OwnerTaskID, &c.ActiveModelID, &c.ReasoningEffort, &c.Purpose, &c.Topic, &c.PurposeUpdatedAt, &c.WorkspaceID)
}

func (s *UserStore) GetConversation(id string) (*Conversation, error) {
	c := &Conversation{}
	err := scanConv(s.db.QueryRow(`SELECT `+convCols+` FROM conversations WHERE id = ?`, id), c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// FindConversationByTitle returns the conversation with the given exact title
// (ErrNotFound when there is none).
func (s *UserStore) FindConversationByTitle(title string) (*Conversation, error) {
	c := &Conversation{}
	err := scanConv(s.db.QueryRow(`SELECT `+convCols+` FROM conversations WHERE title = ?`, title), c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// DeleteConversation removes a conversation and all of its message rows in
// one transaction (the FTS5 index follows via its triggers). The
// conversation row determines existence: a delete that matches nothing is
// ErrNotFound and rolls back, so a missing id never touches message rows.
func (s *UserStore) DeleteConversation(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM conversations WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE conversation_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// HasMessages reports whether the conversation has at least one message.
func (s *UserStore) HasMessages(convID string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *UserStore) ListConversations() ([]Conversation, error) {
	rows, err := s.db.Query(`SELECT ` + convCols + ` FROM conversations ORDER BY last_activity_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := scanConv(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConversationModel changes the conversation's active model. (The in-flight
// turn completes on the old model; the new model applies from the next turn.)
func (s *UserStore) SetConversationModel(id, modelID string) error {
	res, err := s.db.Exec(`UPDATE conversations SET active_model_id = ?, last_activity_at = ? WHERE id = ?`, modelID, nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetConversationOwnerTask links a conversation to the task that owns it
// (pass "" to clear).
func (s *UserStore) SetConversationOwnerTask(convID, taskID string) error {
	var v *string
	if taskID != "" {
		v = &taskID
	}
	res, err := s.db.Exec(`UPDATE conversations SET owner_task_id = ? WHERE id = ?`, v, convID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetConversationTitle updates a conversation's title.
func (s *UserStore) SetConversationTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	return err
}

// SetConversationReasoningEffort stores the per-conversation reasoning-effort
// choice ("" clears it — the model's own default then applies). The value is
// validated against the active model's profile by the caller (the store
// stores what it is told; the harness never sends an unsupported effort).
func (s *UserStore) SetConversationReasoningEffort(id, effort string) error {
	res, err := s.db.Exec(`UPDATE conversations SET reasoning_effort = ? WHERE id = ?`, effort, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetConversationWorkspace points the conversation at a workspace (the
// design log's WORKSPACE LOADING section — loading IS the pointer) or
// clears the pointer ("" = no workspace). The value is validated by the
// caller (a non-empty id must be one of the user's workspaces); the store
// stores what it is told.
func (s *UserStore) SetConversationWorkspace(id, workspaceID string) error {
	res, err := s.db.Exec(`UPDATE conversations SET workspace_id = ? WHERE id = ?`, workspaceID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetFrozenSystemPrompt stores the conversation's frozen system prompt:
// the base core + the user-profile section. The core never changes; the
// profile section is re-baked at every compaction event (design log
// 2026-11-25 "Profile in the system prompt"). It is the content of the
// system message the request builder sends.
func (s *UserStore) SetFrozenSystemPrompt(id, prompt string) error {
	res, err := s.db.Exec(`UPDATE conversations SET frozen_system_prompt = ? WHERE id = ?`, prompt, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetConversationSummary updates the conversation's rolling purpose/topic
// (concurrent-instances design, layer 2) and stamps when it was last updated.
// The summary nudge (set_conversation_summary tool) is the sole writer; the
// roster shows the staleness of this stamp, so an abandoned summary degrades
// visibly to the title instead of asserting a dead world.
func (s *UserStore) SetConversationSummary(id, purpose, topic string) error {
	res, err := s.db.Exec(`UPDATE conversations SET purpose = ?, topic = ?, purpose_updated_at = ? WHERE id = ?`, purpose, topic, nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Message is a P3 row.
type Message struct {
	ID                 int64   `json:"id"`
	ConversationID     string  `json:"conversation_id"`
	Role               string  `json:"role"`
	Content            string  `json:"content"`
	Reasoning          string  `json:"reasoning,omitempty"`
	ReasoningSignature string  `json:"-"` // the per-turn signature of the trace (Anthropic replay); never sent to the client
	Seq                int     `json:"seq"`
	CreatedAt          string  `json:"created_at"`
	ModelID            *string `json:"model_id,omitempty"`
	// CompactedFrom / CompactedTo: the prompt size in tokens before / after
	// the context compaction this summary row records (0/0 = a pre-existing
	// summary row, no tokens recorded). Set only on role=summary rows at
	// compaction time; the UI renders them as the persistent compaction
	// marker ("Context compacted from X to Y tokens").
	CompactedFrom int `json:"compacted_from,omitempty"`
	CompactedTo   int `json:"compacted_to,omitempty"`
}

// Messages are searched in the per-user vector DB (semantic) + FTS5 (lexical);
// they carry no local embedding column.
const msgCols = `id, conversation_id, role, content, reasoning, reasoning_signature, seq, created_at, model_id, compacted_from, compacted_to`

func scanMsg(rs interface{ Scan(...any) error }, m *Message) error {
	return rs.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Reasoning, &m.ReasoningSignature, &m.Seq, &m.CreatedAt, &m.ModelID, &m.CompactedFrom, &m.CompactedTo)
}

// AppendMessage adds a message to a conversation with the next seq (atomic).
// reasoning (the model's thinking trace for an assistant turn) +
// reasoningSignature (its per-turn signature, Anthropic) and modelID may be
// empty/nil.
func (s *UserStore) AppendMessage(convID, role, content, reasoning, reasoningSignature string, modelID *string) (*Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("append: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var next int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), -1) + 1 FROM messages WHERE conversation_id = ?`, convID).Scan(&next); err != nil {
		return nil, fmt.Errorf("append: seq: %w", err)
	}
	now := nowUTC()
	res, err := tx.Exec(`INSERT INTO messages (conversation_id, role, content, reasoning, reasoning_signature, seq, created_at, model_id) VALUES (?,?,?,?,?,?,?,?)`,
		convID, role, content, reasoning, reasoningSignature, next, now, modelID)
	if err != nil {
		return nil, fmt.Errorf("append: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("append: lastid: %w", err)
	}
	if _, err := tx.Exec(`UPDATE conversations SET last_activity_at = ? WHERE id = ?`, now, convID); err != nil {
		return nil, fmt.Errorf("append: touch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("append: commit: %w", err)
	}
	if s.AfterAppend != nil {
		// AFTER the commit: the row is durable, so the observer's view of
		// it is always consistent. A failed append never fires the hook —
		// and a hook that blocks or panics can't roll the row back.
		s.AfterAppend(convID, next)
	}
	return &Message{ID: id, ConversationID: convID, Role: role, Content: content, Reasoning: reasoning, ReasoningSignature: reasoningSignature, Seq: next, CreatedAt: now, ModelID: modelID}, nil
}

// SetMessageCompaction stamps the prompt size in tokens before/after the
// context compaction a summary row records (the UI's "Context compacted from
// X to Y tokens" marker). Called right after the summary row is appended.
func (s *UserStore) SetMessageCompaction(id int64, from, to int) error {
	res, err := s.db.Exec(`UPDATE messages SET compacted_from = ?, compacted_to = ? WHERE id = ?`, from, to, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *UserStore) GetMessage(id int64) (*Message, error) {
	m := &Message{}
	err := scanMsg(s.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id = ?`, id), m)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *UserStore) ListMessages(convID string) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages WHERE conversation_id = ? ORDER BY seq`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := scanMsg(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMessagesPaged returns one page of a conversation's DISPLAYABLE messages
// for the chat log's lazy loading: the most recent limit of them in seq order,
// optionally only those with seq < beforeSeq (the scroll-up cursor — the
// client prepends each page as it scrolls up). beforeSeq <= 0 means "no
// cursor" (the newest page).
//
// Only the displayable roles (user/assistant/summary) count toward the page
// size: check-in + system rows are model-facing internal rows the display path
// drops, so filtering here keeps "limit = N" meaning "N visible messages"
// instead of N raw rows (which would render as far fewer).
func (s *UserStore) ListMessagesPaged(convID string, limit, beforeSeq int) ([]Message, error) {
	if limit <= 0 {
		return s.ListMessages(convID)
	}
	query := `SELECT ` + msgCols + ` FROM messages WHERE conversation_id = ? AND role IN ('user','assistant','summary')`
	args := []any{convID}
	if beforeSeq > 0 {
		query += ` AND seq < ?`
		args = append(args, beforeSeq)
	}
	// Newest-first so the LIMIT takes the most recent `limit`; the result is
	// reversed back to seq order (oldest first) for the display.
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	desc := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		if err := scanMsg(rows, &m); err != nil {
			return nil, err
		}
		desc = append(desc, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(desc))
	for i := len(desc) - 1; i >= 0; i-- {
		out = append(out, desc[i])
	}
	return out, nil
}

// MaxDisplaySeq returns the highest seq among the conversation's displayable
// rows (user/assistant/summary) — 0 when it has none. The multi-device
// stream handler snapshots it BEFORE a turn so the done event can carry the
// rows the turn persisted.
func (s *UserStore) MaxDisplaySeq(convID string) (int, error) {
	var seq int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM messages WHERE conversation_id = ? AND role IN ('user','assistant','summary')`, convID).Scan(&seq)
	return seq, err
}

// DisplaySeqsAfter returns the seqs of the conversation's displayable rows
// (user/assistant/summary) newer than afterSeq, in seq order — the
// multi-device done-event payload: the client tags the rows it rendered
// optimistically (which have no seq until persisted) so a sync pull
// recognizes them instead of rendering them twice.
func (s *UserStore) DisplaySeqsAfter(convID string, afterSeq int) ([]int, error) {
	rows, err := s.db.Query(`SELECT seq FROM messages WHERE conversation_id = ? AND role IN ('user','assistant','summary') AND seq > ? ORDER BY seq ASC`, convID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []int
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return seqs, nil
}

// SearchMessages returns messages matching an FTS5 query, ranked by bm25.
// query must be valid FTS5 syntax (the caller sanitizes user text first).
func (s *UserStore) SearchMessages(query string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT `+msgCols+`
		FROM messages
		WHERE EXISTS (SELECT 1 FROM messages_fts WHERE messages_fts.rowid = messages.id AND messages_fts MATCH ?)
		ORDER BY id
		LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := scanMsg(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
