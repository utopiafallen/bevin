package store

import (
	"database/sql"
)

// AgentMessage is a row in the agent-to-agent queue (schema v3, concurrent-
// instances design, layer 4): a message one conversation sends to another
// of the same user. It is QUEUED, not pushed — delivered at the recipient's
// next user turn (v1 never spawns LLM work for an idle recipient).
// DeliveredAt == "" = still queued.
type AgentMessage struct {
	ID               int64  `json:"id"`
	FromConversation string `json:"from_conversation"`
	ToConversation   string `json:"to_conversation"`
	Text             string `json:"text"`
	CreatedAt        string `json:"created_at"`
	DeliveredAt      string `json:"delivered_at,omitempty"`
}

const agentMessageCols = `id, from_conversation, to_conversation, text, created_at, delivered_at`

func scanAgentMessage(rs interface{ Scan(...any) error }, m *AgentMessage) error {
	var delivered sql.NullString
	if err := rs.Scan(&m.ID, &m.FromConversation, &m.ToConversation, &m.Text, &m.CreatedAt, &delivered); err != nil {
		return err
	}
	m.DeliveredAt = delivered.String
	return nil
}

// SendAgentMessage queues a message from one conversation to another (both
// in this user's store; the caller validates).
func (s *UserStore) SendAgentMessage(from, to, text string) (*AgentMessage, error) {
	now := nowUTC()
	res, err := s.db.Exec(`INSERT INTO agent_messages (from_conversation, to_conversation, text, created_at) VALUES (?,?,?,?)`, from, to, text, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &AgentMessage{ID: id, FromConversation: from, ToConversation: to, Text: text, CreatedAt: now}, nil
}

// PendingAgentMessages returns the messages queued TO the conversation
// (still undelivered), oldest first.
func (s *UserStore) PendingAgentMessages(toConvID string) ([]AgentMessage, error) {
	rows, err := s.db.Query(`SELECT `+agentMessageCols+` FROM agent_messages WHERE to_conversation = ? AND delivered_at IS NULL ORDER BY id`, toConvID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentMessage
	for rows.Next() {
		var m AgentMessage
		if err := scanAgentMessage(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountPendingAgentMessages counts the messages still queued from one
// conversation to another (the per-pair cap: a sender that keeps replying
// without its messages being processed is refused, not queued forever).
func (s *UserStore) CountPendingAgentMessages(from, to string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE from_conversation = ? AND to_conversation = ? AND delivered_at IS NULL`, from, to).Scan(&n)
	return n, err
}

// MarkAgentMessagesDelivered stamps every queued message to the conversation
// as delivered. The harness calls it only AFTER the delivery is persisted in
// the recipient's history — marking before the append would lose the message
// if the append failed.
func (s *UserStore) MarkAgentMessagesDelivered(toConvID string) error {
	_, err := s.db.Exec(`UPDATE agent_messages SET delivered_at = ? WHERE to_conversation = ? AND delivered_at IS NULL`, nowUTC(), toConvID)
	return err
}
