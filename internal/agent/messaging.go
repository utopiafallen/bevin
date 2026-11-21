package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// agent_message (concurrent-instances, layer 4): a message to one of the
// user's sibling conversations. QUEUED, not pushed: it lands in the
// recipient's history at its next user turn (v1 never spawns LLM work for
// an idle recipient), and the roster shows it as pending until then — the
// queue is never silent. The sender's history is never shared (the
// recipient sees only the message text); a per-pair pending cap refuses a
// sender that keeps replying without its messages being processed.
type agentMessageArgs struct {
	To      string `json:"to"`
	Message string `json:"message"`
}

const (
	// agentMsgPendingCap: max queued (undelivered) messages one
	// conversation may hold to another before sends are refused (the
	// design's per-pair tunable).
	agentMsgPendingCap = 3
	// maxAgentMsgLen: a message that needs more than this is a document,
	// not a coordination note — write it to a file and say where.
	maxAgentMsgLen = 2000
)

func registerAgentMessageTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "agent_message",
		Description: "Send a message to one of the user's other (sibling) conversations, by the conv id from the roster. It is delivered there at the recipient's next user turn and shows as pending in the roster until then. Use it to hand off context or coordinate — the recipient sees only your message, not your history.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to": map[string]any{
					"type":        "string",
					"description": "the recipient conversation's id (the conv <id> in its roster line)",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "the message (concise — it is the only context the recipient gets)",
				},
			},
			"required": []string{"to", "message"},
		},
		Execute: a.execAgentMessage,
	})
}

func (a *Agent) execAgentMessage(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ma agentMessageArgs
	if err := json.Unmarshal(args, &ma); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"to\" and \"message\" are required.", err), nil
	}
	from := conversationFromContext(ctx)
	if from == "" {
		return "agent_message has no conversation context.", nil
	}
	to := strings.TrimSpace(ma.To)
	text := strings.TrimSpace(ma.Message)
	if text == "" {
		return "agent_message needs a non-empty \"message\".", nil
	}
	if len(text) > maxAgentMsgLen {
		return fmt.Sprintf("message is too long (%d chars, max %d) — condense it (or write it to a file and say where).", len(text), maxAgentMsgLen), nil
	}
	if to == from {
		return "you can't message your own conversation (it's you).", nil
	}
	us, err := a.userStore(handle)
	if err != nil {
		return fmt.Sprintf("no user store: %v", err), nil
	}
	recv, err := us.GetConversation(to)
	if err != nil {
		return fmt.Sprintf("no conversation with id %q — the roster lines carry each sibling's conv id.", to), nil
	}
	n, err := us.CountPendingAgentMessages(from, to)
	if err != nil {
		return fmt.Sprintf("could not check the queue: %v", err), nil
	}
	if n >= agentMsgPendingCap {
		return fmt.Sprintf("already %d queued messages to %q — process the messages you have when they are delivered, don't keep replying.", n, recv.Title), nil
	}
	if _, err := us.SendAgentMessage(from, to, text); err != nil {
		return fmt.Sprintf("could not queue the message: %v", err), nil
	}
	return fmt.Sprintf("queued to %q (conv %s) — it arrives at their next user turn; the roster shows it as pending until then.", recv.Title, recv.ID), nil
}
