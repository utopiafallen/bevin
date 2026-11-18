package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestPrivilegeCheck — the startup privilege gate: the check and the
// refusal message must agree with the platform (an actionable refusal is
// the whole point — it names the required privilege, the runtime it is
// for, and the dev escape hatch).
func TestPrivilegeCheck(t *testing.T) {
	switch runtime.GOOS {
	case "linux":
		if privilegeOK() != (os.Geteuid() == 0) {
			t.Fatal("privilegeOK() disagrees with geteuid on linux")
		}
		for _, want := range []string{"root", "systemd-nspawn", "-allow-unprivileged"} {
			if !strings.Contains(privilegeMessage(), want) {
				t.Fatalf("linux privilege message missing %q: %s", want, privilegeMessage())
			}
		}
	case "windows":
		for _, want := range []string{"Administrator", "Host Compute Service", "-allow-unprivileged"} {
			if !strings.Contains(privilegeMessage(), want) {
				t.Fatalf("windows privilege message missing %q: %s", want, privilegeMessage())
			}
		}
	default:
		if !privilegeOK() {
			t.Fatal("no privilege requirement on platforms without a container backend")
		}
	}
}
