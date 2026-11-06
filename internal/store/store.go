// Package store persists Alcyona's local state in a single SQLite file.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store is the local persistence layer.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "alcyona.db"))
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// User is a row in the users table.
type User struct {
	ID     int64
	Handle string
}

// GetOrCreateUser returns the user with the given handle, creating it if new.
func (s *Store) GetOrCreateUser(handle string) (*User, error) {
	var u User
	err := s.db.QueryRow(
		`INSERT INTO users (handle) VALUES (?)
		 ON CONFLICT(handle) DO UPDATE SET handle = excluded.handle
		 RETURNING id, handle`, handle,
	).Scan(&u.ID, &u.Handle)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// TODO(learning): per-user memory read/write over the memories table goes here.

// migrate creates the initial schema. Later migrations go here, in order.
func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY,
	handle     TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS conversations (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id),
	title      TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS messages (
	id              INTEGER PRIMARY KEY,
	conversation_id INTEGER NOT NULL REFERENCES conversations(id),
	role            TEXT NOT NULL CHECK (role IN ('system','user','assistant','tool')),
	content         TEXT NOT NULL,
	created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS memories (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id),
	facet      TEXT NOT NULL,  -- which aspect of the user this records
	content    TEXT NOT NULL,
	confidence REAL NOT NULL DEFAULT 1.0,
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conversation_id);
CREATE INDEX IF NOT EXISTS idx_memories_user ON memories(user_id);
`
	_, err := s.db.Exec(schema)
	return err
}
