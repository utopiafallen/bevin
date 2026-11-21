// The set_profile tool: the model's surface over the user's profile KVs
// (P1) — the distilled, durable "bevin's understanding of the user" shown
// on the profile page.
//
// Single-writer invariant: the profile is maintained by the CURATION task
// (its final step — the profile reflects the curated store, not mid-turn
// impressions), exactly as the purpose nudge is the sole writer of the
// purpose. The wire filter offers set_profile ONLY in the curation task's
// conversation (see wireTools); every other conversation gets the tool
// withheld, so no mid-conversation path can bypass curation.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	profileMaxKey   = 40  // a profile key is a label (preferences, work, ...)
	profileMaxValue = 200 // a profile entry is a distilled note, not a fact dump
)

func registerSetProfileTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "set_profile",
		Description: "Update the user's profile (the distilled, durable key: value notes about the user shown on their profile page). Upserts one entry per call.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{
					"type":        "string",
					"description": "the label for the entry (e.g. preferences, work, family)",
				},
				"value": map[string]any{
					"type":        "string",
					"description": "the distilled note itself (one short, durable sentence)",
				},
			},
			"required": []string{"key", "value"},
		},
		Execute: a.execSetProfile,
	})
}

func (a *Agent) execSetProfile(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	// Single-writer check (defense in depth beyond the wire filter, which
	// withholds the tool everywhere else): only the curation task's own
	// conversation may write the profile.
	convID := conversationFromContext(ctx)
	us, err := a.userStore(handle)
	if err != nil {
		return "internal error: could not open the user store.", nil
	}
	if convID == "" {
		return "set_profile is only available in the memory-curation task.", nil
	}
	c, err := us.GetConversation(convID)
	if err != nil || c.OwnerTaskID == nil {
		return "set_profile is only available in the memory-curation task.", nil
	}
	t, err := us.GetTask(*c.OwnerTaskID)
	if err != nil || t.Title != CurationTaskTitle {
		return "set_profile is only available in the memory-curation task.", nil
	}
	var p struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"key\" and \"value\" are required.", err), nil
	}
	key := strings.TrimSpace(p.Key)
	value := strings.TrimSpace(p.Value)
	if key == "" || value == "" {
		return "set_profile needs both a \"key\" (a short label) and a \"value\" (the distilled note).", nil
	}
	if len(key) > profileMaxKey {
		return fmt.Sprintf("the key is too long (max %d chars) — a short label.", profileMaxKey), nil
	}
	if len(value) > profileMaxValue {
		return fmt.Sprintf("the value is too long (max %d chars) — one short, durable sentence.", profileMaxValue), nil
	}
	if err := us.SetProfile(key, value, "curation"); err != nil {
		return "could not update the profile (internal error) — try again once.", nil
	}
	return fmt.Sprintf("profile updated: %s.", key), nil
}
