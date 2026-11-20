package store

import "testing"

// TestSetUserEmailRejectsInjection pins the store-level backstop (security
// 2026-11-20): an address that fails email.ValidAddress (CRLF header
// injection, a missing/doubled @, spaces, control chars) is rejected before
// it reaches the DB. Every HTTP entry point validates too (400) — this is
// the last line for current and future writers.
func TestSetUserEmailRejectsInjection(t *testing.T) {
	dir := t.TempDir()
	srv, err := OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"evil@example.com\r\nBcc: leak@evil.com",
		"evil@example.com\nSubject: hijacked",
		"evil@example.com\t",
		"no-at-sign",
		"@example.com",
		"user@",
		"two@@example.com",
		"user name@example.com",
	}
	for _, a := range bad {
		if err := srv.SetUserEmail("admin", a); err == nil {
			t.Errorf("SetUserEmail(%q) should be rejected", a)
		}
	}
	// A valid address + a clear still work.
	if err := srv.SetUserEmail("admin", "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetUserEmail("admin", ""); err != nil {
		t.Fatal(err)
	}
}
