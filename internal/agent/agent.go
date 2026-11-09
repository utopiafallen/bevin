// Package agent is Alcyona's harness: it runs a conversation turn for a user —
// persist the user message, call the conversation's active model, persist the
// reply. The provider abstraction isolates each API's quirks; the harness never
// special-cases a vendor.
//
// This is the functional v1 turn. The full agentic loop (memory/retrieval via
// the Retriever seam, tool loop, per-model slot scheduler, dependency gating,
// aux-model multimodal) layers on top in later revisions.
package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/utopiafallen/alcyona-agent/internal/config"
	"github.com/utopiafallen/alcyona-agent/internal/provider"
	"github.com/utopiafallen/alcyona-agent/internal/store"
)

const baseSystemPrompt = `You are Alcyona, a personal assistant. Be direct and concise.`

// Agent runs conversation turns for users, using the enabled models from the
// config. Per-user SQLite stores are opened lazily and cached.
type Agent struct {
	cfg          *config.ServerConfig
	dataDir      string
	providers    map[string]provider.Provider // by model ID (enabled only)
	defaultModel string

	mu    sync.Mutex
	users map[string]*store.UserStore
}

// New builds an Agent from a loaded config. It fails if no model is enabled.
func New(cfg *config.ServerConfig, dataDir string) (*Agent, error) {
	a := &Agent{
		cfg:       cfg,
		dataDir:   dataDir,
		providers: map[string]provider.Provider{},
		users:     map[string]*store.UserStore{},
	}
	for _, m := range cfg.Models {
		if !m.Enabled {
			continue
		}
		p, err := provider.New(m.Provider, m.BaseURL, m.APIKey)
		if err != nil {
			return nil, fmt.Errorf("model %s: %w", m.ID, err)
		}
		a.providers[m.ID] = p
		if a.defaultModel == "" {
			a.defaultModel = m.ID
		}
	}
	if len(a.providers) == 0 {
		return nil, fmt.Errorf("no enabled models configured")
	}
	return a, nil
}

// DefaultModelID returns the default (first enabled) model ID.
func (a *Agent) DefaultModelID() string { return a.defaultModel }

// Models returns the enabled models (for the UI).
func (a *Agent) Models() []config.Model {
	var out []config.Model
	for _, m := range a.cfg.Models {
		if m.Enabled {
			out = append(out, m)
		}
	}
	return out
}

// Close closes all open per-user stores.
func (a *Agent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, us := range a.users {
		_ = us.Close()
	}
	a.users = map[string]*store.UserStore{}
}

func (a *Agent) userStore(handle string) (*store.UserStore, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if us, ok := a.users[handle]; ok {
		return us, nil
	}
	us, err := store.OpenUser(filepath.Join(a.dataDir, "users", handle))
	if err != nil {
		return nil, err
	}
	a.users[handle] = us
	return us, nil
}

// EnsureConversation returns a conversation id, creating it (with a default
// active model) when convID is empty.
func (a *Agent) EnsureConversation(ctx context.Context, handle, convID, title string) (string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", err
	}
	if convID == "" {
		if title == "" {
			title = "New conversation"
		}
		c, err := us.CreateConversation(title, a.defaultModel)
		if err != nil {
			return "", err
		}
		return c.ID, nil
	}
	if _, err := us.GetConversation(convID); err != nil {
		return "", err
	}
	return convID, nil
}

// Turn runs one chat turn: persist the user message, call the active model with
// the full conversation history, persist the assistant reply. Returns the reply
// text and the conversation id.
func (a *Agent) Turn(ctx context.Context, handle, convID, userText string) (string, string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", "", err
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		return "", "", err
	}
	p, ok := a.providers[conv.ActiveModelID]
	if !ok {
		return conv.ID, "", fmt.Errorf("no enabled provider for active model %q", conv.ActiveModelID)
	}

	// Persist the user message first, so history is always durable/consistent.
	if _, err := us.AppendMessage(convID, string(provider.RoleUser), userText, nil, nil); err != nil {
		return conv.ID, "", err
	}
	hist, err := us.ListMessages(convID)
	if err != nil {
		return conv.ID, "", err
	}
	msgs := make([]provider.Message, 0, len(hist))
	for _, h := range hist {
		role := provider.Role(h.Role)
		if role == "" {
			role = provider.RoleUser
		}
		msgs = append(msgs, provider.Message{Role: role, Text: h.Content})
	}

	res, err := p.Chat(ctx, conv.ActiveModelID, provider.ChatRequest{
		System:   []provider.ContentPart{{Text: baseSystemPrompt}},
		Messages: msgs,
	})
	if err != nil {
		return conv.ID, "", err
	}
	modelID := conv.ActiveModelID
	if _, err := us.AppendMessage(convID, string(provider.RoleAssistant), res.Text, &modelID, nil); err != nil {
		return conv.ID, "", err
	}
	return conv.ID, res.Text, nil
}
