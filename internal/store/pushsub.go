package store

import (
	"database/sql"
	"fmt"
)

// Web-push subscriptions (the PWA stage 2): one row per subscribed
// DEVICE. The per-user store is already per-user (one store per user),
// so the row carries only the device's subscription. The subscription IS
// the delivery gate — there is no separate "notifications on" flag; a
// device leaves by removing its row.

// PushSubscription is one subscribed device's Web Push subscription
// (the browser's PushSubscription, minus the transport details).
type PushSubscription struct {
	Endpoint  string `json:"endpoint"`
	P256dh    string `json:"p256dh"`
	Auth      string `json:"auth"`
	UserAgent string `json:"user_agent"`
	CreatedAt string `json:"created_at"`
}

// AddPushSubscription upserts the subscription (the same endpoint
// re-subscribing — the device's keys refresh in place).
func (s *UserStore) AddPushSubscription(sub PushSubscription) error {
	if sub.Endpoint == "" || sub.P256dh == "" || sub.Auth == "" {
		return fmt.Errorf("push subscription: endpoint, p256dh and auth are all required")
	}
	_, err := s.db.Exec(
		`INSERT INTO push_subscriptions (endpoint, p256dh, auth, user_agent, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh,
			auth = excluded.auth, user_agent = excluded.user_agent`,
		sub.Endpoint, sub.P256dh, sub.Auth, sub.UserAgent, nowUTC())
	if err != nil {
		return fmt.Errorf("add push subscription: %w", err)
	}
	return nil
}

// RemovePushSubscription removes the device's subscription (unsubscribe).
// A no-op when the endpoint was never registered.
func (s *UserStore) RemovePushSubscription(endpoint string) error {
	if _, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint); err != nil {
		return fmt.Errorf("remove push subscription: %w", err)
	}
	return nil
}

// ListPushSubscriptions returns all of the user's subscribed devices
// (the push fan-out's recipient list).
func (s *UserStore) ListPushSubscriptions() ([]PushSubscription, error) {
	rows, err := s.db.Query(`SELECT endpoint, p256dh, auth, user_agent, created_at FROM push_subscriptions ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth, &p.UserAgent, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("list push subscriptions: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	return out, nil
}

// LatestAssistantReply returns the newest assistant row with content —
// the turn's FINAL reply row (the tool-loop's iteration rows are
// contentless carriers; the reply lands on the last one). The PWA push
// composes its notification body from its first line. Nil when the
// conversation has no assistant reply yet.
func (s *UserStore) LatestAssistantReply(convID string) (*Message, error) {
	var m Message
	err := s.db.QueryRow(
		`SELECT id, conversation_id, role, content, reasoning, reasoning_signature, seq, created_at, model_id, compacted_from, compacted_to
		 FROM messages
		 WHERE conversation_id = ? AND role = 'assistant' AND content <> ''
		 ORDER BY seq DESC LIMIT 1`, convID).
		Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Reasoning, &m.ReasoningSignature, &m.Seq, &m.CreatedAt, &m.ModelID, &m.CompactedFrom, &m.CompactedTo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest assistant reply: %w", err)
	}
	return &m, nil
}
