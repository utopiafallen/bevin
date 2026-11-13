package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ServerStore is server-level state (server.db): the user registry (S1),
// credentials, and auth sessions. It is distinct from any user's per-user
// store.
type ServerStore struct {
	db  *sql.DB
	dir string
}

// OpenServer opens (creating if needed) server.db under dir.
func OpenServer(dir string) (*ServerStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dir, "server.db")))
	if err != nil {
		return nil, err
	}
	s := &ServerStore{db: db, dir: dir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate server: %w", err)
	}
	return s, nil
}

// Close closes the server store.
func (s *ServerStore) Close() error { return s.db.Close() }

// ErrUserNotFound is returned when a handle has no account.
var ErrUserNotFound = errors.New("user not found")

func (s *ServerStore) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);
-- S1 user account
CREATE TABLE IF NOT EXISTS users (
	handle       TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	role         TEXT NOT NULL DEFAULT 'user',
	status       TEXT NOT NULL DEFAULT 'active',
	email        TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL
);
-- internal auth: password hash per user (argon2id)
CREATE TABLE IF NOT EXISTS credentials (
	handle        TEXT PRIMARY KEY,
	password_hash TEXT NOT NULL,
	created_at    TEXT NOT NULL
);
-- P8 auth session (server-scoped)
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	handle     TEXT NOT NULL REFERENCES users(handle) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	last_seen  TEXT NOT NULL,
	user_agent TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_handle ON sessions(handle);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// v1: initial schema.
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO schema_version (version, applied_at) VALUES (?, ?)`, 1, nowUTC()); err != nil {
		return err
	}
	// v2: add the email column (existing installs; fresh installs already have
	// it from the schema above).
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	if v < 2 {
		var hasCol int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'email'`).Scan(&hasCol)
		if hasCol == 0 {
			if _, err := s.db.Exec(`ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO schema_version (version, applied_at) VALUES (?, ?)`, 2, nowUTC()); err != nil {
			return err
		}
	}
	return nil
}

// User is an S1 account row.
type User struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"` // admin | user
	Status      string `json:"status"`
	Email       string `json:"email"`
	CreatedAt   string `json:"created_at"`
}

// CreateUser inserts a new account. Fails if the handle already exists.
func (s *ServerStore) CreateUser(handle, displayName, role string) (*User, error) {
	now := nowUTC()
	res, err := s.db.Exec(
		`INSERT INTO users (handle, display_name, role, status, created_at) VALUES (?,?,?,'active',?)`,
		handle, displayName, role, now)
	if err != nil {
		return nil, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil, fmt.Errorf("user %q not created", handle)
	}
	return &User{Handle: handle, DisplayName: displayName, Role: role, Status: "active", CreatedAt: now}, nil
}

// GetUser returns the account for a handle.
func (s *ServerStore) GetUser(handle string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(
		`SELECT handle, display_name, role, status, email, created_at FROM users WHERE handle = ?`,
		handle).Scan(&u.Handle, &u.DisplayName, &u.Role, &u.Status, &u.Email, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ListUsers returns all accounts.
func (s *ServerStore) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT handle, display_name, role, status, email, created_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Handle, &u.DisplayName, &u.Role, &u.Status, &u.Email, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserEmail sets (or clears) a user's email address ("" clears it).
func (s *ServerStore) SetUserEmail(handle, emailAddr string) error {
	res, err := s.db.Exec(`UPDATE users SET email = ? WHERE handle = ?`, emailAddr, handle)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetUserStatus sets active | disabled.
func (s *ServerStore) SetUserStatus(handle, status string) error {
	res, err := s.db.Exec(`UPDATE users SET status = ? WHERE handle = ?`, status, handle)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetCredential stores (or replaces) the argon2id password hash for a user.
func (s *ServerStore) SetCredential(handle, passwordHash string) error {
	_, err := s.db.Exec(
		`INSERT INTO credentials (handle, password_hash, created_at) VALUES (?,?,?)
		 ON CONFLICT(handle) DO UPDATE SET password_hash = excluded.password_hash`,
		handle, passwordHash, nowUTC())
	return err
}

// PasswordHash returns the stored hash for a user ("" if none).
func (s *ServerStore) PasswordHash(handle string) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT password_hash FROM credentials WHERE handle = ?`, handle).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}

// Session is a P8 auth session.
type Session struct {
	Token     string `json:"token"`
	Handle    string `json:"handle"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	LastSeen  string `json:"last_seen"`
	UserAgent string `json:"user_agent"`
}

// CreateSession stores a new session token for a user with a TTL.
func (s *ServerStore) CreateSession(token, handle string, ttl time.Duration, userAgent string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, handle, created_at, expires_at, last_seen, user_agent) VALUES (?,?,?,?,?,?)`,
		token, handle, now.Format(time.RFC3339Nano), now.Add(ttl).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), userAgent)
	return err
}

// GetSession returns a live (non-expired) session, or ErrSessionNotFound.
func (s *ServerStore) GetSession(token string) (*Session, error) {
	sess := &Session{}
	err := s.db.QueryRow(
		`SELECT token, handle, created_at, expires_at, last_seen, user_agent FROM sessions WHERE token = ?`,
		token).Scan(&sess.Token, &sess.Handle, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeen, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	exp, perr := time.Parse(time.RFC3339Nano, sess.ExpiresAt)
	if perr != nil {
		return nil, perr
	}
	if time.Now().UTC().After(exp) {
		s.DeleteSession(token)
		return nil, ErrSessionNotFound
	}
	return sess, nil
}

// ErrSessionNotFound is returned for an unknown or expired session token.
var ErrSessionNotFound = errors.New("session not found")

// DeleteSession removes a session by token.
func (s *ServerStore) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// PruneSessions removes expired sessions.
func (s *ServerStore) PruneSessions() (int, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}