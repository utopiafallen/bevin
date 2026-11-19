package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// registerReportProgress registers the progress-declaration tool (the design
// log's no-progress section). It is a NO-OP / OBSERVATION tool: it changes
// nothing in the world. The model calls it to report its honest estimated
// completion % + its next step. The harness reads the % (from the call's args)
// for the confidence accumulator (the L2 / progress signal), and the UI can
// render a live progress bar from it. The tool result is a neutral
// acknowledgment — and because it is a normal tool call in the transcript, the
// model also sees its OWN prior declarations (reinforcing the signal, and
// prefix-safe: it appends to the tail like any tool call).
//
// It is ALWAYS-ON in the wire schema: a voluntary declaration is also a signal
// (and the "report progress periodically" UX works even when no layer has
// fired — the progress bar updates as the model works). It is REQUIRED when a
// check-in is issued (the harness asks for it explicitly via the check-in
// message). An invalid % (out of range, or missing next_step) is an executor
// error -> ok=false -> the accumulator does NOT count it as a declaration (the
// model self-heals from the error result).
func registerReportProgress(r *ToolRegistry) error {
	return r.Register(Tool{
		Name: reportProgressTool,
		Description: `Report your estimated progress toward the user's goal.
- percent (integer 0-100): your honest estimate of how much of the requested work is DONE. Be truthful — a low or declining percent while you keep repeating the same action is exactly when Bevin needs to hear it (it may stop the turn and tell the user what is blocking you).
- next_step (string): one line on what you are going to do next.
Call this whenever you are asked for a check-in. You may also call it proactively to keep the user informed while you work autonomously.`,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"percent":   map[string]any{"type": "integer", "minimum": 0, "maximum": 100, "description": "estimated % of the requested work that is done"},
				"next_step": map[string]any{"type": "string", "description": "one line: what you will do next"},
			},
			"required": []string{"percent", "next_step"},
		},
		Execute: func(ctx context.Context, handle string, args json.RawMessage) (string, error) {
			var a struct {
				Percent  int    `json:"percent"`
				NextStep string `json:"next_step"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: expected {percent: integer 0-100, next_step: string} — %v", err)
			}
			if a.Percent < 0 || a.Percent > 100 {
				return "", fmt.Errorf("percent must be between 0 and 100 (got %d)", a.Percent)
			}
			if a.NextStep == "" {
				return "", fmt.Errorf("next_step is required (one line on what you will do next)")
			}
			return fmt.Sprintf("Noted: %d%% done. Next: %s. Continue working toward the goal.", a.Percent, a.NextStep), nil
		},
	})
}
