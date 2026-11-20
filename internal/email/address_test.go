package email

import (
	"context"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
)

// TestValidAddress pins the address gate (security 2026-11-20): the To:
// header is written verbatim into the sent message, so CRLF (and the other
// header metacharacters) must never reach the store — a CRLF address would
// inject arbitrary headers (Bcc/Cc/Subject) when bevin notifies the user.
func TestValidAddress(t *testing.T) {
	ok := []string{
		"a@b.c",
		"admin@example.com",
		"user+tag@example.co.uk",
		"first.last@sub.domain.com",
		strings.Repeat("a", 64) + "@example.com", // local at the RFC limit
	}
	for _, a := range ok {
		if !ValidAddress(a) {
			t.Errorf("ValidAddress(%q) = false, want true", a)
		}
	}
	bad := []string{
		"",
		"no-at-sign",
		"@b.c",
		"a@",
		"two@@example.com",
		"user name@example.com", // space
		"a<b@c.d",               // bracket
		`a"b@c.d`,               // quote
		"evil@example.com\r\nBcc: leak@evil.com",
		"evil@example.com\nBcc: leak@evil.com",
		"evil@example.com\rBcc: leak@evil.com",
		"evil@example.com\x7fBcc: x",
		strings.Repeat("a", 65) + "@example.com", // local over the RFC limit
	}
	for _, a := range bad {
		if ValidAddress(a) {
			t.Errorf("ValidAddress(%q) = true, want false", a)
		}
	}
}

// TestSendRejectsHeaderInjection pins the Send guards: they fire BEFORE the
// dial (an unreachable host is never reached), so a poisoned recipient or a
// control-char from address/name is refused instead of sent.
func TestSendRejectsHeaderInjection(t *testing.T) {
	s := New(config.Email{Enabled: true, SMTPHost: "127.0.0.1", SMTPPort: 1, FromAddress: "bevin@example.com"})
	if s == nil {
		t.Fatal("expected a configured sender")
	}
	if err := s.Send(context.Background(), "evil@example.com\r\nBcc: leak@evil.com", "s", "b"); err == nil || !strings.Contains(err.Error(), "invalid recipient") {
		t.Fatalf("CRLF recipient: err = %v, want the invalid-recipient guard (before the dial)", err)
	}
	badFrom := &Sender{cfg: config.Email{Enabled: true, SMTPHost: "127.0.0.1", SMTPPort: 1, FromAddress: "bevi\r\nn@example.com"}}
	if err := badFrom.Send(context.Background(), "ok@example.com", "s", "b"); err == nil || !strings.Contains(err.Error(), "from address") {
		t.Fatalf("CRLF from address: err = %v, want the from-address guard", err)
	}
	badName := &Sender{cfg: config.Email{Enabled: true, SMTPHost: "127.0.0.1", SMTPPort: 1, FromAddress: "b@example.com", FromName: "B\r\ncc: x"}}
	if err := badName.Send(context.Background(), "ok@example.com", "s", "b"); err == nil || !strings.Contains(err.Error(), "from name") {
		t.Fatalf("CRLF from name: err = %v, want the from-name guard", err)
	}
}
