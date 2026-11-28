package server

// The user side of the SKILL SYSTEM (the design log's SKILL SYSTEM
// section): the AGENT authors skills; the user REVIEWs and DELETEs them —
// never creates or edits. Global skills are store rows; workspace-local
// skills are .SKILLS/ files in the workspace folder (the disk is the
// source of truth, exactly like the workspace files — the listing reads
// the files, the delete removes one).

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/utopiafallen/bevin/internal/store"
)

// skillWire is the user-facing form of a skill — the full body, this is a
// review surface (the workspace page renders it for reading).
type skillWire struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// GET /api/skills — the user's global skills.
func (a *App) listSkills(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	sks, err := us.ListSkills()
	if err != nil {
		log.Printf("list skills: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]skillWire, 0, len(sks))
	for _, s := range sks {
		out = append(out, skillWire{Name: s.Name, Description: s.Description, Content: s.Content, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// DELETE /api/skills?name=... — hard-delete a global skill (the user's
// only write on skills; the agent re-creates what it needs in a later
// conversation — the deferred zombie-skill guard, design log).
func (a *App) deleteSkill(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := us.DeleteSkill(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no global skill named " + name})
			return
		}
		log.Printf("delete skill %q: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// wsSkillFileWire is the on-disk .SKILLS/ file shape — the P10 fields
// (name/description/content/created_at/updated_at), one file per skill.
type wsSkillFileWire struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// listWsSkillFiles reads a workspace's .SKILLS/ folder from disk (sorted;
// unreadable or malformed files are skipped, mirroring the agent's reader).
func listWsSkillFiles(folder string) []wsSkillFileWire {
	matches, _ := filepath.Glob(filepath.Join(folder, ".SKILLS", "*.json"))
	sort.Strings(matches)
	var out []wsSkillFileWire
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f wsSkillFileWire
		if err := json.Unmarshal(data, &f); err != nil || f.Name == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// wsSkillEntry is one workspace-local skill in the aggregate listing —
// the .SKILLS/ file's fields plus the WORKSPACE it belongs to (the skills
// page's second list carries a workspace column).
type wsSkillEntry struct {
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceTitle string `json:"workspace_title"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Content        string `json:"content"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// GET /api/workspaces/skills — EVERY workspace's local skills with
// attribution (the skills page's "workspace-local" list — one row per
// skill, the workspace column showing whose it is).
func (a *App) allWorkspaceSkills(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	wss, err := us.ListWorkspaces()
	if err != nil {
		log.Printf("list workspaces: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var out []wsSkillEntry
	for _, ws := range wss {
		for _, f := range listWsSkillFiles(ws.FolderPath) {
			out = append(out, wsSkillEntry{WorkspaceID: ws.ID, WorkspaceTitle: ws.Title, Name: f.Name, Description: f.Description, Content: f.Content, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkspaceTitle != out[j].WorkspaceTitle {
			return out[i].WorkspaceTitle < out[j].WorkspaceTitle
		}
		return out[i].Name < out[j].Name
	})
	if out == nil {
		out = []wsSkillEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// DELETE /api/workspaces/{id}/skills?name=... — remove one .SKILLS/ file
// (matched by the skill's NAME field, not the slug — the file name is an
// implementation detail the user never sees).
func (a *App) deleteWorkspaceSkill(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	ws, ok := a.workspaceFor(w, r, us)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	dir := filepath.Join(ws.FolderPath, ".SKILLS")
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f wsSkillFileWire
		if err := json.Unmarshal(data, &f); err != nil || f.Name != name {
			continue
		}
		if err := os.Remove(p); err != nil {
			log.Printf("delete workspace skill %q: %v", name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no skill named " + name + " in this workspace"})
}
