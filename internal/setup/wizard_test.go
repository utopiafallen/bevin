package setup

import (
	"bytes"
	"os"
	"testing"
)

// TestWizardEmailPortCoerced — regression guard (6f845c6): the wizard's
// advance payload must send email.smtp_port as a JSON NUMBER
// (config.Email.SMTPPort is an int). A string 400s the decode and breaks
// EVERY step's Next button, not just the email step.
func TestWizardEmailPortCoerced(t *testing.T) {
	b, err := os.ReadFile("wizard.html")
	if err != nil {
		t.Fatal(err)
	}
	// The emailPayload construction must coerce the port to a number.
	if !bytes.Contains(b, []byte(`smtp_port: parseInt(S.email.smtp_port, 10)`)) {
		t.Fatalf("wizard emailPayload does not coerce smtp_port to a number — /setup/advance would 400 on every step")
	}
}
