package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// v1File writes a bevin.db stamped at schema v1 — the pre-migration layout:
// the base DDL only (no purpose columns, no agent_messages table).
func v1File(t *testing.T, dir string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bevin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(userSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
}

func schemaVersions(t *testing.T, dir string) []int {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "bevin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func conversationColumns(t *testing.T, dir string) map[string]bool {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "bevin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT name FROM pragma_table_info('conversations')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}

// checkPostMigration asserts the v3 invariants: all three versions recorded,
// the purpose columns present, agent_messages usable.
func checkPostMigration(t *testing.T, dir string) {
	t.Helper()
	versions := schemaVersions(t, dir)
	if len(versions) != 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Fatalf("schema versions = %v, want [1 2 3]", versions)
	}
	cols := conversationColumns(t, dir)
	for _, c := range []string{"purpose", "topic", "purpose_updated_at"} {
		if !cols[c] {
			t.Fatalf("conversations missing column %q (columns: %v)", c, cols)
		}
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "bevin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM agent_messages`).Scan(&n); err != nil {
		t.Fatalf("agent_messages not queryable: %v", err)
	}
}

// TestMigrationsFreshDB: a brand-new file gets the base version plus every
// pending step, in one open.
func TestMigrationsFreshDB(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkPostMigration(t, dir)
}

// TestMigrationsUpgradeV1File: a v1 file from an old build is migrated forward
// in place; a pre-upgrade conversation row picks up the defaults.
func TestMigrationsUpgradeV1File(t *testing.T) {
	dir := t.TempDir()
	v1File(t, dir)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "bevin.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO conversations (id, title, created_at, last_activity_at, state, active_model_id) VALUES ('v1conv', 'old', 'x', 'x', 'active', 'm1')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkPostMigration(t, dir)

	c, err := s.GetConversation("v1conv")
	if err != nil {
		t.Fatal(err)
	}
	if c.Purpose != "" || c.Topic != "" || c.PurposeUpdatedAt != "" {
		t.Fatalf("pre-upgrade conversation should have empty purpose fields, got %+v", c)
	}
	if err := s.SetConversationSummary("v1conv", "auth refactor", "session store"); err != nil {
		t.Fatal(err)
	}
	c2, err := s.GetConversation("v1conv")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Purpose != "auth refactor" || c2.Topic != "session store" || c2.PurposeUpdatedAt == "" {
		t.Fatalf("summary not persisted: %+v", c2)
	}
}

// TestMigrationsIdempotentReopen: opening an already-migrated file a second
// time applies nothing new (the steps are recorded, not re-run).
func TestMigrationsIdempotentReopen(t *testing.T) {
	dir := t.TempDir()
	v1File(t, dir)
	s1, err := OpenUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	checkPostMigration(t, dir)
}
