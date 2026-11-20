package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSafeRelPathColon pins the NTFS ADS gate (security 2026-11-20): on
// Windows a colon in a path targets an alternate data stream (content
// invisible to listings, retrievable by name) — rejected. On other OSes a
// colon is a legal filename character — allowed. The existing escapes are
// rejected either way.
func TestSafeRelPathColon(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := safeRelPath("file.txt:evil"); err == nil {
			t.Fatal("a colon must be rejected on Windows (NTFS ADS)")
		}
		if _, err := safeRelPath("docs\\a:b"); err == nil {
			t.Fatal("a colon mid-path must be rejected on Windows")
		}
	} else {
		if rel, err := safeRelPath("notes: draft.txt"); err != nil {
			t.Fatalf("a colon must be legal on non-Windows: %v", err)
		} else if rel == "" {
			t.Fatal("empty rel")
		}
	}
	if _, err := safeRelPath("../x"); err == nil {
		t.Fatal(".. must be rejected")
	}
	if _, err := safeRelPath("/abs"); err == nil {
		t.Fatal("an absolute path must be rejected")
	}
}

// chunkedUploadRequest builds a multipart upload with ContentLength -1
// (the chunked / no-declared-length case): the declared-length fast path
// cannot see the size, so the caps must be enforced mid-stream.
func chunkedUploadRequest(t *testing.T, url, filename string, content []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if fw, err := mw.CreateFormFile("file", filename); err != nil {
		t.Fatal(err)
	} else if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, url, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.ContentLength = -1 // chunked: no declared length
	r.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-bob"})
	return r
}

// createBobWorkspace creates a workspace for bob through the API and
// returns its id + on-disk folder.
func createBobWorkspace(t *testing.T, a *App, title string) (string, string) {
	t.Helper()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/workspaces", "tok-bob", map[string]string{"title": title}))
	if w.Code != http.StatusCreated {
		t.Fatalf("workspace create code = %d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		Workspace struct {
			ID         string `json:"id"`
			FolderPath string `json:"folder_path"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res.Workspace.ID, res.Workspace.FolderPath
}

// assertNoUploadLeftovers verifies a rejected upload left neither the
// target file nor a .upload temp behind.
func assertNoUploadLeftovers(t *testing.T, folder, name string) {
	t.Helper()
	target := filepath.Join(folder, name)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a rejected upload must not leave %q on disk (stat err = %v)", name, err)
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".upload") {
			t.Fatalf("a rejected upload left a temp file: %s", e.Name())
		}
	}
}

// TestUploadChunkedEnforcesFileCap — the per-file cap holds for a CHUNKED
// upload (no declared Content-Length): the old check only looked at
// r.ContentLength, which is -1 here, so the cap now comes from the
// mid-stream LimitReader (security 2026-11-20).
func TestUploadChunkedEnforcesFileCap(t *testing.T) {
	a, _, _ := twoModelApp(t)
	oldCap := wsMaxUploadBytes
	wsMaxUploadBytes = 1024
	t.Cleanup(func() { wsMaxUploadBytes = oldCap })

	wsID, folder := createBobWorkspace(t, a, "caps")
	r := chunkedUploadRequest(t, "/api/workspaces/"+wsID+"/files", "big.bin", bytes.Repeat([]byte("x"), 4096))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked over-cap upload code = %d body=%s, want 413", w.Code, w.Body.String())
	}
	assertNoUploadLeftovers(t, folder, "big.bin")

	// A chunked upload WITHIN the cap still lands (the cap is not a
	// chunked upload ban).
	r = chunkedUploadRequest(t, "/api/workspaces/"+wsID+"/files", "ok.bin", bytes.Repeat([]byte("y"), 512))
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("chunked under-cap upload code = %d body=%s, want 201", w.Code, w.Body.String())
	}
	if st, err := os.Stat(filepath.Join(folder, "ok.bin")); err != nil || st.Size() != 512 {
		t.Fatalf("the under-cap chunked upload did not land (err = %v, size = %v)", err, st)
	}
}

// TestUploadBudgetEnforcedIncrementally — the workspace budget is enforced
// MID-STREAM: a file that would push the workspace past its limit stops at
// the crossing (nothing past it reaches disk), not after the whole file is
// written (the old post-write check).
func TestUploadBudgetEnforcedIncrementally(t *testing.T) {
	a, cfg, _ := twoModelApp(t)
	cfg.MaxWorkspaceSize = 2048

	wsID, folder := createBobWorkspace(t, a, "budget")

	// 1 KiB fits (budget 2048).
	r := chunkedUploadRequest(t, "/api/workspaces/"+wsID+"/files", "one.bin", bytes.Repeat([]byte("a"), 1024))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("first upload code = %d body=%s, want 201", w.Code, w.Body.String())
	}

	// 2 KiB crosses the remaining 1 KiB mid-stream: 413, nothing past the
	// crossing on disk, no temp left.
	r = chunkedUploadRequest(t, "/api/workspaces/"+wsID+"/files", "two.bin", bytes.Repeat([]byte("b"), 2048))
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("budget-crossing upload code = %d body=%s, want 413", w.Code, w.Body.String())
	}
	assertNoUploadLeftovers(t, folder, "two.bin")

	// Fill the workspace exactly to its limit (2 x 1 KiB = 2048).
	r = chunkedUploadRequest(t, "/api/workspaces/"+wsID+"/files", "fill.bin", bytes.Repeat([]byte("d"), 1024))
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("fill upload code = %d body=%s, want 201", w.Code, w.Body.String())
	}

	// A request to a workspace AT its limit is refused before any write.
	r = chunkedUploadRequest(t, "/api/workspaces/"+wsID+"/files", "three.bin", bytes.Repeat([]byte("c"), 100))
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("at-limit upload code = %d body=%s, want 413", w.Code, w.Body.String())
	}
	assertNoUploadLeftovers(t, folder, "three.bin")
}
