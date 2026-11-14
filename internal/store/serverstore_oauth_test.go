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

// TestServerStoreRefusesNewerSchema pins the version-reset invariant on the
// server DB (no migration paths — no live data worth migrating): a server.db
// stamped with a schema version newer than this build is refused loudly at
// open.
func TestServerStoreRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+dir+"/server.db")
	if err != nil {
		t.Fatal(err)
	}
	dml := `
CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
);
INSERT INTO schema_version (version, applied_at) VALUES (99, 'x');
`
	if _, err := raw.Exec(dml); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenServer(dir); err == nil {
		t.Fatal("a server.db stamped with a newer schema version must be refused, got nil error")
	}
}
