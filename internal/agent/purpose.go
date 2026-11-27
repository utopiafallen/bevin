package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// set_purpose (concurrent-instances, layer 2): the model's self-report of
// this conversation's GOAL — a short, goal-first phrase (a few words, not
// a sentence) that doubles as the user's list label (rendered in place of
// the title) and the roster line shown to sibling conversations (a
// collision with a parallel conversation is a one-glance call). The
// cadence nudge is the only thing that asks for it; the call is the sole
// updater (staleness is visible via the nudge's "N turns old" wording).
type purposeArgs struct {
	Purpose string `json:"purpose"`
	Topic   string `json:"topic"`
}

const maxPurposeLen = 200

func registerPurposeTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "set_purpose",
		Description: "Set this conversation's purpose: a short phrase, goal-first (a few words, not a sentence) — it becomes the conversation's label in the user's chat list and its siblings' rosters. Call it when a purpose check asks, or when the goal shifts.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"purpose": map[string]any{
					"type":        "string",
					"description": "short phrase, goal-first — e.g. \"research reflux remedies\"",
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
		return "set_purpose needs a short \"purpose\" phrase.", nil
	}
	if len(purpose) > maxPurposeLen {
		return fmt.Sprintf("purpose is too long (%d chars, max %d) — a short phrase, goal-first.", len(purpose), maxPurposeLen), nil
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
	return "purpose updated — it is now the conversation's label.", nil
}
