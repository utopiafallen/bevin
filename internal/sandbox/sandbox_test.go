package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPathHelpers(t *testing.T) {
	dataDir := "/data/bevin"
	if got := SandboxRoot(dataDir); got != filepath.Join(dataDir, "sandboxes") {
		t.Fatalf("SandboxRoot = %q", got)
	}
	if got := BaseDir(dataDir); got != filepath.Join(dataDir, "sandboxes", "base") {
		t.Fatalf("BaseDir = %q", got)
	}
	if got := BaseRoot(dataDir); got != filepath.Join(dataDir, "sandboxes", "base", "root") {
		t.Fatalf("BaseRoot = %q", got)
	}
	u := UserSandboxDir(dataDir, "alice")
	if !strings.HasPrefix(u, filepath.Join(dataDir, "sandboxes")) {
		t.Fatalf("UserSandboxDir = %q", u)
	}
	if got := UserDataDir(dataDir, "alice"); got != filepath.Join(dataDir, "users", "alice") {
		t.Fatalf("UserDataDir = %q", got)
	}
	// The user's own data dir is what gets mapped INTO the container.
	if got := UserDataDir(dataDir, "alice"); got != filepath.Join(dataDir, "users", "alice") {
		t.Fatalf("UserDataDir = %q", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	var c Config
	if c.Network != "" {
		t.Fatalf("zero-config Network = %q, want empty (defaults to veth)", c.Network)
	}
	if c.Enabled {
		t.Fatal("zero-config Enabled must be false (opt-in)")
	}
}
