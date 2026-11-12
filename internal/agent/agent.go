// Package agent is Bevin's harness: it runs a conversation turn for a user —
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
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

const baseSystemPrompt = `You are Bevin, a personal assistant. Be direct and concise.`

// Agent runs conversation turns for users, using the enabled models from the
// config. Per-user SQLite stores are opened lazily and cached.
type Agent struct {
	cfg          *config.ServerConfig
	dataDir      string
	providers    map[string]provider.Provider // by model ID (admin-enabled; auto-disabled ones stay so in-flight conversations can still attempt + re-enable them)
	defaultModel string

	mu    sync.Mutex
	users map[string]*store.UserStore

	cfgMu sync.Mutex // guards reads/writes of cfg.Models (the auto-disable state)
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
		if m.Available() && a.defaultModel == "" {
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

// Models returns the available models (admin-enabled and not auto-disabled) for
// the user UI.
func (a *Agent) Models() []config.Model {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	out := make([]config.Model, 0, len(a.cfg.Models))
	for _, m := range a.cfg.Models {
		if m.Available() {
			out = append(out, m)
		}
	}
	return out
}

// AllModels returns a copy of every configured model (with their Enabled +
// AutoDisabled states), for the admin UI.
func (a *Agent) AllModels() []config.Model {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	out := make([]config.Model, len(a.cfg.Models))
	copy(out, a.cfg.Models)
	return out
}

// isAutoDisabled reports whether the model is currently auto-disabled.
func (a *Agent) isAutoDisabled(modelID string) bool {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, m := range a.cfg.Models {
		if m.ID == modelID {
			return m.AutoDisabled
		}
	}
	return false
}

// SetModelAutoDisabled sets a model's auto-disable state + persists the config.
// It is independent of the admin's Enabled flag, and only rewrites disk when the
// state actually changes.
func (a *Agent) SetModelAutoDisabled(modelID string, disabled bool) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	changed := false
	for i := range a.cfg.Models {
		if a.cfg.Models[i].ID == modelID {
			if a.cfg.Models[i].AutoDisabled != disabled {
				a.cfg.Models[i].AutoDisabled = disabled
				changed = true
			}
			break
		}
	}
	if !changed {
		return
	}
	if err := config.Save(a.dataDir, a.cfg); err != nil {
		log.Printf("auto-disable: failed to persist config for model %s: %v", modelID, err)
	}
}

// isModelUnavailable reports whether a provider error means the requested model
// is not available (deleted/renamed upstream), as opposed to a transient
// failure (rate limit, timeout, network). It inspects the provider's error
// format: "<provider>: <status>: <body>".
func isModelUnavailable(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "does not exist") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "unknown model") ||
		strings.Contains(lower, "no such model") ||
		(strings.Contains(lower, "404") && strings.Contains(lower, "model"))
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
		if a.defaultModel == "" {
			return "", fmt.Errorf("no available models to start a conversation")
		}
		if title == "" {
			title = "New conversation"
		}
		c, err := us.CreateConversation(title, a.defaultModel)
		if err != nil {
			return "", err
		}
		// Freeze the system prompt at conversation start. It is the content of
		// the system message the request builder sends; a profile/input change
		// later appends a new system message rather than mutating this.
		if err := us.SetFrozenSystemPrompt(c.ID, baseSystemPrompt); err != nil {
			return "", err
		}
		return c.ID, nil
	}
	if _, err := us.GetConversation(convID); err != nil {
		return "", err
	}
	return convID, nil
}

// runTurn is the shared logic for Turn (non-streaming, emit=nil) and
// TurnStream (streaming, emit!=nil). It persists the user message, builds the
// request (frozen system prompt + latest context summary + raw turns since
// that summary), pre-compacts if the prompt is over budget, calls the active
// model (p.Chat when emit is nil, p.Stream otherwise — emitting deltas via
// emit), persists the assistant reply, and post-compacts if the prompt (now
// including the reply) exceeds the budget. Compaction folds the raw turns
// since the latest summary into a new chained summary (stored as
// role=summary); full history is retained. The post-compact prefers the
// API-reported usage (res.Usage — from the final SSE chunk when streaming)
// and falls back to the char/4 estimate. A compaction failure is not fatal
// (the reply is already persisted) — it is logged + the next turn retries.
// Returns the reply text + the conversation id.
func (a *Agent) runTurn(ctx context.Context, handle, convID, userText string, emit func(provider.Chunk) error) (string, string, error) {
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
	if _, err := us.AppendMessage(convID, roleUser, userText, nil, nil); err != nil {
		return conv.ID, "", err
	}
	hist, err := us.ListMessages(convID)
	if err != nil {
		return conv.ID, "", err
	}
	req, tokens := buildRequest(conv, hist)
	if tokens > summarizeBudget(a.modelMaxContext(conv.ActiveModelID)) {
		// Pre-compact: the prompt is over budget before the call. Fold the
		// older turns (incl. this message) into a summary so the call fits.
		// No API usage yet for this call — fall back to the char/4 estimate.
		if cerr := a.compactIfNeeded(ctx, us, conv, p, 0); cerr != nil {
			return conv.ID, "", cerr
		}
		hist, err = us.ListMessages(convID)
		if err != nil {
			return conv.ID, "", err
		}
		req, _ = buildRequest(conv, hist)
	}
	// Call the model (non-streaming or streaming).
	var res *provider.Result
	if emit == nil {
		res, err = p.Chat(ctx, conv.ActiveModelID, req)
	} else {
		res, err = p.Stream(ctx, conv.ActiveModelID, req, emit)
	}
	if err != nil {
		if isModelUnavailable(err) {
			// The provider reports the model is unavailable — auto-disable it
			// (independent of the admin's Enabled flag) so new conversations
			// stop routing to it. A later successful attempt re-enables it.
			a.SetModelAutoDisabled(conv.ActiveModelID, true)
		}
		return conv.ID, "", err
	}
	// A successful call proves the model is available again — re-enable it if
	// it had been auto-disabled.
	if a.isAutoDisabled(conv.ActiveModelID) {
		a.SetModelAutoDisabled(conv.ActiveModelID, false)
	}
	modelID := conv.ActiveModelID
	if _, err := us.AppendMessage(convID, roleAssistant, res.Text, &modelID, nil); err != nil {
		return conv.ID, "", err
	}
	// Post-compact: if the prompt (now including the reply) exceeds the
	// budget, compact the older turns so the next call fits. Prefer the
	// API-reported usage (res.Usage — from the final SSE chunk when
	// streaming) — the next call's prompt is ~this call's total (prompt + the
	// reply just produced); fall back to the char/4 estimate when the backend
	// omits usage.
	actualTokens := 0
	if res.Usage != nil {
		actualTokens = res.Usage.TotalTokens
	}
	if cerr := a.compactIfNeeded(ctx, us, conv, p, actualTokens); cerr != nil {
		log.Printf("compact after turn: %v", cerr)
	}
	return conv.ID, res.Text, nil
}

// Turn runs one chat turn (non-streaming). See runTurn for the full flow.
func (a *Agent) Turn(ctx context.Context, handle, convID, userText string) (string, string, error) {
	return a.runTurn(ctx, handle, convID, userText, nil)
}

// TurnStream runs one chat turn (streaming), emitting each delta via emit.
// See runTurn for the full flow. The aggregated reply + the conversation id
// are returned when the stream completes.
func (a *Agent) TurnStream(ctx context.Context, handle, convID, userText string, emit func(provider.Chunk) error) (string, string, error) {
	return a.runTurn(ctx, handle, convID, userText, emit)
}
