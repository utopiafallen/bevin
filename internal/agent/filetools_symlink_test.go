package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileEditSymlinkContainment pins the symlink re-check (security
// 2026-11-20): the lexical path checks can't see a link inside the
// workspace pointing anywhere — an edit through a symlinked file or a
// symlinked ancestor directory must be refused.
func TestFileEditSymlinkContainment(t *testing.T) {
	a, _, _ := newTestAgent(t)
	folder := newTestFileWorkspace(t, a, "u1", "w1")
	const handle = "u1"

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlinked FILE target: the edit is refused (it would follow the
	// link outside).
	if err := os.Symlink(outsideFile, filepath.Join(folder, "link.txt")); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}
	out := editToolExec(t, a, handle, map[string]any{
		"path": "link.txt", "old_string": "secret", "new_string": "clobber",
	})
	if !strings.Contains(out, "symlink") {
		t.Fatalf("symlinked file edit: want the refusal, got %q", out)
	}
	// The link's target is untouched.
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "top secret\n" {
		t.Fatalf("the symlink edit touched the outside file: %q (err %v)", got, err)
	}

	// A symlinked ANCESTOR directory: the lexical path is clean
	// (dirtarget/f.txt) but the directory resolves outside — refused.
	if err := os.MkdirAll(filepath.Join(outside, "fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "fake", "f.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "fake"), filepath.Join(folder, "dirtarget")); err != nil {
		t.Fatal(err)
	}
	out = editToolExec(t, a, handle, map[string]any{
		"path": "dirtarget/f.txt", "old_string": "x", "new_string": "y",
	})
	if !strings.Contains(out, "escape") && !strings.Contains(out, "symlink") {
		t.Fatalf("edit through a symlinked dir: want the refusal, got %q", out)
	}

	// A clean relative path still works (the gate is not a blanket ban).
	writeFileToDisk(t, filepath.Join(folder, "ok.txt"), "fine\n")
	out = editToolExec(t, a, handle, map[string]any{
		"path": "ok.txt", "old_string": "fine", "new_string": "F",
	})
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("a clean path broke: %q", out)
	}
}
