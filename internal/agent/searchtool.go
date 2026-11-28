package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/store"
)

// The user-data search tool: bevin's own surface over the user's data,
// mirroring the web UI's search page (one query fans out across every
// source — conversations semantically, memories as recall's hybrid blend,
// profile lexically, workspace files by name/path). The optional scopes
// narrow the sources that have that dimension: conversation = conversation
// hits + provenance-linked memories; workspace = file hits. The profile is
// always searched whole (it is the user's global context — no conversation
// or workspace dimension). All failures are RESULTS back to the model,
// like the memory tools.
//
// Named user_data_search (not bare "search") so the model never conflates
// it with web_search or reads it as an umbrella over web + user data —
// user directive, 2026-11-27.

const (
	// searchToolMaxSnippet caps a conversation hit's content in the result
	// (the model can open the conversation for the full text — the snippet
	// is for recognition + context, not a copy of history).
	searchToolMaxSnippet = 240
	searchToolDefaultTop = 10
	searchToolMaxTop     = 25
)

func registerSearchTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "user_data_search",
		Description: `Search the user's own data. Memory results carry the memory id (#N) — the id forget and remember's supersede_id need. Optional scopes: "conversation" (a conversation id) restricts the conversation and memory results to that conversation; "workspace" (a workspace id) restricts the file results to that workspace.`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"q": map[string]any{
					"type":        "string",
					"description": "what to search for, in natural language",
				},
				"conversation": map[string]any{
					"type":        "string",
					"description": "optional: a conversation id — restrict the conversation and memory results to that conversation",
				},
				"workspace": map[string]any{
					"type":        "string",
					"description": "optional: a workspace id — restrict the file results to that workspace",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "how many results per source (default 10, max 25)",
				},
			},
			"required": []string{"q"},
		},
		Execute: a.execSearch,
	})
}

func (a *Agent) execSearch(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var p struct {
		Q            string `json:"q"`
		Conversation string `json:"conversation"`
		Workspace    string `json:"workspace"`
		Limit        int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"q\" is required.", err), nil
	}
	q := strings.TrimSpace(p.Q)
	if q == "" {
		return "user_data_search needs a query (\"q\").", nil
	}
	// The conversation's loaded workspace is the DEFAULT file scope (the
	// design log's WORKSPACE LOADING section) — an explicit "workspace"
	// arg still overrides; no loaded workspace = today's behavior (no
	// file leg).
	if p.Workspace == "" {
		p.Workspace = a.loadedWorkspace(handle, conversationFromContext(ctx))
	}
	limit := p.Limit
	if limit <= 0 {
		limit = searchToolDefaultTop
	}
	if limit > searchToolMaxTop {
		limit = searchToolMaxTop
	}
	out, err := a.SearchAll(ctx, handle, q, p.Conversation, p.Workspace, limit)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "the conversation or workspace id you gave does not exist — search again without that scope, or with an id from the user's data.", nil
		}
		return fmt.Sprintf("search failed: %v", err), nil
	}
	var b strings.Builder
	now := time.Now()
	convScoped := p.Conversation != ""

	convs := anySlice[SearchHit](out["conversations"])
	mems := anySlice[store.Memory](out["memories"])
	profs := anySlice[profileHit](out["profile"])
	files := anySlice[store.WorkspaceFile](out["workspace"])

	anyHit := false
	if len(convs) > 0 {
		label := "CONVERSATIONS (semantic"
		if convScoped {
			label += ", this conversation"
		}
		label += ")"
		b.WriteString(label + "\n")
		for _, hit := range convs {
			anyHit = true
			fmt.Fprintf(&b, "- [%s · %d%%] %s\n", hit.Role, int64(hit.Score*100+0.5), snippet(hit.Content, searchToolMaxSnippet))
		}
		b.WriteString("\n")
	}
	if len(mems) > 0 {
		label := "MEMORIES"
		if convScoped {
			label += " (this conversation)"
		}
		b.WriteString(label + "\n")
		for _, mem := range mems {
			anyHit = true
			// The #id is load-bearing: forget and remember's supersede_id
			// take it (recall's result format, which the search absorbed).
			fmt.Fprintf(&b, "#%d [%s] %s (confidence %.2f, %s)\n", mem.ID, mem.Facet, mem.Statement, mem.Confidence, memoryAge(now, mem.UpdatedAt))
		}
		b.WriteString("\n")
	}
	if len(profs) > 0 {
		b.WriteString("PROFILE\n")
		for _, kv := range profs {
			anyHit = true
			fmt.Fprintf(&b, "- %s: %s\n", kv.Key, kv.Value)
		}
		b.WriteString("\n")
	}
	if len(files) > 0 {
		label := "WORKSPACE FILES"
		if p.Workspace != "" {
			label += " (" + p.Workspace + ")"
		}
		b.WriteString(label + "\n")
		for _, f := range files {
			anyHit = true
			switch {
			case f.Name != "" && f.RelativePath != "":
				fmt.Fprintf(&b, "- %s · %s\n", f.Name, f.RelativePath)
			case f.RelativePath != "":
				fmt.Fprintf(&b, "- %s\n", f.RelativePath)
			default:
				fmt.Fprintf(&b, "- %s\n", f.Name)
			}
		}
		b.WriteString("\n")
	}
	if !anyHit {
		return "no matches for \"" + q + "\" in the user's data" + scopeNote(p.Conversation, p.Workspace) + ".", nil
	}
	res := strings.TrimRight(b.String(), "\n")
	if a.vectorIndex() == nil {
		res += "\n(note: semantic conversation search is not configured — only the local sources were searched)"
	}
	return res + scopeNote(p.Conversation, p.Workspace), nil
}

// anySlice normalizes a SearchAll section value — the typed slice SearchAll
// filled in, or the empty []any placeholder (an empty section) — into a
// typed slice.
func anySlice[T any](v any) []T {
	switch s := v.(type) {
	case []T:
		return s
	case []any:
		out := make([]T, 0, len(s))
		for _, e := range s {
			if t, ok := e.(T); ok {
				out = append(out, t)
			}
		}
		return out
	default:
		return nil
	}
}

// scopeNote renders the active scopes for the result's framing.
func scopeNote(conversationID, workspaceID string) string {
	switch {
	case conversationID != "" && workspaceID != "":
		return " (scoped to conversation " + conversationID + " + workspace " + workspaceID + ")"
	case conversationID != "":
		return " (scoped to conversation " + conversationID + ")"
	case workspaceID != "":
		return " (scoped to workspace " + workspaceID + ")"
	default:
		return ""
	}
}
