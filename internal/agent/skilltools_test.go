package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The skill system (the design log's SKILL SYSTEM + WORKSPACE LOADING
// sections): the six skill tools + set_workspace — search over the two
// tiers (global always, workspace-local while loaded), load/unload as the
// per-conversation active set (content re-injected into the request),
// create/edit/delete (the disk-freeze gate, the ws .SKILLS file layout),
// the workspace pointer (set/clear/report, the switch eviction rule), and
// the search tool's default file scope (the loaded workspace).

func skToolRun(t *testing.T, a *Agent, ctx context.Context, fn func(context.Context, string, json.RawMessage) (string, error), handle, args string) string {
	t.Helper()
	out, err := fn(ctx, handle, []byte(args))
	if err != nil {
		t.Fatalf("tool(%s): %v", args, err)
	}
	return out
}

func skCtx(t *testing.T, convID string) context.Context {
	t.Helper()
	if convID == "" {
		t.Fatal("skCtx: empty conversation id")
	}
	return context.WithValue(context.Background(), ctxConversationKey, convID)
}

func TestSkillSearchLoadUnload(t *testing.T) {
	a, _, dir := newTestAgent(t)
	defer a.Close()
	convID, us := newTestConversation(t, a, "u1")
	ctx := skCtx(t, convID)

	// A global skill (P10) and a workspace workspace-local skill (.SKILLS
	// file).
	if _, err := us.CreateSkill("deploy-cli", "deploys the app", "run the deploy script", nil); err != nil {
		t.Fatal(err)
	}
	out := skToolRun(t, a, ctx, a.execWorkspaceCreate, "u1", `{"title":"W1","description":""}`)
	if !strings.Contains(out, "created workspace") {
		t.Fatalf("workspace create = %s", out)
	}
	wss, err := us.ListWorkspaces()
	if err != nil || len(wss) != 1 {
		t.Fatalf("workspaces = %+v, %v", wss, err)
	}
	w1 := wss[0].ID
	if err := writeWsSkill(a.wsSkillsDir("u1", w1), wsSkillFile{Name: "build-notes", Description: "the build recipe", Content: "make && test"}); err != nil {
		t.Fatal(err)
	}

	// search: only the global tier is available (no workspace loaded), and
	// the result says so.
	out = skToolRun(t, a, ctx, a.execSkillSearch, "u1", `{"q":"the"}`)
	if !strings.Contains(out, "[global] deploy-cli") {
		t.Fatalf("search without workspace = %s (want the global hit)", out)
	}
	if strings.Contains(out, "build-notes") {
		t.Fatalf("workspace skill visible with no workspace loaded: %s", out)
	}
	if !strings.Contains(out, "no workspace is loaded") {
		t.Fatalf("missing the availability note: %s", out)
	}

	// Load the workspace: both tiers come into view.
	out = skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":"`+w1+`"}`)
	if !strings.Contains(out, "works in workspace "+w1) {
		t.Fatalf("set_workspace = %s", out)
	}
	out = skToolRun(t, a, ctx, a.execSkillSearch, "u1", `{"q":"the"}`)
	if !strings.Contains(out, "[global] deploy-cli") || !strings.Contains(out, "[workspace "+w1+"] build-notes") {
		t.Fatalf("search with workspace = %s (want both tiers)", out)
	}

	// load: the auto tier resolves to the loaded workspace; the content
	// comes back AND joins the active set (re-injected into the request).
	out = skToolRun(t, a, ctx, a.execSkillLoad, "u1", `{"name":"build-notes"}`)
	if !strings.Contains(out, "make && test") || !strings.Contains(out, "workspace "+w1) {
		t.Fatalf("skill_load = %s", out)
	}
	keys := a.activeSkillKeys(convID)
	if len(keys) != 1 || keys[0] != "ws:"+w1+":build-notes" {
		t.Fatalf("active keys = %v", keys)
	}
	conv := a.activeSkillsFor(us, "u1", convID)
	if conv == nil {
		t.Fatal("activeSkillsFor returned nil")
	}
	if !strings.Contains(conv.FrozenSystemPrompt, "Loaded skills (this conversation)") || !strings.Contains(conv.FrozenSystemPrompt, "the build recipe") {
		t.Fatalf("the request prompt lacks the loaded skill:\n%s", conv.FrozenSystemPrompt)
	}
	// The frozen ROW on disk is untouched (only the request copy carries
	// the section).
	row, err := us.GetConversation(convID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.FrozenSystemPrompt, "Loaded skills") {
		t.Fatal("the frozen row was modified by a load (it must stay clean)")
	}

	// load a second (global) skill; load is idempotent.
	out = skToolRun(t, a, ctx, a.execSkillLoad, "u1", `{"name":"deploy-cli"}`)
	if !strings.Contains(out, "run the deploy script") {
		t.Fatalf("global load = %s", out)
	}
	if n := len(a.activeSkillKeys(convID)); n != 2 {
		t.Fatalf("active keys = %d, want 2", n)
	}
	skToolRun(t, a, ctx, a.execSkillLoad, "u1", `{"name":"deploy-cli"}`)
	if n := len(a.activeSkillKeys(convID)); n != 2 {
		t.Fatalf("idempotent load failed: %d keys", n)
	}

	// SWITCH: the previous workspace's loaded skills are evicted (the user
	// decision); globals persist.
	skToolRun(t, a, ctx, a.execWorkspaceCreate, "u1", `{"title":"W2","description":""}`)
	wss, _ = us.ListWorkspaces()
	var w2 string
	for _, w := range wss {
		if w.ID != w1 {
			w2 = w.ID
		}
	}
	if w2 == "" {
		t.Fatal("second workspace missing")
	}
	if err := writeWsSkill(a.wsSkillsDir("u1", w2), wsSkillFile{Name: "w2-note", Description: "a w2 note", Content: "w2 body"}); err != nil {
		t.Fatal(err)
	}
	out = skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":"`+w2+`"}`)
	if !strings.Contains(out, "were unloaded") {
		t.Fatalf("switch result missing the eviction: %s", out)
	}
	keys = a.activeSkillKeys(convID)
	if len(keys) != 1 || keys[0] != "global:deploy-cli" {
		t.Fatalf("after switch keys = %v (want only the global)", keys)
	}

	// unload drops a skill from the set.
	out = skToolRun(t, a, ctx, a.execSkillUnload, "u1", `{"name":"deploy-cli"}`)
	if !strings.Contains(out, "no longer injected") {
		t.Fatalf("skill_unload = %s", out)
	}
	if n := len(a.activeSkillKeys(convID)); n != 0 {
		t.Fatalf("after unload keys = %d, want 0", n)
	}
	out = skToolRun(t, a, ctx, a.execSkillUnload, "u1", `{"name":"deploy-cli"}`)
	if !strings.Contains(out, "not loaded") {
		t.Fatalf("unload of an unloaded skill = %s", out)
	}

	// CLEAR: the pointer goes empty and the loaded workspace skills are
	// evicted too.
	skToolRun(t, a, ctx, a.execSkillLoad, "u1", `{"name":"w2-note"}`)
	out = skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":""}`)
	if !strings.Contains(out, "cleared") || !strings.Contains(out, "unloaded") {
		t.Fatalf("clear = %s", out)
	}
	if row, _ := us.GetConversation(convID); row.WorkspaceID != "" {
		t.Fatalf("pointer = %q after clear, want empty", row.WorkspaceID)
	}
	if n := len(a.activeSkillKeys(convID)); n != 0 {
		t.Fatalf("keys after clear = %d, want 0", n)
	}
	_ = dir
}

func TestSkillCreateEditDelete(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	convID, us := newTestConversation(t, a, "u1")
	ctx := skCtx(t, convID)

	// create: the default tier is GLOBAL with no workspace loaded.
	out := skToolRun(t, a, ctx, a.execSkillCreate, "u1", `{"name":"g-skill","description":"a global one","content":"g body"}`)
	if !strings.Contains(out, "created global skill") {
		t.Fatalf("create = %s", out)
	}
	if _, err := us.GetSkillByName("g-skill"); err != nil {
		t.Fatalf("global row missing: %v", err)
	}
	out = skToolRun(t, a, ctx, a.execSkillCreate, "u1", `{"name":"g-skill","description":"x","content":"y"}`)
	if !strings.Contains(out, "already exists") {
		t.Fatalf("duplicate = %s", out)
	}

	// create: the default tier flips to WORKSPACE-LOCAL once a workspace is
	// loaded; the file lands in .SKILLS with the mirrored fields.
	skToolRun(t, a, ctx, a.execWorkspaceCreate, "u1", `{"title":"W1","description":""}`)
	wss, err := us.ListWorkspaces()
	if err != nil || len(wss) != 1 {
		t.Fatalf("workspaces = %+v, %v", wss, err)
	}
	w1 := wss[0].ID
	skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":"`+w1+`"}`)
	out = skToolRun(t, a, ctx, a.execSkillCreate, "u1", `{"name":"Ws Skill","description":"a local one","content":"ws body"}`)
	if !strings.Contains(out, "created skill \"Ws Skill\" in workspace "+w1) {
		t.Fatalf("create local = %s", out)
	}
	data, err := os.ReadFile(filepath.Join(a.wsSkillsDir("u1", w1), "ws-skill.json"))
	if err != nil {
		t.Fatalf("skill file missing: %v (slug wrong?)", err)
	}
	var f wsSkillFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Name != "Ws Skill" || f.Description != "a local one" || f.Content != "ws body" || f.CreatedAt == "" {
		t.Fatalf("file = %+v", f)
	}

	// create: tier=workspace with NO loaded workspace needs the id named;
	// a bad id is a structured result.
	skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":""}`)
	out = skToolRun(t, a, ctx, a.execSkillCreate, "u1", `{"name":"named-ws","description":"x","content":"y","tier":"workspace","workspace":"`+w1+`"}`)
	if !strings.Contains(out, "created skill") {
		t.Fatalf("named-workspace create = %s", out)
	}
	out = skToolRun(t, a, ctx, a.execSkillCreate, "u1", `{"name":"bad","description":"x","content":"y","tier":"workspace","workspace":"nope"}`)
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("bad workspace = %s", out)
	}

	// The disk-freeze gate: create AND edit are refused while the user is
	// over the cap.
	a.sched.SetFrozen(func(h string) bool { return true })
	out = skToolRun(t, a, ctx, a.execSkillCreate, "u1", `{"name":"frozen","description":"x","content":"y"}`)
	if !strings.Contains(out, "over their disk cap") {
		t.Fatalf("frozen create = %s", out)
	}
	out = skToolRun(t, a, ctx, a.execSkillEdit, "u1", `{"name":"g-skill","content":"nope"}`)
	if !strings.Contains(out, "over their disk cap") {
		t.Fatalf("frozen edit = %s", out)
	}
	a.sched.SetFrozen(func(h string) bool { return false })

	// edit: one field at a time; unknown skills are named.
	out = skToolRun(t, a, ctx, a.execSkillEdit, "u1", `{"name":"g-skill","description":"a global one (edited)"}`)
	if !strings.Contains(out, "updated global skill") {
		t.Fatalf("edit global = %s", out)
	}
	sk, err := us.GetSkillByName("g-skill")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Description != "a global one (edited)" || sk.Content != "g body" {
		t.Fatalf("after edit = %+v (content must be preserved)", sk)
	}
	// The pointer was cleared above — the ws skill is reached by NAMING its
	// workspace (the edit's workspace arg).
	out = skToolRun(t, a, ctx, a.execSkillEdit, "u1", `{"name":"Ws Skill","content":"ws body v2","tier":"workspace","workspace":"`+w1+`"}`)
	if !strings.Contains(out, "updated skill") {
		t.Fatalf("edit ws = %s", out)
	}
	if f, ok := a.skillContent(us, "u1", skillRef{name: "Ws Skill", tier: "workspace", wsID: w1}); !ok || f.Content != "ws body v2" {
		t.Fatalf("ws skill after edit = %+v", f)
	}
	out = skToolRun(t, a, ctx, a.execSkillEdit, "u1", `{"name":"ghost","content":"x"}`)
	if !strings.Contains(out, "no global skill named") {
		t.Fatalf("edit unknown = %s", out)
	}
	out = skToolRun(t, a, ctx, a.execSkillEdit, "u1", `{"name":"g-skill"}`)
	if !strings.Contains(out, "at least one") {
		t.Fatalf("edit empty = %s", out)
	}

	// delete: global row, ws file; deleting a LOADED skill also unloads it.
	// (Loading a ws skill needs the pointer set — the availability rule —
	// so point it back.)
	skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":"`+w1+`"}`)
	skToolRun(t, a, ctx, a.execSkillLoad, "u1", `{"name":"Ws Skill"}`)
	out = skToolRun(t, a, ctx, a.execSkillDelete, "u1", `{"name":"Ws Skill"}`)
	if !strings.Contains(out, "deleted skill") {
		t.Fatalf("delete ws = %s", out)
	}
	if _, err := os.Stat(filepath.Join(a.wsSkillsDir("u1", w1), "ws-skill.json")); !os.IsNotExist(err) {
		t.Fatalf("ws skill file still present: %v", err)
	}
	if n := len(a.activeSkillKeys(convID)); n != 0 {
		t.Fatalf("deleted skill still in the active set: %d keys", n)
	}
	out = skToolRun(t, a, ctx, a.execSkillDelete, "u1", `{"name":"g-skill"}`)
	if !strings.Contains(out, "deleted global skill") {
		t.Fatalf("delete global = %s", out)
	}
	if _, err := us.GetSkillByName("g-skill"); err == nil {
		t.Fatal("global row still present after delete")
	}
	out = skToolRun(t, a, ctx, a.execSkillDelete, "u1", `{"name":"ghost"}`)
	if !strings.Contains(out, "no skill named") {
		t.Fatalf("delete unknown = %s", out)
	}
}

func TestSetWorkspaceTool(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	convID, us := newTestConversation(t, a, "u1")
	ctx := skCtx(t, convID)

	out := skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{}`)
	if !strings.Contains(out, "no workspace is loaded") {
		t.Fatalf("report (none) = %s", out)
	}
	out = skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":"nope"}`)
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("bad id = %s", out)
	}
	skToolRun(t, a, ctx, a.execWorkspaceCreate, "u1", `{"title":"W1","description":""}`)
	wss, err := us.ListWorkspaces()
	if err != nil || len(wss) != 1 {
		t.Fatalf("workspaces = %+v, %v", wss, err)
	}
	w1 := wss[0].ID
	out = skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":"`+w1+`"}`)
	if !strings.Contains(out, `works in workspace `+w1+` ("W1")`) {
		t.Fatalf("set = %s", out)
	}
	out = skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{}`)
	if !strings.Contains(out, "works in workspace "+w1) {
		t.Fatalf("report (set) = %s", out)
	}
	out = skToolRun(t, a, ctx, a.execSetWorkspace, "u1", `{"workspace_id":"`+w1+`"}`)
	if !strings.Contains(out, "no change") {
		t.Fatalf("same set = %s", out)
	}
}

// TestSearchDefaultWorkspaceScope — the WORKSPACE LOADING section's second
// derived effect: with a workspace loaded, the search tool's file leg
// defaults to it (an explicit workspace arg still overrides; no loaded
// workspace = today's behavior, no file leg).
func TestSearchDefaultWorkspaceScope(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	convA, convB, wsID, _, _, _ := seedSearchFixtures(t, a, "u1")
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetConversationWorkspace(convA, wsID); err != nil {
		t.Fatal(err)
	}

	tl, ok := a.tools.Get("search")
	if !ok {
		t.Fatal("tool \"search\" not registered")
	}
	run := func(convID string, args map[string]any) string {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if convID != "" {
			ctx = context.WithValue(ctx, ctxConversationKey, convID)
		}
		out, err := tl.Execute(ctx, "u1", raw)
		if err != nil {
			t.Fatalf("search(%v): %v", args, err)
		}
		return out
	}

	// Loaded conversation: the file leg runs against the loaded workspace
	// with NO explicit workspace arg.
	out := run(convA, map[string]any{"q": "zephyr"})
	if !strings.Contains(out, "scoped to workspace "+wsID) || !strings.Contains(out, "zephyr-notes.md") {
		t.Fatalf("default scope = %s", out)
	}
	// The un-loaded conversation: no file leg (today's behavior).
	out = run(convB, map[string]any{"q": "zephyr"})
	if strings.Contains(out, "WORKSPACE FILES") {
		t.Fatalf("no loaded workspace must not add a file leg: %s", out)
	}
	// An explicit arg still overrides (and the no-context fallback keeps
	// today's behavior: no file leg without a named workspace).
	out = run(convA, map[string]any{"q": "zephyr", "workspace": "nope"})
	if !strings.Contains(out, "does not exist") && !strings.Contains(out, "unknown workspace") {
		t.Fatalf("bad explicit workspace = %s", out)
	}
	out = run("", map[string]any{"q": "zephyr"})
	if strings.Contains(out, "WORKSPACE FILES") {
		t.Fatalf("no conversation context must not add a file leg: %s", out)
	}
}
