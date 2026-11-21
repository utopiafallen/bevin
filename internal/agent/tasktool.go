// advance_task (the minimal task system): the working model's own report of
// how its task ended — done or blocked. It is the ONLY path where the model
// picks a terminal state: without it, a model that judges the work
// unfinishable would end the turn "cleanly" and the harness would settle
// the task done (with a confused summary). The tool is scoped to this
// conversation's owning task (1:1 — it can never touch another task's
// state), is OFFERED only in a task's owned conversation (the wire filter
// keys on the conversation's owner_task_id), and its note rides ahead of
// the completion summary (the final reply remains the summary body).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type advanceTaskArgs struct {
	State string `json:"state"`
	Note  string `json:"note"`
}

const maxAdvanceNoteLen = 500

func registerAdvanceTaskTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "advance_task",
		Description: "Report how THIS background task ended: state \"done\" or \"blocked\" (blocked = it cannot be finished; say why in note). Call it once, when the outcome is known — your final reply is still the completion summary.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state": map[string]any{
					"type":        "string",
					"enum":        []string{"done", "blocked"},
					"description": "how the task ended",
				},
				"note": map[string]any{
					"type":        "string",
					"description": "optional, up to 500 chars — why (essential when blocked: what is missing or stuck)",
				},
			},
			"required": []string{"state"},
		},
		Execute: a.execAdvanceTask,
	})
}

func (a *Agent) execAdvanceTask(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ta advanceTaskArgs
	if err := json.Unmarshal(args, &ta); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"state\" (done|blocked) is required.", err), nil
	}
	convID := conversationFromContext(ctx)
	if convID == "" {
		return "advance_task has no conversation context.", nil
	}
	state := strings.TrimSpace(ta.State)
	if state != "done" && state != "blocked" {
		return `advance_task needs "state" = "done" or "blocked".`, nil
	}
	note := strings.TrimSpace(ta.Note)
	if len(note) > maxAdvanceNoteLen {
		return fmt.Sprintf("note is too long (%d chars, max %d).", len(note), maxAdvanceNoteLen), nil
	}
	us, err := a.userStore(handle)
	if err != nil {
		return fmt.Sprintf("no user store: %v", err), nil
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		return fmt.Sprintf("no such conversation: %v", err), nil
	}
	if conv.OwnerTaskID == nil || *conv.OwnerTaskID == "" {
		return "this conversation is not a task — advance_task only reports the outcome of the task a conversation is working.", nil
	}
	a.tasks.setTerminalIntent(convID, state, note)
	if state == "blocked" {
		return "Recorded: this task settles as blocked — your note and final reply are the audit trail.", nil
	}
	return "Recorded: this task settles as done — your final reply is the completion summary.", nil
}
