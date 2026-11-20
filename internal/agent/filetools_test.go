package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/store"
)

// edit_file (WS4): the spec's test matrix minus the read/write halves —
// path containment, workspace resolution, edit exactness + near-miss
// diagnostics, replace_all, the size budget, and the P12 metadata refresh.

func editToolExec(t *testing.T, a *Agent, handle string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	tl, ok := a.tools.Get("edit_file")
	if !ok {
		t.Fatalf("edit_file not registered")
	}
	out, err := tl.Execute(context.Background(), handle, raw)
	if err != nil {
		t.Fatalf("edit_file returned an error (a failure must be a result string): %v", err)
	}
	return out
}

// newTestFileWorkspace creates the user's store + one workspace (id +
// title) and its on-disk folder.
func newTestFileWorkspace(t *testing.T, a *Agent, handle, wsID string) string {
	t.Helper()
	us, err := a.userStore(handle)
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	if _, err := us.CreateWorkspaceWithID(wsID, "Title-"+wsID, "", ""); err != nil {
		t.Fatalf("CreateWorkspaceWithID: %v", err)
	}
	folder := filepath.Join(t.TempDir(), "ws")
	// Re-create the workspace pointing at the temp folder (derived folders
	// would land under the agent's data dir; the temp folder keeps the test
	// hermetic and exercises the FolderPath override).
	if err := us.DeleteWorkspace(wsID); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if _, err := us.CreateWorkspaceWithID(wsID, "Title-"+wsID, "", folder); err != nil {
		t.Fatalf("CreateWorkspaceWithID (folder): %v", err)
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return folder
}

func writeFileToDisk(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFileEditFile(t *testing.T) {
	a, _, _ := newTestAgent(t)
	folder := newTestFileWorkspace(t, a, "u1", "w1")
	const handle = "u1"
	const path = "doc.txt"
	content := "alpha\nbeta\ngamma\n"
	writeFileToDisk(t, filepath.Join(folder, path), content)

	// Path safety: absolute + ..
	for _, p := range []string{"/etc/passwd", "../outside.txt", "a/../../x.txt"} {
		out := editToolExec(t, a, handle, map[string]any{"path": p, "old_string": "a", "new_string": "b"})
		if !strings.Contains(out, "escape") && !strings.Contains(out, "relative to the workspace") {
			t.Fatalf("path %q: want a confinement error, got %q", p, out)
		}
	}

	// Unique replacement + the P12 metadata row appears (source=agent).
	out := editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "beta", "new_string": "BETA",
	})
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("want success, got %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(folder, path))
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("edit result = %q", got)
	}
	us, _ := a.userStore(handle)
	rows, err := us.ListWorkspaceFiles("w1")
	if err != nil || len(rows) != 1 || rows[0].RelativePath != path || rows[0].Source != "agent" {
		t.Fatalf("P12 row: %+v, err %v", rows, err)
	}

	// 0 matches with a case-insensitive hit → the casing hint.
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "Gamma", "new_string": "x",
	})
	if !strings.Contains(out, "case-insensitive") {
		t.Fatalf("want the casing hint, got %q", out)
	}

	// 0 matches with a similar region → the closest-region snippet.
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "beta extra", "new_string": "x",
	})
	if !strings.Contains(out, "closest region") || !strings.Contains(out, "BETA") {
		t.Fatalf("want the near-miss snippet, got %q", out)
	}

	// 0 matches with nothing similar → the look-at-the-content advice.
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "zzzzzzzz", "new_string": "x",
	})
	if !strings.Contains(out, "current content") {
		t.Fatalf("want the look-at-the-content advice, got %q", out)
	}

	// Whitespace is significant (exact match only).
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": " BETA", "new_string": "x",
	})
	if strings.Contains(out, "replaced") {
		t.Fatalf("a whitespace-drifted match must not apply: %q", out)
	}

	// Multiple matches without replace_all → the count + advice.
	writeFileToDisk(t, filepath.Join(folder, path), "dup\ndup\ndup\n")
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "dup", "new_string": "one",
	})
	if !strings.Contains(out, "3 occurrences") {
		t.Fatalf("want the multi-match count, got %q", out)
	}
	// replace_all replaces them all; the P12 row refreshes (no duplicate).
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "dup", "new_string": "one", "replace_all": true,
	})
	if !strings.Contains(out, "all 3") {
		t.Fatalf("want replace-all result, got %q", out)
	}
	got, _ = os.ReadFile(filepath.Join(folder, path))
	if string(got) != "one\none\none\n" {
		t.Fatalf("replace_all result = %q", got)
	}
	rows, _ = us.ListWorkspaceFiles("w1")
	if len(rows) != 1 {
		t.Fatalf("want 1 row (no duplicate on re-edit), got %d", len(rows))
	}
	for _, r := range rows {
		if r.RelativePath == path && r.Size != 12 {
			t.Fatalf("row not refreshed (size %d, want 12)", r.Size)
		}
	}
	// Empty new_string deletes the match.
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "one\n", "new_string": "", "replace_all": true,
	})
	if !strings.Contains(out, "all 3") {
		t.Fatalf("want delete-all result, got %q", out)
	}
	got, _ = os.ReadFile(filepath.Join(folder, path))
	if string(got) != "" {
		t.Fatalf("delete-all result = %q", got)
	}

	// Arg validation: empty old_string, old == new, missing file, directory.
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "  ", "new_string": "x",
	})
	if !strings.Contains(out, "non-empty") {
		t.Fatalf("want empty-old_string refusal, got %q", out)
	}
	out = editToolExec(t, a, handle, map[string]any{
		"path": path, "old_string": "a", "new_string": "a",
	})
	if !strings.Contains(out, "identical") {
		t.Fatalf("want identical refusal, got %q", out)
	}
	out = editToolExec(t, a, handle, map[string]any{
		"path": "ghost.txt", "old_string": "a", "new_string": "b",
	})
	if !strings.Contains(out, "create it first") {
		t.Fatalf("want missing-file result, got %q", out)
	}
	if err := os.MkdirAll(filepath.Join(folder, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	out = editToolExec(t, a, handle, map[string]any{
		"path": "adir", "old_string": "a", "new_string": "b",
	})
	if !strings.Contains(out, "is a directory") {
		t.Fatalf("want directory refusal, got %q", out)
	}
}

func TestFileWorkspaceResolution(t *testing.T) {
	a, _, _ := newTestAgent(t)
	handle := "u1"
	w1 := newTestFileWorkspace(t, a, handle, "alpha")
	writeFileToDisk(t, filepath.Join(w1, "m.txt"), "x\n")

	// Single workspace: the workspace arg is optional.
	out := editToolExec(t, a, handle, map[string]any{
		"path": "m.txt", "old_string": "x", "new_string": "y",
	})
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("single-workspace default failed: %q", out)
	}

	// A second workspace makes the arg mandatory (omitted → the options).
	w2 := newTestFileWorkspace(t, a, handle, "beta")
	writeFileToDisk(t, filepath.Join(w2, "m.txt"), "z\n")
	out = editToolExec(t, a, handle, map[string]any{
		"path": "m.txt", "old_string": "x", "new_string": "y2",
	})
	if !strings.Contains(out, `"alpha"`) || !strings.Contains(out, `"beta"`) {
		t.Fatalf("want both workspaces listed, got %q", out)
	}

	// By id, by exact title, by case-insensitive title — each edits ALPHA's
	// file (resolving to beta would miss: beta's m.txt has no such content).
	// The match accumulates so each edit is unique against the current
	// file.
	cur := "y"
	for _, name := range []string{"alpha", "Title-alpha", "title-alpha"} {
		out = editToolExec(t, a, handle, map[string]any{
			"path": "m.txt", "workspace": name, "old_string": cur, "new_string": cur + "2",
		})
		if !strings.Contains(out, "replaced the match") {
			t.Fatalf("workspace %q did not resolve: %q", name, out)
		}
		cur += "2"
	}
	got, _ := os.ReadFile(filepath.Join(w1, "m.txt"))
	if string(got) != cur+"\n" {
		t.Fatalf("alpha's file not edited by title resolution: %q (want %q)", got, cur)
	}
	got, _ = os.ReadFile(filepath.Join(w2, "m.txt"))
	if string(got) != "z\n" {
		t.Fatalf("beta's file must be untouched: %q", got)
	}

	// Unknown name → the options.
	out = editToolExec(t, a, handle, map[string]any{
		"path": "m.txt", "workspace": "nope", "old_string": "x", "new_string": "y",
	})
	if !strings.Contains(out, "no workspace named") {
		t.Fatalf("want unknown-workspace error, got %q", out)
	}
}

// TestFileWorkspaceSizeBudget: an edit that would push the workspace over
// the admin's max_workspace_size cap is refused (the agent must not bypass
// the user's disk cap). The check counts the workspace's current total +
// the new file content, so the cap is set accordingly.
func TestFileWorkspaceSizeBudget(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	folder := newTestFileWorkspace(t, a, "u1", "w1")
	const handle = "u1"
	cfg.MaxWorkspaceSize = 40 // bytes
	defer func() { cfg.MaxWorkspaceSize = 0 }()

	writeFileToDisk(t, filepath.Join(folder, "base.txt"), strings.Repeat("x", 14)+"z")
	// The edit grows base.txt from 15 to 34 bytes: 15 (current total) + 34
	// (new content) = 49 > 40 → refused; the file is not touched.
	out := editToolExec(t, a, handle, map[string]any{
		"path": "base.txt", "old_string": "z", "new_string": strings.Repeat("y", 20),
	})
	if !strings.Contains(out, "size limit") {
		t.Fatalf("want the budget refusal, got %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(folder, "base.txt"))
	if len(got) != 15 {
		t.Fatalf("the over-budget edit must not touch the file (len=%d)", len(got))
	}
	// An edit that fits goes through (15 + 15 ≤ 40).
	out = editToolExec(t, a, handle, map[string]any{
		"path": "base.txt", "old_string": "z", "new_string": "y",
	})
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("want success, got %q", out)
	}
}

// TestUpsertWorkspaceFileStore is the store-level half of the P12 refresh
// (the agent's recordWorkspaceFile rides on it).
func TestUpsertWorkspaceFileStore(t *testing.T) {
	dir := t.TempDir()
	us, err := store.OpenUser(dir)
	if err != nil {
		t.Fatalf("OpenUser: %v", err)
	}
	defer us.Close()
	if _, err := us.CreateWorkspace("w", "W", ""); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	ws, err := us.ListWorkspaces()
	if err != nil || len(ws) != 1 {
		t.Fatalf("ListWorkspaces: %+v, %v", ws, err)
	}
	// Insert path.
	if err := us.UpsertWorkspaceFile(ws[0].ID, "a.md", "a.md", 10, "hash1", "agent", "doc"); err != nil {
		t.Fatalf("Upsert insert: %v", err)
	}
	rows, _ := us.ListWorkspaceFiles(ws[0].ID)
	if len(rows) != 1 || rows[0].Size != 10 || rows[0].Hash == nil || *rows[0].Hash != "hash1" {
		t.Fatalf("insert row: %+v", rows)
	}
	// Refresh path: same relative_path → the row updates, no duplicate.
	if err := us.UpsertWorkspaceFile(ws[0].ID, "a.md", "a.md", 25, "hash2", "agent", "doc"); err != nil {
		t.Fatalf("Upsert refresh: %v", err)
	}
	rows, _ = us.ListWorkspaceFiles(ws[0].ID)
	if len(rows) != 1 || rows[0].Size != 25 || *rows[0].Hash != "hash2" {
		t.Fatalf("refresh row: %+v", rows)
	}
}
