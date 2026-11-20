package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveInside pins the workspace symlink re-check (security
// 2026-11-20): the lexical checks (safeRelPath + joinSafe) can't see a link
// inside the workspace pointing anywhere — a symlinked file target, or a
// symlinked ancestor that resolves outside the root, must be refused; clean
// paths (existing or to-be-created) pass.
func TestResolveInside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlinked file target.
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}
	if err := resolveInside(root, link); err == nil {
		t.Fatal("a symlinked file target must be refused")
	}
	// A symlinked ancestor resolving outside.
	if err := os.MkdirAll(filepath.Join(outside, "fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "fake"), filepath.Join(root, "dirtarget")); err != nil {
		t.Fatal(err)
	}
	if err := resolveInside(root, filepath.Join(root, "dirtarget", "f.txt")); err == nil {
		t.Fatal("a path through a symlinked dir outside must be refused")
	}
	// Clean: an existing file, and a to-be-created file under a real dir.
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolveInside(root, filepath.Join(root, "real.txt")); err != nil {
		t.Fatalf("a clean existing file must pass: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := resolveInside(root, filepath.Join(root, "docs", "new.md")); err != nil {
		t.Fatalf("a clean to-be-created path must pass: %v", err)
	}
}

// TestWorkspaceSymlinkHTTP — the endpoints refuse symlink escapes end to
// end: a download through a symlinked file, and an upload into a symlinked
// directory (security 2026-11-20).
func TestWorkspaceSymlinkHTTP(t *testing.T) {
	a, _, _ := twoModelApp(t)
	wsID, folder := createBobWorkspace(t, a, "symlink")
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(folder, "link.txt")); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "fake"), filepath.Join(folder, "dirtarget")); err != nil {
		t.Fatal(err)
	}

	// Download through a symlinked file: 400, the outside file is never
	// served.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/"+wsID+"/files/raw?path=link.txt", "tok-bob", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("symlink download code = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "top secret") {
		t.Fatal("the outside file was served through the symlink")
	}

	// Upload into a symlinked directory: 400, nothing lands outside.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("path", "dirtarget"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "in.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("should not land")); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/files", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-bob"})
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("symlink-dir upload code = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "fake", "in.txt")); !os.IsNotExist(err) {
		t.Fatal("the upload landed outside the workspace through the symlink")
	}
}
