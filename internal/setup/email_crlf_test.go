package setup

import (
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
)

// TestValidateEmailRejectsControlChars pins the From-header gate (security
// 2026-11-20): the display name + address are written verbatim into the sent
// message, so a CRLF there would inject arbitrary headers (Bcc/Cc/Subject).
// The username-derived from address (no @) must STAY legal — it is the
// documented placeholder, and MAIL FROM is %q-escaped by net/smtp.
func TestValidateEmailRejectsControlChars(t *testing.T) {
	base := config.Email{Enabled: true, SMTPHost: "smtp.example.com", SMTPPort: 587, FromAddress: "bevin@example.com", FromName: "Bevin"}
	if _, err := validateEmail(base); err != nil {
		t.Fatalf("a clean email must validate: %v", err)
	}
	crlfName := base
	crlfName.FromName = "Bevin\r\nBcc: all@users"
	if _, err := validateEmail(crlfName); err == nil {
		t.Fatal("a CRLF in the from name must be rejected")
	}
	crlfAddr := base
	crlfAddr.FromAddress = "bevi\r\nn@example.com"
	if _, err := validateEmail(crlfAddr); err == nil {
		t.Fatal("a CRLF in the from address must be rejected")
	}
	// The placeholder path: no from address, the username stands in.
	placeholder := config.Email{Enabled: true, SMTPHost: "smtp.example.com", SMTPPort: 587, Username: "bevin-smtp"}
	if _, err := validateEmail(placeholder); err != nil {
		t.Fatalf("a username-derived from address must stay valid: %v", err)
	}
}
