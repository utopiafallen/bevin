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

const convCols = `id, title, created_at, last_activity_at, state, frozen_system_prompt, prompt_input_versions, owner_task_id, active_model_id`

func scanConv(rs interface{ Scan(...any) error }, c *Conversation) error {
	return rs.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.LastActivityAt, &c.State, &c.FrozenSystemPrompt, &c.PromptInputVersions, &c.OwnerTaskID, &c.ActiveModelID)
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

// HasMessages reports whether the conversation has any messages.
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

// SetFrozenSystemPrompt stores the system prompt frozen at conversation start.
// It is the content of the system message the request builder sends; it is not
// mutated in place (a profile/input change appends a new system message later).
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
}

// Messages are searched in the per-user vector DB (semantic) + FTS5 (lexical);
// they carry no local embedding column.
const msgCols = `id, conversation_id, role, content, reasoning, reasoning_signature, seq, created_at, model_id`

func scanMsg(rs interface{ Scan(...any) error }, m *Message) error {
	return rs.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Reasoning, &m.ReasoningSignature, &m.Seq, &m.CreatedAt, &m.ModelID)
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
	return &Message{ID: id, ConversationID: convID, Role: role, Content: content, Reasoning: reasoning, ReasoningSignature: reasoningSignature, Seq: next, CreatedAt: now, ModelID: modelID}, nil
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