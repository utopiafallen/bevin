package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/store"
)

// ---- skill file layout (workspace tier) ----
//
// A workspace-local skill is one JSON file in the workspace's .SKILLS
// folder, mirroring the global (P10) row's fields: name, description,
// content, created_at, updated_at. The file name is the slug of the name
// (a collision between two different names gets a numeric suffix).

type wsSkillFile struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func skillSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('-')
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "skill"
	}
	return s
}

// wsSkillFileFor returns the on-disk path for a workspace skill name,
// taking a collision suffix when another skill already holds the base slug.
func wsSkillFileFor(skillsDir, name string) string {
	base := skillSlug(name)
	cand := filepath.Join(skillsDir, base+".json")
	if wsSkillNameAt(cand) == name {
		return cand
	}
	if _, err := os.Stat(cand); err != nil {
		return cand
	}
	for i := 2; ; i++ {
		cand = filepath.Join(skillsDir, fmt.Sprintf("%s-%d.json", base, i))
		if wsSkillNameAt(cand) == name {
			return cand
		}
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
	}
}

func wsSkillNameAt(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var f wsSkillFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return f.Name
}

// wsSkillsDir is a workspace's .SKILLS folder in the user's data dir.
func (a *Agent) wsSkillsDir(handle, workspaceID string) string {
	return filepath.Join(a.workspaceFolderRoot(handle), workspaceID, ".SKILLS")
}

// listWsSkills reads every skill file in a .SKILLS folder (corrupt or
// unreadable files are skipped — a bad file must not blind the healthy
// ones).
func listWsSkills(skillsDir string) []wsSkillFile {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil
	}
	var out []wsSkillFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(skillsDir, e.Name()))
		if err != nil {
			continue
		}
		var f wsSkillFile
		if err := json.Unmarshal(data, &f); err != nil || f.Name == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func writeWsSkill(skillsDir string, f wsSkillFile) error {
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(wsSkillFileFor(skillsDir, f.Name), data, 0o600)
}

func wsSkillExists(skillsDir, name string) bool {
	for _, f := range listWsSkills(skillsDir) {
		if f.Name == name {
			return true
		}
	}
	return false
}

// ---- per-conversation active skill set ----
//
// The design log's SKILL SYSTEM section: a loaded skill joins the
// conversation's ACTIVE SET; its content is re-injected into every request
// until skill_unload drops it (or the conversation's workspace pointer
// changes, evicting that workspace's skills). Keys:
// "global:<name>" or "ws:<workspaceID>:<name>". In-memory runtime state —
// a bevin restart clears the sets (v1), like the pty session registry.

func (a *Agent) activeSkillKeys(convID string) []string {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	keys := make([]string, 0, len(a.skillLoaded[convID]))
	for k := range a.skillLoaded[convID] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (a *Agent) addLoadedSkill(convID, key string) bool {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	set := a.skillLoaded[convID]
	if set == nil {
		set = map[string]bool{}
		a.skillLoaded[convID] = set
	}
	if set[key] {
		return false
	}
	set[key] = true
	return true
}

func (a *Agent) removeLoadedSkill(convID, key string) bool {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	set := a.skillLoaded[convID]
	if !set[key] {
		return false
	}
	delete(set, key)
	if len(set) == 0 {
		delete(a.skillLoaded, convID)
	}
	return true
}

// evictWorkspaceSkills drops every skill loaded from one workspace (the
// WORKSPACE LOADING section's switch rule). Returns the evicted keys.
func (a *Agent) evictWorkspaceSkills(convID, workspaceID string) []string {
	a.skillMu.Lock()
	defer a.skillMu.Unlock()
	prefix := "ws:" + workspaceID + ":"
	set := a.skillLoaded[convID]
	var evicted []string
	for k := range set {
		if strings.HasPrefix(k, prefix) {
			delete(set, k)
			evicted = append(evicted, k)
		}
	}
	if len(set) == 0 {
		delete(a.skillLoaded, convID)
	}
	sort.Strings(evicted)
	return evicted
}

// activeSkillBlock renders the conversation's ACTIVE SKILL SET as the block
// every request carries (the SKILL SYSTEM section's re-injection): one
// section per loaded skill. It floats at the HEAD of the message list as a
// single ephemeral user message, injected right after the compaction
// message (buildRequest) — never the system prompt, whose head is the
// prefix-cached part of every request. Anchored to the top of the window,
// history grows below it and every request is an exact extension of the
// previous one, so the KV prefix cache survives all turns; only a
// load/unload or a compaction re-encodes. Unresolved keys (a skill
// deleted, or its workspace gone) are skipped rather than failing the
// request.
func (a *Agent) activeSkillBlock(us *store.UserStore, handle, convID string) string {
	keys := a.activeSkillKeys(convID)
	if len(keys) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Loaded skills (this conversation):\n")
	wroteAny := false
	for _, key := range keys {
		var f wsSkillFile
		var scope string
		switch {
		case strings.HasPrefix(key, "global:"):
			name := strings.TrimPrefix(key, "global:")
			sk, err := us.GetSkillByName(name)
			if err != nil {
				continue // deleted since the load — skip
			}
			f = wsSkillFile{Name: sk.Name, Description: sk.Description, Content: sk.Content}
			scope = "global"
		default:
			rest := strings.TrimPrefix(key, "ws:")
			wsID, name, ok := strings.Cut(rest, ":")
			if !ok {
				continue
			}
			for _, f2 := range listWsSkills(a.wsSkillsDir(handle, wsID)) {
				if f2.Name == name {
					f = f2
					break
				}
			}
			if f.Name == "" {
				continue // workspace/skill gone — skip
			}
			scope = "workspace-local"
		}
		b.WriteString(fmt.Sprintf("\n### Skill: %s (%s)\n%s\n\n%s\n", f.Name, scope, f.Description, f.Content))
		wroteAny = true
	}
	if !wroteAny {
		return ""
	}
	return b.String()
}

// ---- tier resolution ----
//
// An omitted tier resolves against the conversation: a loaded workspace
// makes workspace-local the resolution (the Skills decision's "reflection
// prefers workspace-local over global"), else global.

type skillRef struct {
	name string
	tier string // "global" | "workspace"
	wsID string // the workspace, when tier == "workspace"
}

func (r skillRef) key() string {
	if r.tier == "global" {
		return "global:" + r.name
	}
	return "ws:" + r.wsID + ":" + r.name
}

func skillTierLabel(ref skillRef) string {
	if ref.tier == "global" {
		return "global"
	}
	return fmt.Sprintf("workspace %s", ref.wsID)
}

// loadedWorkspace is the workspace this conversation's pointer names (""
// when none, or when the context has no conversation).
func (a *Agent) loadedWorkspace(handle, convID string) string {
	if convID == "" {
		return ""
	}
	us, err := a.userStore(handle)
	if err != nil {
		return ""
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		return ""
	}
	return conv.WorkspaceID
}

// resolveSkillRef resolves (name, tier) into a concrete reference: the
// explicit tier as given, or the auto-resolution rules. A wsOverride names
// the workspace directly (edit/delete of a skill in a workspace this
// conversation isn't currently loaded into); "" falls back to the loaded
// workspace. ok=false carries a model-facing explanation.
func (a *Agent) resolveSkillRef(handle, convID, name, tier, wsOverride string) (skillRef, string, bool) {
	switch tier {
	case "":
		if loadedWS := a.loadedWorkspace(handle, convID); loadedWS != "" && wsSkillExists(a.wsSkillsDir(handle, loadedWS), name) {
			return skillRef{name: name, tier: "workspace", wsID: loadedWS}, "", true
		}
		return skillRef{name: name, tier: "global"}, "", true
	case "global":
		return skillRef{name: name, tier: "global"}, "", true
	case "workspace":
		wsID := wsOverride
		if wsID == "" {
			wsID = a.loadedWorkspace(handle, convID)
		}
		if wsID != "" {
			return skillRef{name: name, tier: "workspace", wsID: wsID}, "", true
		}
		return skillRef{}, "no workspace is loaded in this conversation and none was named — set one first (set_workspace), pass the workspace id, or use tier=global", false
	default:
		return skillRef{}, `tier must be "global" or "workspace"`, false
	}
}

// skillContent resolves a concrete reference to its content.
func (a *Agent) skillContent(us *store.UserStore, handle string, ref skillRef) (wsSkillFile, bool) {
	if ref.tier == "global" {
		sk, err := us.GetSkillByName(ref.name)
		if err != nil {
			return wsSkillFile{}, false
		}
		return wsSkillFile{Name: sk.Name, Description: sk.Description, Content: sk.Content}, true
	}
	for _, f := range listWsSkills(a.wsSkillsDir(handle, ref.wsID)) {
		if f.Name == ref.name {
			return f, true
		}
	}
	return wsSkillFile{}, false
}

// ---- tools ----

func registerSkillTools(r *ToolRegistry, a *Agent) error {
	// skill_search — the discovery path (there is no catalog in the system
	// prompt; this is how skills are found).
	if err := r.Register(Tool{
		Name:        "skill_search",
		Description: "Search the user's skills by name/description/content (substring terms). Global skills are always available; a loaded workspace's skills are available too. No skill list is injected — this is the way to find skills.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"q":     map[string]any{"type": "string", "description": "the search text (space-separated terms)"},
				"limit": map[string]any{"type": "integer", "description": "max results (default 10)"},
			},
			"required": []string{"q"},
		},
		Execute: a.execSkillSearch,
	}); err != nil {
		return err
	}
	if err := r.Register(Tool{
		Name:        "skill_load",
		Description: "Load a skill into this conversation: its content then joins your context on every turn until skill_unload. Returns the skill's full content.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "the skill name (from skill_search)"},
				"tier": map[string]any{"type": "string", "enum": []string{"global", "workspace"}, "description": "omit to auto-resolve (a loaded workspace's skill wins, else global)"},
			},
			"required": []string{"name"},
		},
		Execute: a.execSkillLoad,
	}); err != nil {
		return err
	}
	if err := r.Register(Tool{
		Name:        "skill_unload",
		Description: "Unload a skill from this conversation: its content stops being injected. A context-budget primitive — unload what the task no longer needs.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "the skill name"},
				"tier": map[string]any{"type": "string", "enum": []string{"global", "workspace"}, "description": "omit to auto-resolve (a loaded workspace's skill wins, else global)"},
			},
			"required": []string{"name"},
		},
		Execute: a.execSkillUnload,
	}); err != nil {
		return err
	}
	if err := r.Register(Tool{
		Name:        "skill_create",
		Description: "Create a skill (only you author skills — the user can view and delete, never write). Global = all of the user's conversations; workspace = one workspace's .SKILLS folder (available while that workspace is loaded). Records how to use tool-like capabilities in the environment, workflows, conventions.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "a short unique name (<=120 chars)"},
				"description": map[string]any{"type": "string", "description": "one line: what it is + when to use it (<=500 chars)"},
				"content":     map[string]any{"type": "string", "description": "the full skill body"},
				"tier":        map[string]any{"type": "string", "enum": []string{"global", "workspace"}, "description": "omit for the default: workspace-local while a workspace is loaded, else global"},
				"workspace":   map[string]any{"type": "string", "description": "the workspace id, for tier=workspace with none loaded (else the loaded workspace)"},
			},
			"required": []string{"name", "description", "content"},
		},
		Execute: a.execSkillCreate,
	}); err != nil {
		return err
	}
	if err := r.Register(Tool{
		Name:        "skill_edit",
		Description: "Edit a skill's description and/or content (at least one).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "the skill name"},
				"description": map[string]any{"type": "string", "description": "new one-line description"},
				"content":     map[string]any{"type": "string", "description": "new full body"},
				"tier":        map[string]any{"type": "string", "enum": []string{"global", "workspace"}, "description": "omit to auto-resolve"},
				"workspace":   map[string]any{"type": "string", "description": "the workspace id, for tier=workspace with none loaded (else the loaded workspace)"},
			},
			"required": []string{"name"},
		},
		Execute: a.execSkillEdit,
	}); err != nil {
		return err
	}
	if err := r.Register(Tool{
		Name:        "skill_delete",
		Description: "Delete a skill (hard delete). Typically at the user's request — they may view and delete skills; this is the tool that does it.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string", "description": "the skill name"},
				"tier":      map[string]any{"type": "string", "enum": []string{"global", "workspace"}, "description": "omit to auto-resolve"},
				"workspace": map[string]any{"type": "string", "description": "the workspace id, for tier=workspace with none loaded (else the loaded workspace)"},
			},
			"required": []string{"name"},
		},
		Execute: a.execSkillDelete,
	}); err != nil {
		return err
	}
	if err := r.Register(Tool{
		Name:        "set_workspace",
		Description: "Point this conversation at a workspace (load it) or clear the pointer. A loaded workspace makes its skills available and is the search tool's default file scope. Omit workspace_id to report the current pointer; an empty string clears it. Switching unloads the previous workspace's loaded skills.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workspace_id": map[string]any{"type": "string", "description": "the workspace id to load, or empty to clear (omit to report)"},
			},
		},
		Execute: a.execSetWorkspace,
	}); err != nil {
		return err
	}
	return nil
}

// ---- executors ----

func (a *Agent) execSkillSearch(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca struct {
		Q     string `json:"q"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &ca); err != nil {
		return "invalid arguments: " + err.Error(), nil
	}
	q := strings.TrimSpace(ca.Q)
	if q == "" {
		return "q is required", nil
	}
	limit := ca.Limit
	if limit <= 0 {
		limit = searchToolDefaultTop
	}
	if limit > searchToolMaxTop {
		limit = searchToolMaxTop
	}
	terms := strings.Fields(strings.ToLower(q))
	// rank: 0 name hit, 1 description hit, 2 content-only hit.
	matchRank := func(f wsSkillFile) (int, bool) {
		hitName, hitDesc, hitContent := false, false, false
		for _, t := range terms {
			hitName = hitName || strings.Contains(strings.ToLower(f.Name), t)
			hitDesc = hitDesc || strings.Contains(strings.ToLower(f.Description), t)
			hitContent = hitContent || strings.Contains(strings.ToLower(f.Content), t)
		}
		switch {
		case hitName:
			return 0, true
		case hitDesc:
			return 1, true
		case hitContent:
			return 2, true
		default:
			return 0, false
		}
	}

	type hit struct {
		rank int
		f    wsSkillFile // name + description (the listing surface)
		ws   string      // "" for global
	}
	var hits []hit
	convID := conversationFromContext(ctx)
	loadedWS := a.loadedWorkspace(handle, convID)
	tierNote := ""
	if us, _ := a.userStoreOrResult(handle); us != nil {
		if skills, err := us.ListSkills(); err == nil {
			for _, sk := range skills {
				if r, ok := matchRank(wsSkillFile{Name: sk.Name, Description: sk.Description, Content: sk.Content}); ok {
					hits = append(hits, hit{rank: r, f: wsSkillFile{Name: sk.Name, Description: sk.Description}})
				}
			}
		}
	}
	if loadedWS != "" {
		for _, f := range listWsSkills(a.wsSkillsDir(handle, loadedWS)) {
			if r, ok := matchRank(f); ok {
				hits = append(hits, hit{rank: r, f: wsSkillFile{Name: f.Name, Description: f.Description}, ws: loadedWS})
			}
		}
	} else {
		tierNote = "(no workspace is loaded in this conversation — only global skills are available)"
	}
	if len(hits) == 0 {
		return fmt.Sprintf("SKILLS\n(no skill matches %q) %s", q, tierNote), nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return hits[i].f.Name < hits[j].f.Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	var b strings.Builder
	b.WriteString("SKILLS\n")
	for _, h := range hits {
		if h.ws == "" {
			fmt.Fprintf(&b, "- [global] %s: %s\n", h.f.Name, h.f.Description)
		} else {
			fmt.Fprintf(&b, "- [workspace %s] %s: %s\n", h.ws, h.f.Name, h.f.Description)
		}
	}
	if tierNote != "" {
		b.WriteString(tierNote + "\n")
	}
	b.WriteString("skill_load {name} for the full content (it then joins this conversation).")
	return b.String(), nil
}

func (a *Agent) execSkillLoad(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca struct {
		Name string `json:"name"`
		Tier string `json:"tier"`
	}
	if err := json.Unmarshal(args, &ca); err != nil {
		return "invalid arguments: " + err.Error(), nil
	}
	name := strings.TrimSpace(ca.Name)
	if name == "" {
		return "name is required", nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	convID := conversationFromContext(ctx)
	ref, why, ok := a.resolveSkillRef(handle, convID, name, ca.Tier, "")
	if !ok {
		return why, nil
	}
	f, found := a.skillContent(us, handle, ref)
	if !found {
		if ref.tier == "global" {
			return fmt.Sprintf("no global skill named %q — skill_search lists what exists", name), nil
		}
		return fmt.Sprintf("no skill named %q in workspace %s — skill_search lists what exists", name, ref.wsID), nil
	}
	fresh := a.addLoadedSkill(convID, ref.key())
	note := "it is now part of your context on every turn until skill_unload"
	if !fresh {
		note = "it was already loaded"
	}
	return fmt.Sprintf("loaded skill %q (%s) into this conversation — %s.\n\n%s\n\n%s", f.Name, skillTierLabel(ref), note, f.Description, f.Content), nil
}

func (a *Agent) execSkillUnload(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca struct {
		Name string `json:"name"`
		Tier string `json:"tier"`
	}
	if err := json.Unmarshal(args, &ca); err != nil {
		return "invalid arguments: " + err.Error(), nil
	}
	name := strings.TrimSpace(ca.Name)
	if name == "" {
		return "name is required", nil
	}
	convID := conversationFromContext(ctx)
	loadedWS := a.loadedWorkspace(handle, convID)
	// Try both possible keys so the model never needs the tier when only
	// one tier holds the name.
	var tried []string
	if ca.Tier == "workspace" {
		if loadedWS != "" {
			tried = append(tried, skillRef{name: name, tier: "workspace", wsID: loadedWS}.key())
		}
	} else {
		if loadedWS != "" {
			tried = append(tried, skillRef{name: name, tier: "workspace", wsID: loadedWS}.key())
		}
		tried = append(tried, skillRef{name: name, tier: "global"}.key())
	}
	for _, key := range tried {
		if a.removeLoadedSkill(convID, key) {
			return fmt.Sprintf("unloaded skill %q (%s) from this conversation — its content is no longer injected", name, keyTierLabel(key)), nil
		}
	}
	return fmt.Sprintf("skill %q is not loaded in this conversation (nothing to unload)", name), nil
}

func keyTierLabel(key string) string {
	if strings.HasPrefix(key, "global:") {
		return "global"
	}
	rest := strings.TrimPrefix(key, "ws:")
	if wsID, _, ok := strings.Cut(rest, ":"); ok {
		return fmt.Sprintf("workspace %s", wsID)
	}
	return key
}

func (a *Agent) execSkillCreate(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
		Tier        string `json:"tier"`
		Workspace   string `json:"workspace"`
	}
	if err := json.Unmarshal(args, &ca); err != nil {
		return "invalid arguments: " + err.Error(), nil
	}
	name := strings.TrimSpace(ca.Name)
	desc := strings.TrimSpace(ca.Description)
	content := strings.TrimSpace(ca.Content)
	if name == "" || len(name) > 120 {
		return "name is required (1-120 chars)", nil
	}
	if desc == "" || len(desc) > 500 {
		return "description is required (1-500 chars, one line: what it is + when to use it)", nil
	}
	if content == "" {
		return "content is required", nil
	}
	if a.diskIsFrozen(handle) {
		return "the user is over their disk cap — creating skills is paused. Ask them to free up disk space first.", nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	convID := conversationFromContext(ctx)

	tier, wsID := ca.Tier, ca.Workspace
	if tier == "" {
		if wsID == "" {
			// The default: workspace-local while a workspace is loaded, else
			// global (the Skills decision's reflection preference).
			if loaded := a.loadedWorkspace(handle, convID); loaded != "" {
				tier, wsID = "workspace", loaded
			} else {
				tier = "global"
			}
		} else {
			tier = "workspace"
		}
	}
	switch tier {
	case "global":
		if _, err := us.GetSkillByName(name); err == nil {
			return fmt.Sprintf("a global skill named %q already exists — skill_edit it instead", name), nil
		}
		if _, err := us.CreateSkill(name, desc, content, nil); err != nil {
			return "could not save the skill: " + err.Error(), nil
		}
		return fmt.Sprintf("created global skill %q — available to all of the user's conversations via skill_search", name), nil
	case "workspace":
		if wsID == "" {
			if loaded := a.loadedWorkspace(handle, convID); loaded != "" {
				wsID = loaded
			} else {
				return "no workspace is loaded in this conversation and none was named — set the workspace first (set_workspace) or use tier=global", nil
			}
		}
		if _, err := us.GetWorkspace(wsID); err != nil {
			return fmt.Sprintf("workspace %s does not exist — name one of the user's workspaces (or omit the tier for the default)", wsID), nil
		}
		dir := a.wsSkillsDir(handle, wsID)
		if wsSkillExists(dir, name) {
			return fmt.Sprintf("a skill named %q already exists in workspace %s — skill_edit it instead", name, wsID), nil
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if err := writeWsSkill(dir, wsSkillFile{Name: name, Description: desc, Content: content, CreatedAt: now, UpdatedAt: now}); err != nil {
			return "could not write the skill file: " + err.Error(), nil
		}
		return fmt.Sprintf("created skill %q in workspace %s — available while that workspace is loaded", name, wsID), nil
	default:
		return `tier must be "global" or "workspace"`, nil
	}
}

func (a *Agent) execSkillEdit(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
		Tier        string `json:"tier"`
		Workspace   string `json:"workspace"`
	}
	if err := json.Unmarshal(args, &ca); err != nil {
		return "invalid arguments: " + err.Error(), nil
	}
	name := strings.TrimSpace(ca.Name)
	if name == "" {
		return "name is required", nil
	}
	if ca.Description == "" && ca.Content == "" {
		return "give a new description and/or content (at least one)", nil
	}
	if ca.Description != "" && len(ca.Description) > 500 {
		return "description is too long (<=500 chars)", nil
	}
	if a.diskIsFrozen(handle) {
		return "the user is over their disk cap — editing skills is paused. Ask them to free up disk space first.", nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	convID := conversationFromContext(ctx)
	ref, why, ok := a.resolveSkillRef(handle, convID, name, ca.Tier, ca.Workspace)
	if !ok {
		return why, nil
	}
	if ref.tier == "workspace" && ca.Workspace != "" {
		if _, err := us.GetWorkspace(ca.Workspace); err != nil {
			return fmt.Sprintf("workspace %s does not exist — name one of the user's workspaces", ca.Workspace), nil
		}
	}
	if ref.tier == "global" {
		sk, err := us.GetSkillByName(name)
		if err != nil {
			return fmt.Sprintf("no global skill named %q — skill_create it first", name), nil
		}
		if ca.Description != "" {
			sk.Description = ca.Description
		}
		if ca.Content != "" {
			sk.Content = ca.Content
		}
		if err := us.UpdateSkill(name, sk.Description, sk.Content); err != nil {
			return "could not save the skill: " + err.Error(), nil
		}
		return fmt.Sprintf("updated global skill %q", name), nil
	}
	dir := a.wsSkillsDir(handle, ref.wsID)
	var f wsSkillFile
	found := false
	for _, g := range listWsSkills(dir) {
		if g.Name == name {
			f, found = g, true
			break
		}
	}
	if !found {
		return fmt.Sprintf("no skill named %q in workspace %s — skill_create it first", name, ref.wsID), nil
	}
	if ca.Description != "" {
		f.Description = ca.Description
	}
	if ca.Content != "" {
		f.Content = ca.Content
	}
	f.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeWsSkill(dir, f); err != nil {
		return "could not write the skill file: " + err.Error(), nil
	}
	return fmt.Sprintf("updated skill %q in workspace %s", name, ref.wsID), nil
}

func (a *Agent) execSkillDelete(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca struct {
		Name      string `json:"name"`
		Tier      string `json:"tier"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(args, &ca); err != nil {
		return "invalid arguments: " + err.Error(), nil
	}
	name := strings.TrimSpace(ca.Name)
	if name == "" {
		return "name is required", nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	convID := conversationFromContext(ctx)
	wsID := ca.Workspace
	if wsID == "" {
		wsID = a.loadedWorkspace(handle, convID)
	}
	if wsID != "" {
		if _, err := us.GetWorkspace(wsID); err != nil {
			return fmt.Sprintf("workspace %s does not exist — name one of the user's workspaces", wsID), nil
		}
	}
	dir := func(id string) string { return a.wsSkillsDir(handle, id) }

	tryGlobal := ca.Tier == "global" || ca.Tier == ""
	tryWS := (ca.Tier == "workspace" && wsID != "") || (ca.Tier == "" && wsID != "")
	if tryWS && !wsSkillExists(dir(wsID), name) {
		tryWS = false
	}
	if tryGlobal && wsID != "" && wsSkillExists(dir(wsID), name) && ca.Tier == "" {
		// auto-resolution: the workspace skill wins (same rule as load).
		tryGlobal = false
	}
	if tryGlobal {
		err := us.DeleteSkill(name)
		if err == nil {
			a.removeLoadedSkill(convID, skillRef{name: name, tier: "global"}.key())
			return fmt.Sprintf("deleted global skill %q", name), nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "could not delete the skill: " + err.Error(), nil
		}
		if ca.Tier == "global" {
			return fmt.Sprintf("no global skill named %q", name), nil
		}
	}
	if tryWS {
		if err := os.Remove(wsSkillFileFor(dir(wsID), name)); err != nil {
			return fmt.Sprintf("no skill named %q in workspace %s", name, wsID), nil
		}
		a.removeLoadedSkill(convID, skillRef{name: name, tier: "workspace", wsID: wsID}.key())
		return fmt.Sprintf("deleted skill %q from workspace %s", name, wsID), nil
	}
	return fmt.Sprintf("no skill named %q to delete (no global skill by that name%s)", name, loadedWSTail(wsID)), nil
}

func loadedWSTail(wsID string) string {
	if wsID == "" {
		return ""
	}
	return fmt.Sprintf(" or in workspace %s", wsID)
}

func (a *Agent) execSetWorkspace(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca struct {
		WorkspaceID *string `json:"workspace_id"`
	}
	if err := json.Unmarshal(args, &ca); err != nil {
		return "invalid arguments: " + err.Error(), nil
	}
	convID := conversationFromContext(ctx)
	if convID == "" {
		return "no conversation context for set_workspace", nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		return "could not read this conversation: " + err.Error(), nil
	}
	wsTitle := func(id string) string {
		if ws, err := us.GetWorkspace(id); err == nil {
			return ws.Title
		}
		return id
	}

	if ca.WorkspaceID == nil { // report the current pointer
		if conv.WorkspaceID == "" {
			return "no workspace is loaded in this conversation", nil
		}
		return fmt.Sprintf("this conversation works in workspace %s (\"%s\")", conv.WorkspaceID, wsTitle(conv.WorkspaceID)), nil
	}
	target := strings.TrimSpace(*ca.WorkspaceID)
	if target != "" {
		if _, err := us.GetWorkspace(target); err != nil {
			return fmt.Sprintf("workspace %s does not exist — name one of the user's workspaces", target), nil
		}
	}
	if target == conv.WorkspaceID {
		if target == "" {
			return "no workspace is loaded in this conversation (no change)", nil
		}
		return fmt.Sprintf("this conversation already works in workspace %s (\"%s\") — no change", target, wsTitle(target)), nil
	}
	if err := us.SetConversationWorkspace(convID, target); err != nil {
		return "could not update the conversation: " + err.Error(), nil
	}
	evicted := []string{}
	if conv.WorkspaceID != "" {
		evicted = a.evictWorkspaceSkills(convID, conv.WorkspaceID)
	}
	if target == "" {
		out := "the workspace was cleared — this conversation now has no workspace"
		if len(evicted) > 0 {
			out += fmt.Sprintf("; its previously loaded skills were unloaded (%s)", strings.Join(evicted, ", "))
		}
		return out + ".", nil
	}
	out := fmt.Sprintf("this conversation now works in workspace %s (\"%s\") — its skills are available (skill_search), and search's default file scope is now this workspace", target, wsTitle(target))
	if len(evicted) > 0 {
		out += fmt.Sprintf("; skills previously loaded from %s were unloaded (%s)", conv.WorkspaceID, strings.Join(evicted, ", "))
	}
	return out + ".", nil
}
