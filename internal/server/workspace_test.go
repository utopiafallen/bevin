package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uploadRequest builds a multipart POST (fields: path + file).
func uploadRequest(target, dir, name, content string) *http.Request {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("path", dir)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		panic(err)
	}
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-bob"})
	return r
}

// TestWorkspaceLifecycle — the workspace API end to end (as bob):
// create (folder + .SKILLS + .dni on disk), list, upload (flat + into a
// subdirectory), file listing from disk, download, delete a file, run
// (503 — sandboxing is off in the test config), delete the workspace
// (folder + rows), 404 afterwards. Path traversal is rejected at every
// file endpoint.
func TestWorkspaceLifecycle(t *testing.T) {
	a, _, _ := twoModelApp(t)
	bob := "tok-bob"

	// 1. The list starts empty.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list code = %d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Workspaces []struct {
			ID string `json:"id"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Workspaces) != 0 {
		t.Fatalf("initial list = %+v, want empty", list.Workspaces)
	}

	// 2. Create. A blank title is rejected first.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/workspaces", bob, map[string]string{"title": "  "}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank title code = %d, want 400", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/workspaces", bob, map[string]string{"title": "My Notes", "description": "scratch"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create code = %d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Workspace struct {
			ID         string `json:"id"`
			FolderPath string `json:"folder_path"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	wsID := created.Workspace.ID
	if wsID == "" {
		t.Fatal("no workspace id in the create response")
	}
	// The folder exists with the .SKILLS + .dni subfolders.
	base := created.Workspace.FolderPath
	for _, sub := range []string{".SKILLS", ".dni"} {
		st, err := os.Stat(filepath.Join(base, sub))
		if err != nil || !st.IsDir() {
			t.Fatalf("%s missing on disk: %v", sub, err)
		}
	}

	// 3. Files listing: the two managed subfolders, nothing else.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/"+wsID+"/files", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("files code = %d body=%s", w.Code, w.Body.String())
	}
	var fl struct {
		Files []struct {
			Path string `json:"path"`
			Dir  bool   `json:"dir"`
			Size int64  `json:"size"`
		} `json:"files"`
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil {
		t.Fatal(err)
	}
	if len(fl.Files) != 2 || fl.Size != 0 {
		t.Fatalf("initial files = %+v size %d, want .SKILLS/.dni only", fl.Files, fl.Size)
	}

	// 4. Upload a file at the root.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, uploadRequest("/api/workspaces/"+wsID+"/files", "", "hello.txt", "hello workspace"))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload code = %d body=%s", w.Code, w.Body.String())
	}
	var up struct {
		File struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"file"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.File.Path != "hello.txt" || up.File.Size != 15 {
		t.Fatalf("upload = %+v, want hello.txt / 15 bytes", up.File)
	}

	// 5. Upload into a subdirectory (created on the fly).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, uploadRequest("/api/workspaces/"+wsID+"/files", "docs/reports", "q3.txt", "numbers"))
	if w.Code != http.StatusCreated {
		t.Fatalf("subdir upload code = %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.File.Path != "docs/reports/q3.txt" {
		t.Fatalf("subdir upload path = %q, want docs/reports/q3.txt", up.File.Path)
	}

	// 6. The listing now shows both files + the subdirectory, with the
	//    right total size.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/"+wsID+"/files", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("files2 code = %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]struct {
		Dir  bool
		Size int64
	}{}
	for _, e := range fl.Files {
		byPath[e.Path] = struct {
			Dir  bool
			Size int64
		}{e.Dir, e.Size}
	}
	if _, ok := byPath["docs"]; !ok || !byPath["docs"].Dir {
		t.Fatalf("listing missing the docs directory: %+v", byPath)
	}
	if e, ok := byPath["docs/reports/q3.txt"]; !ok || e.Size != 7 {
		t.Fatalf("listing missing docs/reports/q3.txt: %+v", byPath)
	}
	if fl.Size != 15+7 {
		t.Fatalf("size = %d, want %d", fl.Size, 15+7)
	}

	// 7. Download round-trips the bytes (attachment).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/"+wsID+"/files/raw?path=docs/reports/q3.txt", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("raw code = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "numbers" {
		t.Fatalf("downloaded = %q, want 'numbers'", got)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q, want attachment", cd)
	}

	// 8. Path traversal is rejected at every file endpoint.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, uploadRequest("/api/workspaces/"+wsID+"/files", "../evil", "x.txt", "x"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal upload code = %d, want 400", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/"+wsID+"/files/raw?path=../../server.toml", bob, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal raw code = %d, want 400", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/"+wsID+"/files?path=..", bob, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal delete code = %d, want 400", w.Code)
	}

	// 9. Delete the subdir file (then a raw fetch of it 404s), then the
	//    directory (everything beneath a directory goes too).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/"+wsID+"/files?path=docs/reports/q3.txt", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("file delete code = %d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/"+wsID+"/files/raw?path=docs/reports/q3.txt", bob, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("raw after file delete = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/"+wsID+"/files?path=docs", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("dir delete code = %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(base, "docs")); !os.IsNotExist(err) {
		t.Fatal("docs/ survived the directory delete")
	}

	// 10. Run: the user-facing "run in workspace" endpoint is GONE (user
	//     directive — the agent manages the user's environment, the user
	//     focuses on workspace state): the route no longer exists. The
	//     mux answers 405 (the path is claimed by the GET / SPA
	//     catch-all, the method is not) or 404 — either proves the
	//     endpoint is gone.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/workspaces/"+wsID+"/run", bob, map[string]string{"command": "echo hi"}))
	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("run code = %d body=%s, want 404/405 (the endpoint was removed)", w.Code, w.Body.String())
	}

	// 11. Delete the workspace: folder + rows go.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/"+wsID, bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("workspace delete code = %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatal("the workspace folder survived the delete")
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/"+wsID+"/files", bob, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("files after workspace delete = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/"+wsID, bob, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("second workspace delete = %d, want 404", w.Code)
	}

	// 12. The list is back to empty (per-user store: bob sees exactly
	//     his own workspaces, all deleted now).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("final list code = %d", w.Code)
	}
	var final struct {
		Workspaces []any `json:"workspaces"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &final)
	if len(final.Workspaces) != 0 {
		t.Fatalf("final list = %d workspaces, want 0", len(final.Workspaces))
	}
}
