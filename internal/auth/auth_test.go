package auth

import (
	"context"
	"testing"
	"time"

	"github.com/utopiafallen/alcyona-agent/internal/store"
)

func TestArgon2RoundTrip(t *testing.T) {
	enc, err := hashArgon2id("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyArgon2id(enc, "s3cret") {
		t.Fatal("correct password should verify")
	}
	if verifyArgon2id(enc, "wrong") {
		t.Fatal("wrong password should not verify")
	}
	if verifyArgon2id("garbage", "s3cret") {
		t.Fatal("malformed encoded string should not verify")
	}
}

func newTestAuth(t *testing.T) *InternalProvider {
	t.Helper()
	ss, err := store.OpenServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	return NewInternalProvider(ss, time.Hour)
}

func TestInternalProviderFlow(t *testing.T) {
	p := newTestAuth(t)
	ctx := context.Background()

	admin, err := p.CreateUserProvisioned(ctx, "admin", "Admin", "admin", "admin-pass")
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != "admin" {
		t.Fatalf("role = %q", admin.Role)
	}

	// Bad / unknown logins.
	if _, _, err := p.Authenticate(ctx, "admin", "nope"); err != ErrInvalidCredentials {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
	if _, _, err := p.Authenticate(ctx, "ghost", "x"); err != ErrInvalidCredentials {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}

	// Good login.
	u, token, err := p.Authenticate(ctx, "admin", "admin-pass")
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "admin" || token == "" {
		t.Fatalf("login = %+v token=%q", u, token)
	}

	// Resolve token; reject a bad token.
	got, err := p.UserFromToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != "admin" || got.Role != "admin" {
		t.Fatalf("resolved = %+v", got)
	}
	if _, err := p.UserFromToken(ctx, "badtoken"); err != store.ErrSessionNotFound {
		t.Fatalf("want ErrSessionNotFound, got %v", err)
	}

	// Logout invalidates the token.
	if err := p.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UserFromToken(ctx, token); err != store.ErrSessionNotFound {
		t.Fatalf("after logout want ErrSessionNotFound, got %v", err)
	}

	// Change password.
	if err := p.SetPassword(ctx, "admin", "new-pass"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Authenticate(ctx, "admin", "admin-pass"); err != ErrInvalidCredentials {
		t.Fatal("old password should not work after change")
	}
	if _, _, err := p.Authenticate(ctx, "admin", "new-pass"); err != nil {
		t.Fatal("new password should work")
	}

	// A disabled user cannot authenticate.
	if err := p.srv.SetUserStatus("admin", "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Authenticate(ctx, "admin", "new-pass"); err != ErrInvalidCredentials {
		t.Fatal("disabled user should not authenticate")
	}
}