// Package agent is Alcyona's harness: it turns a user message into a
// model call with per-user context, and records the exchange locally.
package agent

import (
	"context"

	"github.com/utopiafallen/alcyona-agent/internal/modelclient"
	"github.com/utopiafallen/alcyona-agent/internal/store"
)

// Agent orchestrates one conversation turn.
type Agent struct {
	model *modelclient.Client
	store *store.Store
}

// New builds an Agent.
func New(model *modelclient.Client, s *store.Store) *Agent {
	return &Agent{model: model, store: s}
}

const baseSystemPrompt = `You are Alcyona, a personal assistant. Be direct and concise.
You are building up long-term knowledge of this user across conversations;
use what you know about them, and note durable facts you learn.`

// TurnInput is one user turn from the web UI.
type TurnInput struct {
	UserHandle string
	Message    string
}

// TurnOutput is the assistant's reply to one user turn.
type TurnOutput struct {
	Reply string
}

// Run executes one conversation turn for a user.
func (a *Agent) Run(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	if _, err := a.store.GetOrCreateUser(in.UserHandle); err != nil {
		return nil, err
	}

	// TODO(conversations): load this user's recent conversation history
	// from the store instead of starting each turn from a blank slate.
	// TODO(memories): load this user's memories and fold them into the
	// system prompt; after the turn, extract durable facts into memories.
	msgs := []modelclient.Message{
		{Role: "system", Content: baseSystemPrompt},
		{Role: "user", Content: in.Message},
	}

	reply, err := a.model.Chat(ctx, msgs)
	if err != nil {
		return nil, err
	}

	// TODO(persist): write both messages to the conversations/messages tables.

	// TODO(tools): if the reply requests a tool call, execute it against
	// the tool registry and continue the loop with the tool result.

	return &TurnOutput{Reply: reply}, nil
}
