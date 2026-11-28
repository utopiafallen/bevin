package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestSkillsEndpoints — the user side of the SKILL SYSTEM (the design
// log's SKILL SYSTEM section: the agent AUTHORS skills, the user REVIEWs
// and DELETEs them): GET/DELETE the global skills (store rows) +
// GET/DELETE a workspace's .SKILLS/ files (the disk is the source of
// truth — the listing reads the files, the delete removes one).
func TestSkillsEndpoints(t *testing.T) {
	a, _, _ := twoModelApp(t)
	bob := "tok-bob"

	// A workspace (created with its .SKILLS/ folder).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/workspaces", bob, map[string]string{"title": "Notes"}))
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
	base := created.Workspace.FolderPath

	// The global list starts empty.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/skills", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("global list code = %d body=%s", w.Code, w.Body.String())
	}
	var gl struct {
		Skills []map[string]any `json:"skills"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &gl)
	if len(gl.Skills) != 0 {
		t.Fatalf("initial global skills = %+v, want none", gl.Skills)
	}

	// A global skill row (the agent's skill_create writes it — the test
	// writes the same row directly).
	us, err := a.ag.UserStoreFor("bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := us.CreateSkill("deploy-cli", "deploys the app", "run the deploy script", nil); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/skills", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("global list2 code = %d", w.Code)
	}
	var gl2 struct {
		Skills []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Content     string `json:"content"`
		} `json:"skills"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &gl2)
	if len(gl2.Skills) != 1 || gl2.Skills[0].Name != "deploy-cli" || gl2.Skills[0].Content != "run the deploy script" {
		t.Fatalf("global list = %+v, want the full skill (the review surface carries the body)", gl2.Skills)
	}

	// A workspace-local skill file on disk (the agent's writer layout:
	// one JSON file per skill in .SKILLS/).
	body, _ := json.Marshal(map[string]string{
		"name": "build-notes", "description": "the build recipe", "content": "make && test",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
	})
	skillFile := filepath.Join(base, ".SKILLS", "build-notes.json")
	if err := os.WriteFile(skillFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// The workspace-local skills (the design's second list, with the
	// workspace column): the AGGREGATE endpoint walks every workspace.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/skills", bob, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("ws skills code = %d body=%s", w.Code, w.Body.String())
	}
	var wl struct {
		Skills []struct {
			WorkspaceID    string `json:"workspace_id"`
			WorkspaceTitle string `json:"workspace_title"`
			Name           string `json:"name"`
			Content        string `json:"content"`
		} `json:"skills"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &wl)
	if len(wl.Skills) != 1 || wl.Skills[0].Name != "build-notes" || wl.Skills[0].Content != "make && test" ||
		wl.Skills[0].WorkspaceID != wsID || wl.Skills[0].WorkspaceTitle != "Notes" {
		t.Fatalf("ws skills = %+v, want the on-disk skill attributed to its workspace", wl.Skills)
	}

	// Validation: an unknown global skill is a 404, a missing name a 400.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/skills?name=ghost", bob, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("ghost delete code = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/skills", bob, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("nameless delete code = %d, want 400", w.Code)
	}

	// Delete the global skill: 204, then gone from the list.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/skills?name=deploy-cli", bob, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("global delete code = %d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/skills", bob, nil))
	if err := json.Unmarshal(w.Body.Bytes(), &gl); err != nil {
		t.Fatal(err)
	}
	if len(gl.Skills) != 0 {
		t.Fatalf("global list after delete = %+v, want none", gl.Skills)
	}

	// Delete the workspace skill: 204, the FILE goes from disk.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/"+wsID+"/skills?name=build-notes", bob, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("ws skill delete code = %d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(skillFile); !os.IsNotExist(err) {
		t.Fatalf("the .SKILLS/ file survived the delete: %v", err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/workspaces/skills", bob, nil))
	if err := json.Unmarshal(w.Body.Bytes(), &wl); err != nil {
		t.Fatal(err)
	}
	if len(wl.Skills) != 0 {
		t.Fatalf("ws skills after delete = %+v, want none", wl.Skills)
	}

	// Unknowns: an unknown workspace 404s (the per-workspace DELETE keeps
	// its lookup), a nameless match 404s.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/doesnotexist/skills?name=x", bob, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown ws skill delete code = %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodDelete, "/api/workspaces/"+wsID+"/skills?name=ghost", bob, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("ghost ws skill delete code = %d, want 404", w.Code)
	}
}
