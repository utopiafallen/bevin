package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/utopiafallen/bevin/internal/store"
)

// Workspace management tools (the design log's WORKSPACE TOOLS section):
// the agent manages workspace STATE the same way it manages the user's
// environment — creating, editing, and deleting workspaces on the user's
// behalf when asked. Workspaces are HOST-SIDE folders in the user's data
// dir (the agent's dataDir is the same base the server uses:
// <data>/users/<handle>/workspaces/<id>), so no container is involved —
// a server without the container runtime still manages workspace state.
// All failure modes are RESULTS back to the model, never a turn error.
//
// workspace_edit changes the USER-ADJUSTABLE metadata; v1 that set is the
// description (the title is the creation-time identity, fixed on both the
// agent's and the user's surfaces — the EDIT MIRROR: the agent's tool and
// the user's UI edit write the same row with the same validation).

// The validation caps — the SAME limits as the server's workspace API
// (POST /api/workspaces, PATCH /api/workspaces/{id}); the two surfaces
// must agree or "user-adjustable" stops being true from one side.
const (
	wsTitleMax       = 120
	wsDescriptionMax = 500
)

type wsCreateArgs struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

type wsEditArgs struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type wsDeleteArgs struct {
	ID string `json:"id"`
}

func registerWorkspaceTools(r *ToolRegistry, a *Agent) error {
	tools := []Tool{
		{
			Name:        "workspace_list",
			Description: "List the user's workspaces (id, title, description). A workspace is a persistent folder in the user's environment (workspaces/<id> under the mapped data dir) where their files for a body of work live.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Execute: a.execWorkspaceList,
		},
		{
			Name:        "workspace_create",
			Description: "Create a new workspace: a persistent folder in the user's environment plus its metadata row, for a distinct body of work. Returns the id (the environment folder is workspaces/<id> under the mapped data dir).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{
						"type":        "string",
						"description": "short name for the workspace (max 120)",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "one or two lines on what the workspace is for (max 500; optional)",
					},
				},
				"required": []string{"title"},
			},
			Execute: a.execWorkspaceCreate,
		},
		{
			Name:        "workspace_edit",
			Description: "Update a workspace's description (its only user-adjustable metadata; the title is fixed at creation).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "the workspace id (from workspace_list)",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "the new description (max 500; empty clears it)",
					},
				},
				"required": []string{"id", "description"},
			},
			Execute: a.execWorkspaceEdit,
		},
		{
			Name:        "workspace_delete",
			Description: "Delete a workspace: its folder (every file in it) and its metadata. Irreversible — only when the user asked for the workspace itself to go.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "the workspace id (from workspace_list)",
					},
				},
				"required": []string{"id"},
			},
			Execute: a.execWorkspaceDelete,
		},
	}
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// workspaceFolderRoot is the user's workspaces root (the same layout the
// server's workspace API uses).
func (a *Agent) workspaceFolderRoot(handle string) string {
	return filepath.Join(a.dataDir, "users", handle, "workspaces")
}

// workspaceDataDirHint is the mapped data dir as seen INSIDE the user's
// environment (where the model points exec/edit_file): /data on a linux
// host, C:\data on windows (the container platform follows the host).
func workspaceDataDirHint() string {
	if runtime.GOOS == "windows" {
		return `C:\data`
	}
	return "/data"
}

func (a *Agent) userStoreOrResult(handle string) (*store.UserStore, string) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, "could not open the user's store: " + err.Error()
	}
	return us, ""
}

// execWorkspaceList — workspace_list.
func (a *Agent) execWorkspaceList(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	wss, err := us.ListWorkspaces()
	if err != nil {
		return "could not list workspaces: " + err.Error(), nil
	}
	if len(wss) == 0 {
		return "the user has no workspaces yet (create one with workspace_create).", nil
	}
	var b strings.Builder
	for _, w := range wss {
		if w.Description != "" {
			fmt.Fprintf(&b, "%s | %s — %s\n", w.ID, w.Title, w.Description)
		} else {
			fmt.Fprintf(&b, "%s | %s\n", w.ID, w.Title)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// execWorkspaceCreate — workspace_create.
func (a *Agent) execWorkspaceCreate(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca wsCreateArgs
	if err := json.Unmarshal(args, &ca); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"title\" is required.", err), nil
	}
	ca.Title = strings.TrimSpace(ca.Title)
	ca.Description = strings.TrimSpace(ca.Description)
	if ca.Title == "" {
		return "workspace_create needs a non-empty \"title\".", nil
	}
	if len(ca.Title) > wsTitleMax {
		return fmt.Sprintf("title too long (max %d).", wsTitleMax), nil
	}
	if len(ca.Description) > wsDescriptionMax {
		return fmt.Sprintf("description too long (max %d).", wsDescriptionMax), nil
	}
	if a.diskIsFrozen(handle) {
		return "the user is over their disk cap — creating workspaces is paused. Ask them to free up disk space first.", nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	id := store.NewID()
	folder := filepath.Join(a.workspaceFolderRoot(handle), id)
	// The folder first (with .SKILLS/ + .dni/), then the row — a row with a
	// missing folder would 404 every file op, so the folder is the source
	// of truth (the same order as the server's create endpoint).
	if err := os.MkdirAll(filepath.Join(folder, ".SKILLS"), 0o700); err != nil {
		return "could not create the workspace folder: " + err.Error(), nil
	}
	if err := os.MkdirAll(filepath.Join(folder, ".dni"), 0o700); err != nil {
		os.RemoveAll(folder)
		return "could not create the workspace folder: " + err.Error(), nil
	}
	if _, err := us.CreateWorkspaceWithID(id, ca.Title, ca.Description, folder); err != nil {
		os.RemoveAll(folder)
		return "could not save the workspace: " + err.Error(), nil
	}
	return fmt.Sprintf("created workspace %s (\"%s\") — in the user's environment its folder is %s/workspaces/%s.", id, ca.Title, workspaceDataDirHint(), id), nil
}

// execWorkspaceEdit — workspace_edit.
func (a *Agent) execWorkspaceEdit(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ea wsEditArgs
	if err := json.Unmarshal(args, &ea); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"id\" and \"description\" are required.", err), nil
	}
	ea.ID = strings.TrimSpace(ea.ID)
	ea.Description = strings.TrimSpace(ea.Description)
	if ea.ID == "" {
		return "workspace_edit needs the workspace \"id\" (from workspace_list).", nil
	}
	if len(ea.Description) > wsDescriptionMax {
		return fmt.Sprintf("description too long (max %d).", wsDescriptionMax), nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	if err := us.UpdateWorkspaceDescription(ea.ID, ea.Description); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "no workspace with id " + ea.ID + " — run workspace_list and use one of those ids.", nil
		}
		return "could not update the workspace: " + err.Error(), nil
	}
	if ea.Description == "" {
		return fmt.Sprintf("cleared the description of workspace %s.", ea.ID), nil
	}
	return fmt.Sprintf("updated the description of workspace %s.", ea.ID), nil
}

// execWorkspaceDelete — workspace_delete.
func (a *Agent) execWorkspaceDelete(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var da wsDeleteArgs
	if err := json.Unmarshal(args, &da); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"id\" is required.", err), nil
	}
	da.ID = strings.TrimSpace(da.ID)
	if da.ID == "" {
		return "workspace_delete needs the workspace \"id\" (from workspace_list).", nil
	}
	us, errMsg := a.userStoreOrResult(handle)
	if us == nil {
		return errMsg, nil
	}
	ws, err := us.GetWorkspace(da.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "no workspace with id " + da.ID + " — run workspace_list and use one of those ids.", nil
		}
		return "could not find the workspace: " + err.Error(), nil
	}
	if err := os.RemoveAll(ws.FolderPath); err != nil {
		return "could not delete the workspace folder: " + err.Error(), nil
	}
	if err := us.DeleteWorkspace(ws.ID); err != nil {
		return "the folder is gone, but the metadata row could not be deleted: " + err.Error(), nil
	}
	// A delete frees space — re-check immediately so the frozen state (and
	// the disk page) reflect the new usage without waiting for the next
	// monitor walk (the server's delete endpoint does the same).
	a.recheckDiskUsage(handle)
	return fmt.Sprintf("deleted workspace %s (\"%s\") — its folder and every file in it are gone.", ws.ID, ws.Title), nil
}
