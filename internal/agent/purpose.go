package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// set_purpose (concurrent-instances, layer 2): the model's self-report of
// what this conversation is doing — the one-line rolling purpose the roster
// shows to the user's sibling conversations (a collision with a parallel
// conversation is a one-glance call). The cadence nudge is the only thing
// that asks for it; this call is the sole updater (staleness is visible via
// the nudge's "N turns old" wording and the roster's title fallback).
type purposeArgs struct {
	Purpose string `json:"purpose"`
	Topic   string `json:"topic"`
}

const maxPurposeLen = 200

func registerPurposeTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "set_purpose",
		Description: "State what this conversation is doing (one line) — the user's other conversations see it in their roster. Call it when a purpose check asks, or when the conversation's intent shifts.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"purpose": map[string]any{
					"type":        "string",
					"description": "one line: what this conversation is doing right now",
				},
				"topic": map[string]any{
					"type":        "string",
					"description": "optional short label for the subject",
				},
			},
			"required": []string{"purpose"},
		},
		Execute: a.execSetPurpose,
	})
}

func (a *Agent) execSetPurpose(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var pa purposeArgs
	if err := json.Unmarshal(args, &pa); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"purpose\" is required.", err), nil
	}
	convID := conversationFromContext(ctx)
	if convID == "" {
		return "set_purpose has no conversation context.", nil
	}
	purpose := strings.TrimSpace(pa.Purpose)
	if purpose == "" {
		return "set_purpose needs a one-line \"purpose\".", nil
	}
	if len(purpose) > maxPurposeLen {
		return fmt.Sprintf("purpose is too long (%d chars, max %d) — one line only.", len(purpose), maxPurposeLen), nil
	}
	topic := strings.TrimSpace(pa.Topic)
	if len(topic) > 100 {
		return "topic is too long (max 100 chars).", nil
	}
	us, err := a.userStore(handle)
	if err != nil {
		return fmt.Sprintf("no user store: %v", err), nil
	}
	if err := us.SetConversationSummary(convID, purpose, topic); err != nil {
		return fmt.Sprintf("could not update the purpose: %v", err), nil
	}
	a.purposeSet(convID)
	// The purpose IS the user's list label now (it renders in place of
	// the title): nudge every open tab so the label updates live.
	if a.NotifyConversation != nil {
		a.NotifyConversation(handle, convID)
	}
	return "purpose updated — your siblings see it in the roster from their next turn.", nil
}
