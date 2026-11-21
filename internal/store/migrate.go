package store

import (
	"database/sql"
	"fmt"
)

// A migration is one numbered schema step on top of the flat base (v1) DDL.
// Steps run in version order, each in a single transaction (its DDL and its
// schema_version record commit or roll back together), so each runs at most
// once and a failing step retries on the next open.
//
// The old no-migrations rule is lifted (setup phase done): schema now grows
// forward via this runner. A file stamped NEWER than this build is still
// refused — bevin never downgrades state, only forwards.
type migration struct {
	version int
	up      string // DDL, applied as one transaction
}

// userMigrations is the ordered list of numbered steps for the per-user
// store; the base v1 DDL (userSchema) creates everything else.
var userMigrations = []migration{
	{
		// v2: rolling-purpose columns (concurrent-instances design, layer 2).
		// The conversation's self-reported purpose/topic — the one-line "what
		// is this conversation doing" the roster shows to its siblings.
		// Updated by the summary nudge, surfaced as the roster's purpose field.
		version: 2,
		up: `
ALTER TABLE conversations ADD COLUMN purpose TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN topic TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN purpose_updated_at TEXT NOT NULL DEFAULT '';
`,
	},
	{
		// v3: agent-to-agent message queue (layer 4). A message one
		// conversation sends to another; delivered at the recipient's next
		// user turn. delivered_at NULL = still queued.
		version: 3,
		up: `
CREATE TABLE IF NOT EXISTS agent_messages (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	from_conversation TEXT NOT NULL,
	to_conversation   TEXT NOT NULL,
	text              TEXT NOT NULL,
	created_at        TEXT NOT NULL,
	delivered_at      TEXT
);
CREATE INDEX IF NOT EXISTS idx_am_to ON agent_messages(to_conversation, delivered_at);
`,
	},
	{
		// v4 (2026-11-20): tasks.origin_conversation_id — the conversation
		// a task was created from (the minimal task system, tier 2): its
		// completion summary is posted there (the audit trail). NULL = a
		// harness task with no origin (the summary stays in the task's own
		// conversation).
		version: 4,
		up: `
ALTER TABLE tasks ADD COLUMN origin_conversation_id TEXT REFERENCES conversations(id) ON DELETE SET NULL;
`,
	},
	{
		// v5 (2026-11-21): task model targeting + the curation range (the
		// CURATION TASK design). tasks.model = an EXPLICIT model target
		// (the task model rule: no explicit target → any available main
		// model, resolved at dispatch; the origin never implies one).
		// memory_start/memory_end = the curation task's CLOSED range of
		// memory ids (fixed at spawn — parallel growth mid-run waits for
		// the next pass); the watermark is the max memory_end of DONE
		// curation tasks. NULL on non-curation tasks.
		version: 5,
		up: `
ALTER TABLE tasks ADD COLUMN model TEXT;
ALTER TABLE tasks ADD COLUMN memory_start INTEGER;
ALTER TABLE tasks ADD COLUMN memory_end INTEGER;
`,
	},
}

// currentSchemaVersion is the schema this build produces: the base (v1) plus
// one version per migration step.
var currentSchemaVersion = 1 + len(userMigrations)

// migrateSchema runs the idempotent base DDL, refuses a file stamped by a
// newer build, then applies the numbered migrations this build has pending
// (file version below the step's). A fresh file records the base version too,
// so schema_version always starts at 1.
func migrateSchema(db *sql.DB, baseDDL string, migrations []migration, label string) error {
	if _, err := db.Exec(baseDDL); err != nil {
		return err
	}
	cur := 1 + len(migrations)
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	if v > cur {
		return fmt.Errorf("%s was written by a newer bevin build (schema v%d > v%d) — delete the data dir or upgrade bevin", label, v, cur)
	}
	// Record the base version (a fresh file has no rows at all).
	if _, err := db.Exec(`INSERT OR IGNORE INTO schema_version (version, applied_at) VALUES (1, ?)`, nowUTC()); err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= v {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("applying migration v%d: %w", m.version, err)
		}
	}
	return nil
}

// applyMigration runs one numbered step and its version record in a single
// transaction: either the DDL and the record land together, or neither does.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(m.up); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, m.version, nowUTC()); err != nil {
		return err
	}
	return tx.Commit()
}
