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
	"github.com/utopiafallen/bevin/internal/prompt"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// Agent runs conversation turns for users, using the enabled models from the
// config. Per-user SQLite stores are opened lazily and cached.
type Agent struct {
	cfg          *config.ServerConfig
	dataDir      string
	providers    map[string]provider.Provider // by model ID (admin-enabled; auto-disabled ones stay so in-flight conversations can still attempt + re-enable them)
	defaultModel string
	// sched schedules turns through the per-model execution-slot pools (see
	// scheduler.go): each model's turns run up to its slot count, FIFO.
	sched *Scheduler

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
	a.sched = NewScheduler(a.modelSlots)
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

// DefaultModelID returns the default (first available) model ID.
func (a *Agent) DefaultModelID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.defaultModel
}

// modelAPIName maps an internal model ID to the provider-side model name the
// wire request must carry. The internal ID is a derived slug (see
// config.ModelIDSlug) — safe to store and reference, but NOT the name the
// upstream API knows: a model whose upstream name contains / . : _ or
// uppercase (e.g. "unsloth/Qwen3.8-27B-GGUF:UD-IQ3_S") slugifies to something
// the provider rejects with "model not found". Every provider call goes
// through this mapping; an unknown ID maps to itself so the provider error
// still names the unknown model.
func (a *Agent) modelAPIName(internalID string) string {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, m := range a.cfg.Models {
		if m.ID == internalID {
			return m.ModelID
		}
	}
	return internalID
}

// providerFor returns the provider for a model ID (admin-enabled; auto-disabled
// models keep their provider so in-flight conversations can still attempt +
// re-enable them).
func (a *Agent) providerFor(modelID string) (provider.Provider, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.providers[modelID]
	return p, ok
}

// Reload swaps in a freshly saved config (the admin edit wizard's finalize):
// the model set, slot counts, and provider endpoints update in place — the
// running agent does not need a restart. The per-user stores are untouched,
// and every model pool is poked so queued jobs see the new slot counts
// immediately. It fails (leaving the agent on the old config) when the new
// config has no enabled model.
func (a *Agent) Reload(cfg *config.ServerConfig) error {
	providers := map[string]provider.Provider{}
	defaultModel := ""
	for _, m := range cfg.Models {
		if !m.Enabled {
			continue
		}
		p, err := provider.New(m.Provider, m.BaseURL, m.APIKey)
		if err != nil {
			return fmt.Errorf("model %s: %w", m.ID, err)
		}
		providers[m.ID] = p
		if m.Available() && defaultModel == "" {
			defaultModel = m.ID
		}
	}
	if len(providers) == 0 {
		return fmt.Errorf("no enabled models configured")
	}
	a.cfgMu.Lock()
	a.cfg = cfg
	a.cfgMu.Unlock()
	a.mu.Lock()
	a.providers = providers
	a.defaultModel = defaultModel
	a.mu.Unlock()
	for _, m := range cfg.Models {
		a.sched.Poke(m.ID)
	}
	return nil
}

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

// newUserConversation creates a conversation with the user's preferred model:
// their per-user default from agent.toml (set in first-time onboarding) when
// it is still configured + available, else the server default. The frozen
// system prompt is set at creation.
func (a *Agent) newUserConversation(us *store.UserStore, handle, title string) (*store.Conversation, error) {
	modelID := a.DefaultModelID()
	if ac, err := config.LoadAgent(filepath.Join(a.dataDir, "users", handle)); err == nil && ac.DefaultModelID != "" {
		for _, m := range a.Models() {
			if m.ID == ac.DefaultModelID && m.Available() {
				modelID = ac.DefaultModelID
				break
			}
		}
	}
	if modelID == "" {
		return nil, fmt.Errorf("no available models to start a conversation")
	}
	if title == "" {
		title = "New conversation"
	}
	c, err := us.CreateConversation(title, modelID)
	if err != nil {
		return nil, err
	}
	// Freeze the system prompt at conversation start. It is the content of
	// the system message the request builder sends; a profile/input change
	// later appends a new system message rather than mutating this.
	if err := us.SetFrozenSystemPrompt(c.ID, prompt.Base); err != nil {
		return nil, err
	}
	return c, nil
}

// EnsureConversation returns a conversation id, creating it (with the
// user's default active model) when convID is empty.
func (a *Agent) EnsureConversation(ctx context.Context, handle, convID, title string) (string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", err
	}
	if convID == "" {
		c, err := a.newUserConversation(us, handle, title)
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

const onboardingTitle = "Meet Bevin"

// OnboardingConversation creates the first-time "Meet Bevin" conversation for
// a user: their chosen default model, the standard frozen system prompt, and
// (when it has no messages yet) ONE seeded assistant welcome message. The
// onboarding content lives in the message, never the prompt, so the
// conversation stays a normal conversation forever. Idempotent: an existing
// conversation is reused and re-seeded only when empty.
func (a *Agent) OnboardingConversation(ctx context.Context, handle, modelID, welcome string) (string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", err
	}
	if conv, err := us.FindConversationByTitle(onboardingTitle); err == nil {
		if has, err := us.HasMessages(conv.ID); err == nil && !has {
			if _, err := us.AppendMessage(conv.ID, "assistant", welcome, nil, nil); err != nil {
				return "", err
			}
		}
		return conv.ID, nil
	}
	// modelID was validated by the caller against the enabled set; fall back
	// to the server default if it is somehow not available.
	if modelID == "" || !a.ModelAvailable(modelID) {
		modelID = a.DefaultModelID()
	}
	if modelID == "" {
		return "", fmt.Errorf("no available models to start a conversation")
	}
	c, err := us.CreateConversation(onboardingTitle, modelID)
	if err != nil {
		return "", err
	}
	if err := us.SetFrozenSystemPrompt(c.ID, prompt.Base); err != nil {
		return "", err
	}
	if welcome != "" {
		if _, err := us.AppendMessage(c.ID, "assistant", welcome, nil, nil); err != nil {
			return "", err
		}
	}
	return c.ID, nil
}

// ModelAvailable reports whether modelID is configured, enabled and not
// auto-disabled (i.e. usable for new work right now).
func (a *Agent) ModelAvailable(modelID string) bool {
	for _, m := range a.Models() {
		if m.ID == modelID {
			return m.Available()
		}
	}
	return false
}

// ConversationMessage is one displayable message from a conversation. The
// store also keeps bevin-internal rows (summary context artifacts, and tool
// rows once tools exist); those are never rendered as chat, so
// ConversationMessages filters to user + assistant only.
type ConversationMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// ConversationMessages returns the displayable messages of the user's
// conversation in seq order. store.ErrNotFound when the conversation is not
// in the user's store.
func (a *Agent) ConversationMessages(handle, convID string) ([]ConversationMessage, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	if _, err := us.GetConversation(convID); err != nil {
		return nil, err
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		return nil, err
	}
	out := make([]ConversationMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != roleUser && m.Role != roleAssistant {
			continue
		}
		out = append(out, ConversationMessage{Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt})
	}
	return out, nil
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
	p, ok := a.providerFor(conv.ActiveModelID)
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
	// Call the model (non-streaming or streaming). The wire request carries
	// the provider-side model name, not the internal ID (they differ whenever
	// the upstream name slugs to something else).
	apiName := a.modelAPIName(conv.ActiveModelID)
	var res *provider.Result
	if emit == nil {
		res, err = p.Chat(ctx, apiName, req)
	} else {
		res, err = p.Stream(ctx, apiName, req, emit)
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
	// Deterministic backstop for the no-echo rule: if the model echoed a
	// transcript timestamp marker into the start of its reply, strip it
	// before the reply is persisted (and before it is sent to the client on
	// the streaming "done" event / returned from Turn).
	res.Text = stripLeadingTranscriptTimestamp(res.Text)
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

// modelSlots returns a model's current execution-slot count (min 1) from live
// config — the scheduler consults it on every dispatch, so the admin's slot
// edits apply without a restart. A model removed from the config while a job
// is queued gets 1, so its queued jobs complete quickly (with an error)
// instead of waiting on a pool that no longer exists.
func (a *Agent) modelSlots(modelID string) int {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, m := range a.cfg.Models {
		if m.ID == modelID {
			if m.Slots < 1 {
				return 1
			}
			return m.Slots
		}
	}
	return 1
}

// PoolStatus is one model's pool as seen by the admin dashboard: the slot
// count + how many are busy + the turns waiting for a slot.
type PoolStatus struct {
	ID           string      `json:"id"`
	DisplayName  string      `json:"display_name"`
	Provider     string      `json:"provider"`
	Slots        int         `json:"slots"`
	Busy         int         `json:"busy"`
	Enabled      bool        `json:"enabled"`
	AutoDisabled bool        `json:"auto_disabled"`
	Waiting      []QueuedJob `json:"waiting"`
}

// Pools returns the live state of every configured model's execution-slot
// pool (for the admin dashboard).
func (a *Agent) Pools() []PoolStatus {
	load := a.sched.Load()
	out := []PoolStatus{}
	for _, m := range a.AllModels() {
		slots := m.Slots
		if slots < 1 {
			slots = 1
		}
		wait := []QueuedJob{}
		if l, ok := load[m.ID]; ok {
			wait = l.Waiting
			if wait == nil {
				wait = []QueuedJob{}
			}
			out = append(out, PoolStatus{m.ID, m.DisplayName, m.Provider, slots, l.Busy, m.Enabled, m.AutoDisabled, wait})
			continue
		}
		out = append(out, PoolStatus{m.ID, m.DisplayName, m.Provider, slots, 0, m.Enabled, m.AutoDisabled, wait})
	}
	return out
}

// Turn runs one chat turn (non-streaming) through the per-model slot pool:
// it queues on the conversation's active model and holds one slot for the
// duration of the turn. See runTurn for the full flow.
func (a *Agent) Turn(ctx context.Context, handle, convID, userText string) (string, string, error) {
	return a.runScheduled(ctx, handle, convID, userText, nil)
}

// TurnStream runs one chat turn (streaming) through the per-model slot pool,
// emitting each delta via emit once the turn holds a slot and the model call
// starts. See runTurn for the full flow.
func (a *Agent) TurnStream(ctx context.Context, handle, convID, userText string, emit func(provider.Chunk) error) (string, string, error) {
	return a.runScheduled(ctx, handle, convID, userText, emit)
}

// runScheduled resolves the conversation (so the scheduler knows which model
// pool to queue on) and runs the turn under that pool's slot limit. A
// conversation that no longer exists fails before anything is queued; a
// canceled context drops the job out of the queue (the user message is not
// persisted — the turn never ran).
func (a *Agent) runScheduled(ctx context.Context, handle, convID, userText string, emit func(provider.Chunk) error) (string, string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", "", err
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		return "", "", err
	}
	var (
		reply  string
		retErr error
	)
	jerr := a.sched.Submit(ctx, JobInfo{Handle: handle, ConvID: convID, ModelID: conv.ActiveModelID}, func(jobCtx context.Context) error {
		_, reply, retErr = a.runTurn(jobCtx, handle, convID, userText, emit)
		return retErr
	})
	if jerr != nil {
		// The context was canceled (queued or running): runTurn may have
		// already returned the cause; prefer the explicit job error.
		if retErr == nil {
			retErr = jerr
		}
		return conv.ID, reply, retErr
	}
	return conv.ID, reply, retErr
}
