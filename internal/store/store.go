// Package store persists Bevin's local state in SQLite (pure-Go driver, no
// cgo). There are two stores:
//
//   - ServerStore: server-level state in server.db (user registry S1,
//     credentials, auth sessions).
//   - UserStore: one user's learned state in users/<handle>/bevin.db
//     (conversations, messages, memories, tasks, skills, workspaces, profile).
//
// One SQLite file per user gives structural isolation. Both run in WAL mode
// with short transactions (N parallel task loops + a foreground session write
// to the same per-user DB).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const currentSchemaVersion = 1

// UserStore is a single user's learned state (users/<handle>/bevin.db).
type UserStore struct {
	db  *sql.DB
	dir string
}

// OpenUser opens (creating if needed) a user's bevin.db under dir.
func OpenUser(dir string) (*UserStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "bevin.db"))
	if err != nil {
		return nil, err
	}
	if err := configure(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &UserStore{db: db, dir: dir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the user store.
func (s *UserStore) Close() error { return s.db.Close() }

func configure(db *sql.DB) error {
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(p); err != nil {
			return err
		}
	}
	return nil
}

// nowUTC is the canonical stored timestamp (RFC3339, UTC).
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// newID returns a random 24-hex-char id for TEXT-PK entities.
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- embedding helpers (512-dim MRL stored as little-endian float32) ----

func embedBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func blobEmbed(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// cosine returns the cosine similarity of two equal-length vectors (0 if
// empty or length-mismatched).
func cosine(a, b []float32) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// migrate creates the per-user schema (idempotent) and records the version.
func (s *UserStore) migrate() error {
	if _, err := s.db.Exec(userSchema); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO schema_version (version, applied_at) VALUES (?, ?)`,
		currentSchemaVersion, nowUTC())
	return err
}

// userSchema is the full per-user schema (P1–P13, with FTS5).
const userSchema = `
CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);

-- P2 conversation
CREATE TABLE IF NOT EXISTS conversations (
	id                    TEXT PRIMARY KEY,
	title                 TEXT NOT NULL DEFAULT '',
	created_at            TEXT NOT NULL,
	last_activity_at      TEXT NOT NULL,
	state                 TEXT NOT NULL DEFAULT 'active',
	frozen_system_prompt  TEXT NOT NULL DEFAULT '',
	prompt_input_versions TEXT NOT NULL DEFAULT '',
	owner_task_id         TEXT,
	active_model_id       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conv_task ON conversations(owner_task_id);
CREATE INDEX IF NOT EXISTS idx_conv_model ON conversations(active_model_id);
CREATE INDEX IF NOT EXISTS idx_conv_state ON conversations(state);

-- P3 message (atomic unit of truth; embedded + FTS5)
CREATE TABLE IF NOT EXISTS messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	role            TEXT NOT NULL,
	content         TEXT NOT NULL DEFAULT '',
	seq             INTEGER NOT NULL,
	created_at      TEXT NOT NULL,
	model_id        TEXT,
	embedding       BLOB
);
CREATE INDEX IF NOT EXISTS idx_msg_conv_seq ON messages(conversation_id, seq);
CREATE INDEX IF NOT EXISTS idx_msg_model ON messages(model_id);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	content,
	content='messages', content_rowid='id',
	tokenize='porter unicode61'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.id, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE OF content ON messages BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.id, old.content);
	INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

-- P5 memory (embedded + FTS5; superseded, not deleted)
CREATE TABLE IF NOT EXISTS memories (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	facet         TEXT NOT NULL,
	statement     TEXT NOT NULL,
	subject       TEXT,
	confidence    REAL NOT NULL DEFAULT 1.0,
	status        TEXT NOT NULL DEFAULT 'active',
	superseded_by INTEGER REFERENCES memories(id),
	provenance    TEXT,
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL,
	embedding     BLOB
);
CREATE INDEX IF NOT EXISTS idx_mem_facet ON memories(facet);
CREATE INDEX IF NOT EXISTS idx_mem_status ON memories(status);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
	statement, facet,
	content='memories', content_rowid='id',
	tokenize='porter unicode61'
);
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
	INSERT INTO memories_fts(rowid, statement, facet) VALUES (new.id, new.statement, new.facet);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
	INSERT INTO memories_fts(memories_fts, rowid, statement, facet) VALUES ('delete', old.id, old.statement, old.facet);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
	INSERT INTO memories_fts(memories_fts, rowid, statement, facet) VALUES ('delete', old.id, old.statement, old.facet);
	INSERT INTO memories_fts(rowid, statement, facet) VALUES (new.id, new.statement, new.facet);
END;

-- P5 provenance (message <-> memory, N:M)
CREATE TABLE IF NOT EXISTS memory_provenance (
	memory_id  INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	PRIMARY KEY (memory_id, message_id)
);

-- P6 task
CREATE TABLE IF NOT EXISTS tasks (
	id              TEXT PRIMARY KEY,
	title           TEXT NOT NULL,
	description     TEXT NOT NULL DEFAULT '',
	state           TEXT NOT NULL DEFAULT 'todo',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL,
	conversation_id TEXT REFERENCES conversations(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_task_state ON tasks(state);
CREATE INDEX IF NOT EXISTS idx_task_conv ON tasks(conversation_id);

-- P6 depends_on (0..N parents; strictly directed acyclic)
CREATE TABLE IF NOT EXISTS task_dependencies (
	task_id   TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	PRIMARY KEY (task_id, parent_id)
);

-- P7 tool call
CREATE TABLE IF NOT EXISTS tool_calls (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	tool       TEXT NOT NULL,
	args       TEXT NOT NULL DEFAULT '{}',
	result     TEXT NOT NULL DEFAULT '',
	success    INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tc_msg ON tool_calls(message_id);

-- P9 audit event
CREATE TABLE IF NOT EXISTS audit_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL,
	detail     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

-- P10 skill (user-global; workspace skills live on-disk in .skills/)
CREATE TABLE IF NOT EXISTS skills (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	content     TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS skill_memory_refs (
	skill_id  INTEGER NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
	memory_id INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
	PRIMARY KEY (skill_id, memory_id)
);

-- P11 workspace
CREATE TABLE IF NOT EXISTS workspaces (
	id          TEXT PRIMARY KEY,
	title       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	agent_notes TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	size        INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL DEFAULT 'active',
	folder_path TEXT NOT NULL,
	embedding   BLOB
);

-- P12 workspace file
CREATE TABLE IF NOT EXISTS workspace_files (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	name          TEXT NOT NULL,
	relative_path TEXT NOT NULL,
	mime          TEXT,
	size          INTEGER NOT NULL DEFAULT 0,
	hash          TEXT,
	added_at      TEXT NOT NULL,
	source        TEXT NOT NULL DEFAULT 'user',
	kind          TEXT NOT NULL DEFAULT 'doc',
	ingested      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_wf_ws ON workspace_files(workspace_id);

-- P1 profile (durable, small, injected into every system prompt)
CREATE TABLE IF NOT EXISTS profile (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	source     TEXT NOT NULL DEFAULT 'learned',
	updated_at TEXT NOT NULL
);
`