package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Per-user explicit state: small key/value rows that record facts about
// the user (not a conversation) independently of any conversation's
// existence. Onboarding completion lives here — the "Meet Bevin"
// conversation is a payload the user may delete, and the agent.toml flag
// is a separate file that can drift from this store; the state row is the
// single source of truth, in the same database as the payload it
// describes.
//
// The writer contract mirrors UserStore.AfterAppend: the caller owns the
// call (one at a time per store in practice), and the values are opaque
// strings — the keys are constants below, not free text.

// UserStateKeyOnboardingDone is set ("1") once the user completed the
// first-time "Meet Bevin" flow.
const UserStateKeyOnboardingDone = "onboarding_complete"

// UserStateKeyOnboardingConv holds the id of the seeded "Meet Bevin"
// conversation. "" = the flow is complete but the conversation no longer
// exists (the user deleted it — it is deliberately NOT re-seeded; the
// state, not the artifact, is the truth).
const UserStateKeyOnboardingConv = "onboarding_conversation"

// GetUserState returns the value for key (ok = false when unset).
func (s *UserStore) GetUserState(key string) (value string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM user_state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get user state %q: %w", key, err)
	}
	return value, true, nil
}

// SetUserState upserts key.
func (s *UserStore) SetUserState(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("set user state %q: %w", key, err)
	}
	return nil
}
