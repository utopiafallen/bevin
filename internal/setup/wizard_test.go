package setup

import (
	"bytes"
	"os"
	"strings"
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

// TestWizardServerStepSandboxGate — regression guard: the mandatory
// container check lives on the SERVER step (step 1) and gates its Next
// button: renderServer shows the sandbox section, stepReady("server")
// requires the runtime check to pass, and the sidecars step no longer
// shows the section.
func TestWizardServerStepSandboxGate(t *testing.T) {
	b, err := os.ReadFile("wizard.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// stepReady: the server step gates on the runtime check.
	if !strings.Contains(s, "case \"server\": {") || !strings.Contains(s, "S.sandboxStatus && S.sandboxStatus.runtime") {
		t.Fatalf("stepReady no longer gates the server step on the container runtime check")
	}
	// renderServer shows the sandbox section ...
	i, j := strings.Index(s, "function renderServer()"), strings.Index(s, "function renderAdmin()")
	if i < 0 || j < 0 || j < i {
		t.Fatal("cannot locate renderServer/renderAdmin in the wizard")
	}
	if !strings.Contains(s[i:j], "renderSandboxSection()") {
		t.Fatal("renderServer no longer shows the per-user container (sandbox) section")
	}
	// ... and the sidecars step no longer does.
	k, l := strings.Index(s, "function renderSidecars()"), strings.Index(s, "function renderSandboxSection()")
	if k < 0 || l < 0 || l < k {
		t.Fatal("cannot locate renderSidecars/renderSandboxSection in the wizard")
	}
	if strings.Contains(s[k:l], "renderSandboxSection()") {
		t.Fatal("the sidecars step still shows the sandbox section (it moved to the server step)")
	}
	// The server step polls the check while on it (work-gate + streaming).
	if !strings.Contains(s, "setInterval(pollSandbox, 1000)") {
		t.Fatal("the server step no longer polls the container check")
	}
}
