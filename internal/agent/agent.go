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
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/utopiafallen/bevin/internal/browser"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/profile"
	"github.com/utopiafallen/bevin/internal/prompt"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/qdrant"
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

	// vec is the conversation vector index (embedder + per-user Qdrant
	// collection); nil when the embedder or the Qdrant endpoint is not
	// configured — turns then skip indexing and search returns no hits.
	vec *vectorIndex

	// tools is the user agent's tool registry (the plan's WS1): the turn
	// loop dispatches the model's tool calls into it (see runTurn). The
	// registry is shared across users — the executors resolve per-user
	// state from the handle. browserMgr is the per-user desktop browser
	// manager behind the web_browser tool (WS5); the browser launches
	// lazily on the user's first web_browser call, never at agent start.
	tools      *ToolRegistry
	browserMgr *browser.Manager

	// llmWork counts the per-user running LLM works (turns) — the gate
	// behind the idle learning pass's "no other LLM work running" condition
	// (the scheduler pools are per-model and shared; see learning.go).
	// learn is the idle learning loop (the memory model's idle reflection).
	llmWork *llmWorkTracker
	learn   *learningMgr

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
	a.llmWork = newLLMWorkTracker()
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
	a.vec = a.buildVectorIndex(cfg)
	// The user agent's tools (WS1 registry + WS5 web_browser). The browser
	// manager is created here but launches nothing until a user's first
	// web_browser call (lazy, per user); a host without a desktop browser
	// simply gets the tool's "unavailable" result at use time.
	a.browserMgr = browser.NewManager(dataDir)
	a.tools = NewToolRegistry()
	if err := registerWebBrowser(a.tools, a.browserMgr); err != nil {
		return nil, fmt.Errorf("register web_browser: %w", err)
	}
	// web_search is registered unconditionally, like web_browser: a turn
	// offers it only while the live config has it enabled + configured
	// (wireTools filters the wire list per turn), and a stray call (the
	// model imitating it from its own history after an admin disabled it)
	// gets the structured not-enabled result naming the web_browser
	// fallback.
	if err := registerWebSearch(a.tools, a); err != nil {
		return nil, fmt.Errorf("register web_search: %w", err)
	}
	if err := registerReportProgress(a.tools); err != nil {
		return nil, fmt.Errorf("register report_progress: %w", err)
	}
	// edit_file (WS4): exact-match file edits over the user's workspaces —
	// registered unconditionally like the other tools; a user without
	// workspaces gets the structured "create one first" result at use time.
	if err := registerEditTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register edit_file: %w", err)
	}
	// The memory tools (the memory model's on-demand surface: recall /
	// remember / forget over the user's P5 memories).
	if err := registerMemoryTools(a.tools, a); err != nil {
		return nil, fmt.Errorf("register memory tools: %w", err)
	}
	// The idle learning loop (armed per conversation at turn completion;
	// the pass itself runs on the timer's goroutine, never in a turn).
	a.learn = newLearningMgr(a)
	return a, nil
}

// webSearchState returns the live web_search settings (read fresh under
// cfgMu, the npCfg pattern): an admin save applies from the next tool call
// / turn, no restart. The zero value (the flat-v1 default for pre-existing
// configs with no [tools] section) = disabled = the tool is not offered.
func (a *Agent) webSearchState() config.WebSearch {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg.Tools.WebSearch
}

// wireTools builds ONE turn's wire tool list from the registry: web_search
// is FILTERED OUT while the live config has it disabled/unconfigured (a
// turn never offers a tool it can't use — the model then has no reason to
// call it, and the description note below tells it how to search anyway),
// and the web_browser description carries a per-turn SEARCH note matching
// the web_search state (prefer web_search when it is on the wire; when it
// is not, search-engine tabs — user directive: the agent uses web_browser
// to search). The registry entries are untouched — the wire copies carry
// the note (Wire() returns fresh copies each turn).
func (a *Agent) wireTools() []provider.Tool {
	wsOn := a.webSearchState().Configured()
	list := a.tools.Wire()
	if !wsOn {
		filtered := make([]provider.Tool, 0, len(list))
		for _, t := range list {
			if t.Name == "web_search" {
				continue
			}
			filtered = append(filtered, t)
		}
		list = filtered
	}
	for i := range list {
		if list[i].Name == "web_browser" {
			list[i].Description = webBrowserWireDescription(wsOn)
		}
	}
	return list
}

// buildVectorIndex assembles the conversation vector index from the config
// (the local llama-server embedder + the Qdrant sidecar endpoint). It is nil
// when either is absent — vector search is simply off, not an error.
func (a *Agent) buildVectorIndex(cfg *config.ServerConfig) *vectorIndex {
	if cfg.Embedder.BaseURL == "" || cfg.Embedder.ModelID == "" || cfg.Sidecars.Qdrant == "" {
		return nil
	}
	emb := provider.NewLlama(cfg.Embedder.BaseURL, cfg.Embedder.APIKey)
	return newVectorIndex(emb, cfg.Embedder.ModelID, qdrant.New(cfg.Sidecars.Qdrant))
}

// vectorIndex returns the current index (nil when not configured).
func (a *Agent) vectorIndex() *vectorIndex {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vec
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

// profileFor resolves a model's MODEL PROFILE (its harness behavior bundle —
// what it gets back of its own reasoning, how its context is framed and
// compacted, how its prompt size is estimated) from live config: the model's
// assigned profile name -> the registry (internal/profile). A profile
// reassignment applies to the model's NEXT turn without a restart (same
// liveness as slot edits). Config validation guarantees the name is known
// (a blank self-heals to general at load/save; a set unknown name fails the
// config load loudly), so the fallback is defensive only — a turn must not
// crash on a config anomaly.
func (a *Agent) profileFor(modelID string) profile.Profile {
	a.cfgMu.Lock()
	name := ""
	for _, m := range a.cfg.Models {
		if m.ID == modelID {
			name = m.Profile
			break
		}
	}
	a.cfgMu.Unlock()
	if name == "" {
		return profile.Default()
	}
	if p, err := profile.Get(name); err == nil {
		return p
	}
	return profile.Default()
}

// ReasoningEffortsFor returns the model's profile's supported reasoning-effort
// levels — the per-conversation control's option list for that model (the
// chat UI populates its dropdown from it; an EMPTY slice means the model
// takes no explicit effort, and the UI hides the control). The live-config
// resolution is the same as profileFor: a profile reassignment applies from
// the next read, no restart.
func (a *Agent) ReasoningEffortsFor(modelID string) []string {
	efforts := a.profileFor(modelID).ReasoningEfforts
	if efforts == nil {
		return []string{}
	}
	return efforts
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
	a.vec = a.buildVectorIndex(cfg)
	a.mu.Unlock()
	for _, m := range cfg.Models {
		a.sched.Poke(m.ID)
	}
	return nil
}

// SetDiskFrozen wires the disk-cap frozen check into the scheduler: a user
// over their disk cap has their queued work deferred (kept in the queue) until
// they're back under the cap. The host (the server's disk monitor) installs
// this. Nil-safe: passing nil defers no one.
func (a *Agent) SetDiskFrozen(f func(handle string) bool) {
	a.sched.SetFrozen(f)
}

// PokeAll re-dispatches every model pool — used when a user unfreezes (their
// deferred queued work can now run in any pool it's queued in).
func (a *Agent) PokeAll() {
	a.sched.PokeAll()
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

// Close closes all open per-user stores and kills every launched per-user
// browser (the server-shutdown cleanup trigger — a browser child must not
// outlive the server).
func (a *Agent) Close() {
	if a.browserMgr != nil {
		a.browserMgr.Close()
	}
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

// UserStoreFor returns the user's per-user store (the same cached
// connection the agent uses — so server handlers that touch the user's
// data see it through one pool).
func (a *Agent) UserStoreFor(handle string) (*store.UserStore, error) {
	return a.userStore(handle)
}

// NewConversationModel resolves the model a brand-new conversation for the
// user takes when the request does not pin one: their per-user default from
// agent.toml (set in first-time onboarding) while it is still configured +
// available, else the server default ("" when no model is available at all).
// The create endpoint and the chat's new-conversation preview (the client
// mirrors it from /api/me + /api/models) MUST agree — this is the single
// source of truth for the resolution.
func (a *Agent) NewConversationModel(handle string) string {
	modelID := a.DefaultModelID()
	if ac, err := config.LoadAgent(filepath.Join(a.dataDir, "users", handle)); err == nil && ac.DefaultModelID != "" {
		for _, m := range a.Models() {
			if m.ID == ac.DefaultModelID && m.Available() {
				modelID = ac.DefaultModelID
				break
			}
		}
	}
	return modelID
}

// newUserConversation creates a conversation with modelID + an optional
// reasoning effort, the frozen system prompt set at creation. modelID "" =
// the user's preferred model (NewConversationModel); a pinned modelID must
// be available. effort "" = the model's profile default; a pinned effort
// must be one of the model's profile levels.
func (a *Agent) newUserConversation(us *store.UserStore, handle, title, modelID, effort string) (*store.Conversation, error) {
	if modelID == "" {
		modelID = a.NewConversationModel(handle)
	} else if !a.ModelAvailable(modelID) {
		return nil, fmt.Errorf("model %s is not available", modelID)
	}
	if modelID == "" {
		return nil, fmt.Errorf("no available models to start a conversation")
	}
	if effort != "" {
		supported := false
		for _, e := range a.ReasoningEffortsFor(modelID) {
			if e == effort {
				supported = true
				break
			}
		}
		if !supported {
			return nil, fmt.Errorf("unsupported reasoning effort %q for this model (supported: %s)", effort, strings.Join(a.ReasoningEffortsFor(modelID), ", "))
		}
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
	if effort != "" {
		if err := us.SetConversationReasoningEffort(c.ID, effort); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// CreateConversation creates a fresh conversation (the chat's "new
// conversation" + first message — the conversation row exists only from the
// first send onward): title + optional model + optional reasoning effort
// ("" = the user's preferred / server default + the profile default).
// Returns the new conversation id.
func (a *Agent) CreateConversation(ctx context.Context, handle, title, modelID, effort string) (string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", err
	}
	c, err := a.newUserConversation(us, handle, title, modelID, effort)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// EnsureConversation returns a conversation id, creating it (with the
// user's default active model) when convID is empty.
func (a *Agent) EnsureConversation(ctx context.Context, handle, convID, title string) (string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", err
	}
	if convID == "" {
		c, err := a.newUserConversation(us, handle, title, "", "")
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
			if m, err := us.AppendMessage(conv.ID, "assistant", welcome, "", "", nil); err != nil {
				return "", err
			} else {
				a.indexMessage(ctx, handle, m, onboardingTitle)
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
		if m, err := us.AppendMessage(c.ID, "assistant", welcome, "", "", nil); err != nil {
			return "", err
		} else {
			a.indexMessage(ctx, handle, m, onboardingTitle)
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

// indexMessage embeds + upserts a persisted message into the user's vector
// collection (best-effort — a failure is logged, never fatal to the turn).
func (a *Agent) indexMessage(ctx context.Context, handle string, msg *store.Message, title string) {
	vi := a.vectorIndex()
	if vi == nil {
		return
	}
	if err := vi.IndexMessage(ctx, handle, *msg, title); err != nil {
		logVectorIndexError("index", err)
	}
}

// SearchConversations semantically searches the user's conversation history
// (their Qdrant collection). An unconfigured index yields an empty result —
// not an error — so the endpoint stays 200 either way.
func (a *Agent) SearchConversations(ctx context.Context, handle, query string, limit int) ([]SearchHit, error) {
	vi := a.vectorIndex()
	if vi == nil {
		return []SearchHit{}, nil
	}
	return vi.Search(ctx, handle, query, limit)
}

// UserProfileData returns the user's profile-page data: bevin's profile of
// them (the durable profile KVs that are injected into the system prompt —
// what bevin knows about the user), the learned facts (active memories), and
// the persona list. Personas are a STUB for now (no table, no editor yet) —
// the shape exists so the page renders its full layout from day one.
func (a *Agent) UserProfileData(handle string) (map[string]string, []store.Memory, []any, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, nil, nil, err
	}
	prof, err := us.GetProfile()
	if err != nil {
		return nil, nil, nil, err
	}
	facts, err := us.ListActiveMemories()
	if err != nil {
		return nil, nil, nil, err
	}
	if facts == nil {
		facts = []store.Memory{}
	}
	return prof, facts, []any{}, nil
}

// ftsQuery sanitizes raw user search text into a safe FTS5 query: each
// whitespace-separated token becomes a quoted phrase, space-joined (the FTS5
// default is AND between terms) — so raw input containing FTS5 operators
// (NOT, AND, *, or unbalanced quotes) cannot break the query or widen its
// scope. "" when there are no tokens.
func ftsQuery(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(out, " ")
}

// SearchAll backs the dedicated search page: it searches the user's data
// across every source at once — conversations (semantic, the per-user vector
// index), memories (FTS5), the profile (case-insensitive substring over keys
// + values), and, when a workspace is named, that workspace's files (name +
// path match — file CONTENTS are not indexed yet, metadata only). An empty
// workspaceID is the default scope: all of the user's data. The
// user's workspaces are returned in `workspaces` so the page's scope
// selector can offer them. The vector-index leg is best-effort (an
// unconfigured index or a sidecar blip leaves the local sources intact);
// a store failure is a real error.
func (a *Agent) SearchAll(ctx context.Context, handle, query, workspaceID string, limit int) (map[string]any, error) {
	if limit <= 0 {
		limit = 10
	}
	out := map[string]any{
		"conversations": []any{},
		"memories":      []any{},
		"profile":       []any{},
		"workspaces":    []any{},
		"workspace":     []any{},
	}
	if hits, err := a.SearchConversations(ctx, handle, query, limit); err == nil {
		out["conversations"] = hits
	}
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	if fq := ftsQuery(query); fq != "" {
		if mems, err := us.SearchMemories(fq, limit); err == nil && mems != nil {
			out["memories"] = mems
		}
	}
	type kvHit struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	kv, err := us.GetProfile()
	if err != nil {
		return nil, err
	}
	var profileHits []any
	for k, v := range kv {
		if strings.Contains(strings.ToLower(k), strings.ToLower(query)) || strings.Contains(strings.ToLower(v), strings.ToLower(query)) {
			profileHits = append(profileHits, kvHit{k, v})
		}
	}
	out["profile"] = profileHits
	type wsRef struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	ws, err := us.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	wsRefs := make([]wsRef, 0, len(ws))
	for _, w := range ws {
		wsRefs = append(wsRefs, wsRef{w.ID, w.Title})
	}
	out["workspaces"] = wsRefs
	if workspaceID != "" {
		if _, err := us.GetWorkspace(workspaceID); err != nil {
			return nil, err
		}
		files, err := us.SearchWorkspaceFiles(workspaceID, query, limit)
		if err != nil {
			return nil, err
		}
		out["workspace"] = files
	}
	return out, nil
}

// DeleteConversation removes the user's conversation: its message rows go
// first (the authoritative step — the FTS index follows via its triggers),
// then the indexed vectors, best-effort (an unconfigured vector index or a
// sidecar blip leaves no SQLite residue; a failed point removal is logged,
// never fatal — an orphaned point just stops being reachable, the
// conversation itself is gone). store.ErrNotFound when the conversation is
// not in the user's store.
func (a *Agent) DeleteConversation(ctx context.Context, handle, convID string) error {
	us, err := a.userStore(handle)
	if err != nil {
		return err
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	if err := us.DeleteConversation(convID); err != nil {
		return err
	}
	if vi := a.vectorIndex(); vi != nil {
		if err := vi.DeleteMessages(ctx, handle, ids); err != nil {
			logVectorIndexError("delete points", err)
		}
	}
	return nil
}

// ConversationSummary is one row of the user's conversation list: the
// identity + scheduling facts the UI needs (no frozen prompt, no messages).
type ConversationSummary struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	CreatedAt      string `json:"created_at"`
	LastActivityAt string `json:"last_activity_at"`
	ActiveModelID  string `json:"active_model_id"`
	// ReasoningEffort: the conversation's stored reasoning-effort choice
	// ("" = the model's own default — the UI's dropdown then shows the
	// default).
	ReasoningEffort string `json:"reasoning_effort"`
}

// Conversations lists the user's conversations, most recently active first
// (the store's order).
func (a *Agent) Conversations(handle string) ([]ConversationSummary, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	convos, err := us.ListConversations()
	if err != nil {
		return nil, err
	}
	out := make([]ConversationSummary, 0, len(convos))
	for _, c := range convos {
		out = append(out, ConversationSummary{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, LastActivityAt: c.LastActivityAt, ActiveModelID: c.ActiveModelID, ReasoningEffort: c.ReasoningEffort})
	}
	return out, nil
}

// Conversation returns one of the user's conversations (the slim summary —
// the UI reads it to show + switch the conversation's active model).
// store.ErrNotFound when it is not in the user's store.
func (a *Agent) Conversation(handle, convID string) (*ConversationSummary, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	c, err := us.GetConversation(convID)
	if err != nil {
		return nil, err
	}
	return &ConversationSummary{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, LastActivityAt: c.LastActivityAt, ActiveModelID: c.ActiveModelID, ReasoningEffort: c.ReasoningEffort}, nil
}

// UpdateConversation changes a conversation's active model, title, and/or
// reasoning-effort choice (any of them; at least one is required, enforced by
// the caller). The model must be available right now — also enforced by the
// caller; the effort is validated against the (possibly new) model's profile
// levels by the caller too. The changes apply to the NEXT turn: an in-flight
// turn keeps the model + effort it started on (the scheduler already queued
// it in that model's pool). Returns the updated summary. store.ErrNotFound
// when the conversation is not in the user's store.
func (a *Agent) UpdateConversation(handle, convID string, modelID, title, reasoningEffort *string) (*ConversationSummary, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	if _, err := us.GetConversation(convID); err != nil {
		return nil, err
	}
	if modelID != nil && *modelID != "" {
		if err := us.SetConversationModel(convID, *modelID); err != nil {
			return nil, err
		}
	}
	if title != nil {
		t := strings.TrimSpace(*title)
		if t == "" {
			return nil, fmt.Errorf("title must not be blank")
		}
		if err := us.SetConversationTitle(convID, t); err != nil {
			return nil, err
		}
	}
	if reasoningEffort != nil {
		if err := us.SetConversationReasoningEffort(convID, *reasoningEffort); err != nil {
			return nil, err
		}
	}
	c, err := us.GetConversation(convID)
	if err != nil {
		return nil, err
	}
	return &ConversationSummary{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, LastActivityAt: c.LastActivityAt, ActiveModelID: c.ActiveModelID, ReasoningEffort: c.ReasoningEffort}, nil
}

// ContextUsage reports what the conversation's NEXT turn would send: the
// estimated prompt size in tokens (frozen system prompt + latest summary +
// the raw turns since it, estimated exactly the way the request builder and
// the compaction budget do) and the active model's total context window
// (0 = unknown). The UI's context-window usage visualization reads it when
// a conversation opens, then refines the numerator from each turn's
// API-reported usage (the done event's usage.prompt_tokens).
func (a *Agent) ContextUsage(handle, convID string) (map[string]any, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	c, err := us.GetConversation(convID)
	if err != nil {
		return nil, err
	}
	hist, err := us.ListMessages(convID)
	if err != nil {
		return nil, err
	}
	prof := a.profileFor(c.ActiveModelID)
	_, tokens := buildRequest(c, hist, prof, nil)
	return map[string]any{"tokens": tokens, "max_tokens": a.modelMaxContext(c.ActiveModelID)}, nil
}

// ConversationMessage is one displayable message from a conversation. The
// store also keeps bevin-internal rows (summary context artifacts, and tool
// rows once tools exist); those are never rendered as chat, so
// ConversationMessages filters to user + assistant only. Reasoning is the
// assistant's stored thinking trace (empty for user messages and turns before
// reasoning persistence).
type ConversationMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning,omitempty"`
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
		// An empty assistant row WITH P7 tool calls is the model's
		// tool-call turn — the row exists to carry the calls, and the text
		// lands on the turn's final reply row: not a chat bubble. An empty
		// row WITHOUT any tool calls is a genuine empty reply (the model
		// ended the turn with no text — a reasoning-only completion, or the
		// dropped streamed tool call before 7174606): it stays in the
		// history so the user's question never looks unanswered, and the
		// UI renders it as "(empty reply)".
		if m.Role == roleAssistant && m.Content == "" {
			rows, err := us.ToolCallsForMessage(m.ID)
			if err == nil && len(rows) > 0 {
				continue
			}
		}
		out = append(out, ConversationMessage{Role: m.Role, Content: m.Content, Reasoning: m.Reasoning, CreatedAt: m.CreatedAt})
	}
	return out, nil
}

// npCfg returns the no-progress detector's configuration for a turn. The
// DETECTION params (N, W, K) come from the admin config (server.toml
// [no_progress] — read fresh each turn, so an admin edit applies on the next
// turn without a restart, like the model slots). The CONFIDENCE params
// (weights, decay, threshold) are the named internal defaults — the harness's
// risk model, a design decision rather than a per-deployment dial. Zero/invalid
// detection values are sanitized to the defaults by newNoProgress (a partial
// config must not disable the detector).
func (a *Agent) npCfg() npCfg {
	d := defaultNPCfg()
	c := a.cfg.NoProgress
	return npCfg{
		N:         c.RepeatN,
		W:         c.Window,
		K:         c.StateK,
		WL0:       d.WL0,
		WL1:       d.WL1,
		WDecl:     d.WDecl,
		WDecline:  d.WDecline,
		WMiss:     d.WMiss,
		Decay:     d.Decay,
		Threshold: d.Threshold,
	}
}

// checkinText is the neutral, PERSISTENT check-in the harness appends (as a
// roleCheckin row — in the model's request, filtered from the UI) when a
// no-progress layer fires: it REQUIRES the model to call report_progress on
// its next reply. It is phrased neutrally (a status check, not an accusation)
// so it doesn't bias the model, and it is persistent (a stored, tail-appended
// row — prefix-safe, unlike a system-prompt change) so the model sees the
// trigger when it responds and its % declaration doesn't dangle with no cause.
const checkinText = "Check-in: before your next action, call report_progress to state (1) your honest estimated completion percent for the user's goal and (2) your next step. If you are repeating an action without making progress, say so honestly — a low or declining percent is useful information, not a failure."

// stoppedNotice is the "stopped, not done" text appended to the reply when the
// turn ends on the no-progress confidence threshold: the user must know the
// trajectory was STOPPED (and the indicators), not silently cut off as if the
// answer were complete. It is appended to the reply text (persisted with the
// assistant row and streamed as part of it) so it reaches both the streaming
// and non-streaming paths.
func stoppedNotice(np *noProgress) string {
	var b strings.Builder
	b.WriteString("\n\n---\n")
	fmt.Fprintf(&b, "Bevin stopped this turn: no observed progress (confidence %d%%). ", int(np.confidence()*100+0.5))
	if s := np.signals(); len(s) > 0 {
		b.WriteString("Indicators: " + strings.Join(s, "; ") + ". ")
	}
	b.WriteString("The work is not finished — you can give more specific direction, or ask bevin to continue from here.")
	return b.String()
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
	// The conversation id rides the ctx for the memory tools'
	// per-conversation state (remember's provenance) — harness plumbing,
	// see memorytools.go.
	ctx = context.WithValue(ctx, ctxConversationKey, convID)
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
	userMsg, err := us.AppendMessage(convID, roleUser, userText, "", "", nil)
	if err != nil {
		return conv.ID, "", err
	}
	a.indexMessage(ctx, handle, userMsg, conv.Title)
	// The conversation's active model's profile drives the request builder
	// (its model-facing decisions — see buildRequest); it is resolved once
	// per turn from live config.
	prof := a.profileFor(conv.ActiveModelID)
	apiName := a.modelAPIName(conv.ActiveModelID)
	modelID := conv.ActiveModelID
	// The tool loop (WS1): while the model's answer carries tool calls, the
	// harness persists the assistant row + executes each call SEQUENTIALLY
	// (v1: parallel calls are out of scope) + records each outcome on its
	// P7 row, and calls the model again — the tool transcript is just
	// history on the next iteration (re-read from the store, so the request
	// builder's compaction/selection applies to it unchanged). The loop has NO
	// hard count cap: it stops when the no-progress confidence accumulator
	// crosses its threshold (the model is stuck) — the call after that goes
	// out with NO tools on the wire, forcing a plain-text answer from whatever
	// the tool results showed (see the np state below). With an empty registry
	// the loop body never fires and the turn is exactly today's single call.
	// wireTools (not tools.Wire) builds the per-turn list: web_search is
	// filtered out while disabled/unconfigured, and web_browser carries the
	// matching search note (see wireTools).
	wireTools := a.wireTools()
	var res *provider.Result
	// The no-progress / stuck-detection state (the design log's TOOL LOOP
	// NO-PROGRESS section): REPLACES the hard maxToolIterations cap. The turn
	// stops when the accumulated no-progress CONFIDENCE crosses a threshold
	// (the model is stuck), not at an arbitrary count (which cut "long but
	// progressing" and "stuck" alike). Fresh each turn.
	np := newNoProgress(a.npCfg())
	stopped := false         // the turn ended on the no-progress confidence threshold
	persistedInLoop := false // the reply row was persisted inside the loop (runaway)
	for iter := 0; ; iter++ {
		tools := wireTools
		if np.stopped() {
			tools = nil // the forced text-only final call (the hard stop)
			stopped = true
		}
		hist, err := us.ListMessages(convID)
		if err != nil {
			return conv.ID, "", err
		}
		tcMap, err := toolCallsForMessages(us, hist)
		if err != nil {
			return conv.ID, "", err
		}
		req, tokens := buildRequest(conv, hist, prof, tcMap)
		req.Tools = tools
		if tokens > summarizeBudget(a.modelMaxContext(conv.ActiveModelID), prof) {
			// Pre-compact: the prompt is over budget before the call. Fold the
			// older turns (incl. this message) into a summary so the call fits.
			// No API usage yet for this call — fall back to the char/4
			// estimate.
			if cerr := a.compactIfNeeded(ctx, us, conv, p, 0, prof); cerr != nil {
				return conv.ID, "", cerr
			}
			hist, err = us.ListMessages(convID)
			if err != nil {
				return conv.ID, "", err
			}
			tcMap, err = toolCallsForMessages(us, hist)
			if err != nil {
				return conv.ID, "", err
			}
			req, _ = buildRequest(conv, hist, prof, tcMap)
			req.Tools = tools
		}
		// Call the model (non-streaming or streaming). The wire request carries
		// the provider-side model name, not the internal ID (they differ
		// whenever the upstream name slugs to something else).
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
		// A successful call proves the model is available again — re-enable it
		// if it had been auto-disabled.
		if a.isAutoDisabled(conv.ActiveModelID) {
			a.SetModelAutoDisabled(conv.ActiveModelID, false)
		}
		if len(res.ToolCalls) == 0 {
			break // the final answer
		}
		if np.stopped() {
			// The stop: this call went out with NO tools on the wire, yet the
			// model still returned tool calls (runaway). Executing them would
			// defeat the stop — the turn ends here: the narration (plus the
			// "stopped, not done" notice) is persisted as the reply and the
			// calls are recorded as FAILED P7 rows (wire-valid complete pairs
			// — the model sees why they never ran, and the pair can never
			// orphan).
			res.Text = stripLeadingTranscriptTimestamp(res.Text) + stoppedNotice(np)
			assistantMsg, err := us.AppendMessage(convID, roleAssistant, res.Text, res.Reasoning, res.ReasoningSignature, &modelID)
			if err != nil {
				return conv.ID, "", err
			}
			for _, tc := range res.ToolCalls {
				p7id, err := us.AppendToolCall(assistantMsg.ID, tc.Name, tc.Arguments)
				if err != nil {
					return conv.ID, "", err
				}
				if cerr := us.CompleteToolCall(p7id, "not executed: the turn was stopped (no observed progress — the no-progress confidence reached its threshold)", false); cerr != nil {
					return conv.ID, "", cerr
				}
			}
			persistedInLoop = true
			break
		}
		// Persist the assistant row that carries the tool calls (its text is
		// the model's narration, possibly empty — the UI skips empty
		// assistant rows), then execute + record each call. A tool error is a
		// RESULT back to the model (the self-healing signal), never a turn
		// failure.
		res.Text = stripLeadingTranscriptTimestamp(res.Text)
		assistantMsg, err := us.AppendMessage(convID, roleAssistant, res.Text, res.Reasoning, res.ReasoningSignature, &modelID)
		if err != nil {
			return conv.ID, "", err
		}
		for ci, tc := range res.ToolCalls {
			p7id, err := us.AppendToolCall(assistantMsg.ID, tc.Name, tc.Arguments)
			if err != nil {
				return conv.ID, "", err
			}
			var resultText string
			var ok bool
			if ci >= maxToolCallsPerMessage {
				// Per-message call cap: the excess is recorded, not executed.
				resultText, ok = fmt.Sprintf("not executed: this message requested more than %d tool calls — split the work across fewer calls per reply", maxToolCallsPerMessage), false
			} else {
				resultText, ok = a.executeToolCall(ctx, handle, tc)
			}
			if cerr := us.CompleteToolCall(p7id, resultText, ok); cerr != nil {
				return conv.ID, "", cerr
			}
			// Feed the no-progress accumulator: the executed call (name, args,
			// result) is one observation. A failed / not-executed call (ok=false)
			// is not recorded (it is the model self-healing, not no-progress);
			// report_progress updates the pending declaration, not the action
			// window (it is excluded from L0/L1).
			np.observe(tc.Name, json.RawMessage(tc.Arguments), resultText, ok)
		}
		// Settle the accumulator for this iteration: evaluate the layers,
		// accumulate the confidence, apply decay. A check-in (a persistent,
		// tail-appended, UI-hidden roleCheckin row) is issued when a layer fired
		// and the rate-limit allows — UNLESS the turn is already stopping (then
		// the next call is text-only and a check-in is moot).
		if stop, checkin := np.settle(iter); checkin && !stop {
			if _, err := us.AppendMessage(convID, roleCheckin, checkinText, "", "", nil); err != nil {
				return conv.ID, "", err
			}
		}
	}
	if persistedInLoop {
		// The reply row (narration + the "stopped, not done" notice) was
		// persisted inside the loop (the runaway case); the post-loop path
		// below would double-persist it.
		return conv.ID, res.Text, nil
	}
	// The turn was stopped by the no-progress threshold (the model gave a final
	// answer on the forced text-only call): append the "stopped, not done"
	// notice so the user knows the trajectory was STOPPED, not finished (a
	// silent cut would look like a completed answer).
	if stopped {
		res.Text += stoppedNotice(np)
	}
	// Deterministic backstop for the no-echo rule: if the model echoed a
	// transcript timestamp marker into the start of its reply, strip it
	// before the reply is persisted (and before it is sent to the client on
	// the streaming "done" event / returned from Turn).
	res.Text = stripLeadingTranscriptTimestamp(res.Text)
	assistantMsg, err := us.AppendMessage(convID, roleAssistant, res.Text, res.Reasoning, res.ReasoningSignature, &modelID)
	if err != nil {
		return conv.ID, "", err
	}
	a.indexMessage(ctx, handle, assistantMsg, conv.Title)
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
	if cerr := a.compactIfNeeded(ctx, us, conv, p, actualTokens, prof); cerr != nil {
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

// executeToolCall dispatches one of the model's tool calls into the
// registry and returns the tool result text + success. Every failure mode
// is a RESULT back to the model (never a turn error): an unknown tool name
// (a model imitating a removed/renamed tool from its own history — the
// self-healing path of the tool-evolution invariants) gets an error naming
// the available tools; an executor error gets its message. Either way the
// model sees an actionable signal and can recover within the same turn.
func (a *Agent) executeToolCall(ctx context.Context, handle string, tc provider.ToolCall) (string, bool) {
	t, ok := a.tools.Get(tc.Name)
	if !ok {
		return fmt.Sprintf("unknown tool %q — the available tools are: %s. Use one of those instead.", tc.Name, strings.Join(a.tools.Names(), ", ")), false
	}
	out, err := t.Execute(ctx, handle, json.RawMessage(tc.Arguments))
	if err != nil {
		return fmt.Sprintf("tool %s failed: %v — check the arguments against the tool's schema and try again, or answer from what you already have.", tc.Name, err), false
	}
	return out, true
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
	// The turn is this user's LLM work for its full duration (queue + run) —
	// the idle learning pass's "no other LLM work running" gate reads this.
	a.llmWork.enter(handle)
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
		a.llmWork.leave(handle)
		return conv.ID, reply, retErr
	}
	if retErr == nil {
		// A clean turn: arm this conversation's idle learning pass (the
		// memory model's idle reflection — see learning.go). A canceled
		// turn never arms (its messages may be incomplete).
		a.learn.onTurnComplete(handle, convID)
	}
	a.llmWork.leave(handle)
	return conv.ID, reply, retErr
}
