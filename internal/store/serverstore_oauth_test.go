package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestServerStoreOAuth covers the v3 IdP origin columns: fresh creation,
// lookup by external subject, and the v2→v3 migration (a pre-OAuth store
// gains the columns; existing rows keep their values, origin defaults to
// internal).
func TestServerStoreOAuth(t *testing.T) {
	// Fresh store: an internal + an OAuth user.
	dir := t.TempDir()
	srv, err := OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	ou, err := srv.CreateOAuthUser("alice", "Alice", "auth0|abc123")
	if err != nil {
		t.Fatal(err)
	}
	if ou.IDP != "oauth" || ou.IDPSubject != "auth0|abc123" || ou.Role != "user" || ou.Status != "active" {
		t.Fatalf("created oauth user = %+v", ou)
	}

	got, err := srv.FindOAuthUserBySubject("auth0|abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != "alice" {
		t.Fatalf("found by subject = %q, want alice", got.Handle)
	}
	if _, err := srv.FindOAuthUserBySubject("nope"); err != ErrUserNotFound {
		t.Fatalf("unknown subject err = %v, want ErrUserNotFound", err)
	}

	users, err := srv.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("ListUsers = %d rows, want 2", len(users))
	}
	byHandle := map[string]User{}
	for _, u := range users {
		byHandle[u.Handle] = u
	}
	if byHandle["admin"].IDP != "" {
		t.Fatalf("internal user origin = %q, want empty", byHandle["admin"].IDP)
	}
	if byHandle["alice"].IDP != "oauth" {
		t.Fatalf("oauth user origin = %q, want oauth", byHandle["alice"].IDP)
	}

	// A handle already taken by an internal user is rejected for OAuth use
	// (the server does the collision check; the store enforces the primary
	// key).
	if _, err := srv.CreateOAuthUser("admin", "Clash", "sub-x"); err == nil {
		t.Fatal("expected a conflict creating an oauth user with a taken handle")
	}
}

// TestServerStoreOAuthMigrationV2 builds a v2 store (no idp/idp_subject
// columns, one row) and verifies OpenServer migrates it forward: the
// columns appear, the old row survives with internal origin, and the schema
// version reaches 3.
func TestServerStoreOAuthMigrationV2(t *testing.T) {
	dir := t.TempDir()
	// Write a v2 database directly (the schema as it existed before OAuth).
	raw, err := sql.Open("sqlite", "file:"+dir+"/server.db")
	if err != nil {
		t.Fatal(err)
	}
	v2 := `
CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
	handle       TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	role         TEXT NOT NULL DEFAULT 'user',
	status       TEXT NOT NULL DEFAULT 'active',
	email        TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL
);
INSERT INTO schema_version (version, applied_at) VALUES (1, 'x'), (2, 'x');
INSERT INTO users (handle, display_name, role, status, email, created_at)
VALUES ('legacy', 'Legacy', 'user', 'active', 'legacy@example.com', 'x');
`
	if _, err := raw.Exec(v2); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Open through the store: the migration must run.
	srv, err := OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	u, err := srv.GetUser("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if u.DisplayName != "Legacy" || u.Email != "legacy@example.com" {
		t.Fatalf("migrated row changed: %+v", u)
	}
	if u.IDP != "" || u.IDPSubject != "" {
		t.Fatalf("migrated row defaults = idp %q subject %q, want empty", u.IDP, u.IDPSubject)
	}

	// The new columns work after the migration.
	if _, err := srv.CreateOAuthUser("newbie", "New", "sub-9"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.FindOAuthUserBySubject("sub-9"); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := raw2MaxVersion(t, dir).QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 3 {
		t.Fatalf("schema version = %d, want 3", v)
	}
}

// raw2MaxVersion opens the db file directly for a raw version check.
func raw2MaxVersion(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dir+"/server.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
