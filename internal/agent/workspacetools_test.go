package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workspace tools (the design log's WORKSPACE TOOLS section): the matrix
// is create (folder + .SKILLS/ + .dni/ + the row, trimmed + capped
// validation, the disk-frozen refusal), edit (the description — the only
// user-adjustable metadata in v1 — plus the not-found + cap results),
// delete (folder + row + the wired re-check), list (empty + content), and
// the wire (all four offered to a plain conversation). The folders are REAL
// directories under the test data dir (the tools are host-side).

func wsToolRun(t *testing.T, a *Agent, fn func(context.Context, string, json.RawMessage) (string, error), handle, args string) string {
	t.Helper()
	out, err := fn(context.Background(), handle, []byte(args))
	if err != nil {
		t.Fatalf("tool(%s): %v", args, err)
	}
	return out
}

func TestWorkspaceToolsCRUD(t *testing.T) {
	a, _, dir := newTestAgent(t)
	defer a.Close()
	handle := "u1"

	// The post-delete re-check: record it (the server wires the real one).
	rechecked := make(chan string, 1)
	a.SetDiskRecheck(func(h string) { rechecked <- h })

	// list: empty.
	out := wsToolRun(t, a, a.execWorkspaceList, handle, "{}")
	if !strings.Contains(out, "no workspaces") {
		t.Fatalf("empty list = %s", out)
	}

	// create: trimmed title + description; the folder (with .SKILLS/ +
	// .dni/) must exist before the row does.
	out = wsToolRun(t, a, a.execWorkspaceCreate, handle, `{"title":"  Proj  ","description":"  the thing  "}`)
	if !strings.Contains(out, "created workspace") {
		t.Fatalf("create = %s", out)
	}
	us, err := a.userStore(handle)
	if err != nil {
		t.Fatal(err)
	}
	wss, err := us.ListWorkspaces()
	if err != nil || len(wss) != 1 {
		t.Fatalf("ListWorkspaces = %+v, %v", wss, err)
	}
	ws := wss[0]
	if ws.Title != "Proj" || ws.Description != "the thing" {
		t.Fatalf("row = %+v (trim failed)", ws)
	}
	folder := filepath.Join(dir, "users", handle, "workspaces", ws.ID)
	if ws.FolderPath != folder {
		t.Fatalf("folder_path = %s, want %s", ws.FolderPath, folder)
	}
	for _, sub := range []string{folder, filepath.Join(folder, ".SKILLS"), filepath.Join(folder, ".dni")} {
		if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
			t.Fatalf("missing folder %s: %v", sub, err)
		}
	}

	// create: validation (the same caps as the server's endpoint).
	out = wsToolRun(t, a, a.execWorkspaceCreate, handle, `{"title":""}`)
	if !strings.Contains(out, "non-empty") {
		t.Fatalf("blank title = %s", out)
	}
	out = wsToolRun(t, a, a.execWorkspaceCreate, handle, `{"title":"`+strings.Repeat("x", wsTitleMax+1)+`"}`)
	if !strings.Contains(out, "title too long") {
		t.Fatalf("long title = %s", out)
	}
	out = wsToolRun(t, a, a.execWorkspaceCreate, handle, `{"title":"ok","description":"`+strings.Repeat("y", wsDescriptionMax+1)+`"}`)
	if !strings.Contains(out, "description too long") {
		t.Fatalf("long description = %s", out)
	}
	if n, _ := us.ListWorkspaces(); len(n) != 1 {
		t.Fatalf("rejected creates must not leave rows: %d", len(n))
	}

	// create while disk-frozen: refused (a create is a data-dir write).
	a.sched.SetFrozen(func(string) bool { return true })
	out = wsToolRun(t, a, a.execWorkspaceCreate, handle, `{"title":"frozen"}`)
	if !strings.Contains(out, "disk cap") {
		t.Fatalf("frozen create = %s", out)
	}
	a.sched.SetFrozen(nil)
	if n, _ := us.ListWorkspaces(); len(n) != 1 {
		t.Fatalf("frozen create left a row: %d", len(n))
	}

	// list: the content line (id | title — description).
	out = wsToolRun(t, a, a.execWorkspaceList, handle, "{}")
	if !strings.Contains(out, ws.ID+" | Proj — the thing") {
		t.Fatalf("list = %s", out)
	}

	// edit: the description (v1's only user-adjustable field).
	out = wsToolRun(t, a, a.execWorkspaceEdit, handle, `{"id":"`+ws.ID+`","description":"  v2  "}`)
	if !strings.Contains(out, "updated the description") {
		t.Fatalf("edit = %s", out)
	}
	if got, _ := us.GetWorkspace(ws.ID); got.Description != "v2" {
		t.Fatalf("description = %q, want trimmed v2", got.Description)
	}

	// edit: unknown id.
	out = wsToolRun(t, a, a.execWorkspaceEdit, handle, `{"id":"nope","description":"x"}`)
	if !strings.Contains(out, "no workspace with id") {
		t.Fatalf("edit unknown = %s", out)
	}

	// edit: cap + clear.
	out = wsToolRun(t, a, a.execWorkspaceEdit, handle, `{"id":"`+ws.ID+`","description":"`+strings.Repeat("y", wsDescriptionMax+1)+`"}`)
	if !strings.Contains(out, "description too long") {
		t.Fatalf("long edit = %s", out)
	}
	out = wsToolRun(t, a, a.execWorkspaceEdit, handle, `{"id":"`+ws.ID+`","description":""}`)
	if !strings.Contains(out, "cleared the description") {
		t.Fatalf("clear = %s", out)
	}
	if got, _ := us.GetWorkspace(ws.ID); got.Description != "" {
		t.Fatalf("description = %q, want empty", got.Description)
	}

	// delete: folder + row gone, re-check fired.
	out = wsToolRun(t, a, a.execWorkspaceDelete, handle, `{"id":"`+ws.ID+`"}`)
	if !strings.Contains(out, "deleted workspace") {
		t.Fatalf("delete = %s", out)
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatalf("folder still exists after delete")
	}
	if n, _ := us.ListWorkspaces(); len(n) != 0 {
		t.Fatalf("rows left after delete: %d", len(n))
	}
	select {
	case h := <-rechecked:
		if h != handle {
			t.Fatalf("recheck handle = %q, want %q", h, handle)
		}
	default:
		t.Fatal("the post-delete re-check never ran")
	}

	// delete: unknown id.
	out = wsToolRun(t, a, a.execWorkspaceDelete, handle, `{"id":"nope"}`)
	if !strings.Contains(out, "no workspace with id") {
		t.Fatalf("delete unknown = %s", out)
	}
}

// TestWorkspaceToolsWire: all four tools are offered to a plain
// conversation (unconditional registration — no admin toggle for a user's
// own workspaces).
func TestWorkspaceToolsWire(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	chat, err := a.CreateConversation(context.Background(), "u1", "chat", "", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := us.GetConversation(chat)
	if err != nil {
		t.Fatal(err)
	}
	wire := a.wireTools(us, conv)
	for _, name := range []string{"workspace_list", "workspace_create", "workspace_edit", "workspace_delete"} {
		if !wireOffers(wire, name) {
			t.Fatalf("%s not offered to a plain conversation", name)
		}
	}
}
