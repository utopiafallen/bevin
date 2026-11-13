package setup

import (
	"context"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
)

// TestNormalizeOAuth — the optional external-IdP step validation (the server
// is the source of truth the wizard mirrors).
func TestNormalizeOAuth(t *testing.T) {
	// Disabled: passes through untouched (no fields required).
	d, err := normalizeOAuth(config.OAuth{Enabled: false})
	if err != nil {
		t.Fatalf("disabled oauth must pass, got %v", err)
	}
	if d.Enabled {
		t.Fatal("enabled flag should stay false")
	}

	// Enabled + complete: defaults applied (name "OAuth", scopes).
	full := config.OAuth{
		Enabled: true, ClientID: "app-1", ClientSecret: "s3cret",
		AuthURL: "https://idp.example.com/o/authorize/", TokenURL: "https://idp.example.com/o/token/",
		UserinfoURL: "https://idp.example.com/o/user/",
	}
	n, err := normalizeOAuth(full)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "OAuth" || n.Scopes != "openid profile email" {
		t.Fatalf("defaults not applied: name=%q scopes=%q", n.Name, n.Scopes)
	}

	// Missing each required field -> error.
	missing := func(mut func(*config.OAuth)) {
		o := full
		o.Name = "authentik" // avoid the name default so only the field under test is missing
		mut(&o)
		if _, err := normalizeOAuth(o); err == nil {
			t.Errorf("expected an error for %+v", o)
		}
	}
	missing(func(o *config.OAuth) { o.ClientID = "" })
	missing(func(o *config.OAuth) { o.AuthURL = "" })
	missing(func(o *config.OAuth) { o.TokenURL = "" })
	missing(func(o *config.OAuth) { o.UserinfoURL = "" })

	// Non-http(s) / relative URLs are rejected.
	bad := full
	bad.AuthURL = "ftp://idp.example.com/"
	if _, err := normalizeOAuth(bad); err == nil {
		t.Fatal("expected an error for a non-http(s) auth URL")
	}
	bad = full
	bad.AuthURL = "/o/authorize/"
	if _, err := normalizeOAuth(bad); err == nil {
		t.Fatal("expected an error for a relative auth URL")
	}

	// An explicit scopes value is preserved.
	s := full
	s.Scopes = "openid email"
	n, _ = normalizeOAuth(s)
	if n.Scopes != "openid email" {
		t.Fatalf("explicit scopes should be preserved, got %q", n.Scopes)
	}
}

// TestSanitizeHandle — provider text -> a valid bevin handle.
func TestSanitizeHandle(t *testing.T) {
	cases := map[string]string{
		"Alice":      "alice",
		"alice.bob":  "alice.bob",
		"__lead":     "lead", // leading punctuation dropped
		"UPPER_CASE": "upper_case",
		"sp ace":     "space",
		"a@x.com":    "ax.com", // SanitizeHandle strips disallowed chars (the email LOCAL PART is split on @ by the caller)
		"short":      "short",
		"":           "",
		"!!":         "",
		"a":          "", // <2 chars
	}
	for in, want := range cases {
		if got := SanitizeHandle(in); got != want {
			t.Errorf("SanitizeHandle(%q) = %q, want %q", in, got, want)
		}
	}
	// Long input is truncated to 32.
	long := SanitizeHandle("abcdefghijklmnopqrstuvwxyz0123456789")
	if len(long) != 32 {
		t.Fatalf("SanitizeHandle truncation = %d chars, want 32", len(long))
	}
}

// TestValidHandle — the handle pattern (same as the admin step).
func TestValidHandle(t *testing.T) {
	if !ValidHandle("alice") || !ValidHandle("a1.b-c_d") {
		t.Fatal("valid handles rejected")
	}
	for _, bad := range []string{"", "a", "Alice", "1a-b-c-d-e-f-g-h-i-j-k-l-m-n-o-p-2", "-lead", "a b"} {
		if ValidHandle(bad) {
			t.Errorf("ValidHandle(%q) = true, want false", bad)
		}
	}
}

// TestCommitEditOAuth — the post-setup oauth area: a save with an oauth area
// replaces it, leaves the sibling areas unchanged, persists, and a blank
// secret replacement is the caller's job (CommitEdit stores what it is
// given; the admin endpoint preserves a blank secret).
func TestCommitEditOAuth(t *testing.T) {
	m, _, _ := newTestSetup(t)
	validAnswers(m)
	cfg, err := m.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	oauth := config.OAuth{
		Enabled: true, Name: "authentik", ClientID: "app-1", ClientSecret: "s3cret",
		AuthURL: "https://idp.example.com/o/authorize/", TokenURL: "https://idp.example.com/o/token/",
		UserinfoURL: "https://idp.example.com/o/user/", Scopes: "openid profile email",
	}
	got, err := m.CommitEdit(nil, nil, nil, &oauth, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Auth.OAuth != oauth {
		t.Fatalf("oauth area = %+v, want %+v", got.Auth.OAuth, oauth)
	}
	if got.Email != cfg.Email {
		t.Fatal("email should be preserved on an oauth-only save")
	}
	reloaded, err := config.Load(m.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Auth.OAuth.Enabled || reloaded.Auth.OAuth.ClientID != "app-1" {
		t.Fatalf("oauth save did not persist: %+v", reloaded.Auth.OAuth)
	}

	// Disabling is a normal save.
	off := oauth
	off.Enabled = false
	got, err = m.CommitEdit(nil, nil, nil, &off, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Auth.OAuth.Enabled {
		t.Fatal("oauth should be disabled after the save")
	}

	// An invalid enabled oauth is rejected (and does not clobber the saved one).
	bad := oauth
	bad.TokenURL = "not a url"
	if _, err := m.CommitEdit(nil, nil, nil, &bad, nil); err == nil {
		t.Fatal("expected an error for an invalid oauth URL")
	}
	after, _ := config.Load(m.dataDir)
	if after.Auth.OAuth.Enabled {
		t.Fatal("a rejected oauth save must not clobber the saved one")
	}
}

// TestSetOAuthStep — the wizard's advance path stores + validates the step.
func TestSetOAuthStep(t *testing.T) {
	m, _, _ := newTestSetup(t)
	// Disabled is always accepted.
	if err := m.SetOAuth(config.OAuth{}); err != nil {
		t.Fatalf("disabled oauth step rejected: %v", err)
	}
	// Enabled WITHOUT the server step's public base is rejected (the
	// redirect is derived from it) — even a complete oauth config.
	complete := config.OAuth{
		Enabled: true, Name: "authentik", ClientID: "app-1",
		AuthURL: "https://idp.example.com/o/authorize/", TokenURL: "https://idp.example.com/o/token/",
		UserinfoURL: "https://idp.example.com/o/user/",
	}
	if err := m.SetOAuth(complete); err == nil {
		t.Fatal("expected an error for enabled oauth without the public base URI")
	}
	// With the base set, enabled with a gap is still rejected.
	if err := m.SetServer(ServerAnswers{BaseURI: "https://bevin.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOAuth(config.OAuth{Enabled: true, ClientID: "app"}); err == nil {
		t.Fatal("expected an error for an incomplete enabled oauth")
	}
	// A complete config is stored on the answers.
	full := config.OAuth{
		Enabled: true, Name: "authentik", ClientID: "app-1",
		AuthURL: "https://idp.example.com/o/authorize/", TokenURL: "https://idp.example.com/o/token/",
		UserinfoURL: "https://idp.example.com/o/user/",
	}
	if err := m.SetOAuth(full); err != nil {
		t.Fatal(err)
	}
	a := m.Answers()
	if a.OAuth != full && a.OAuth.Scopes != "openid profile email" {
		t.Fatalf("stored oauth answers = %+v", a.OAuth)
	}
}
