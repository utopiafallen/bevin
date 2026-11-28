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
	"errors"
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
	"github.com/utopiafallen/bevin/internal/sandbox"
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

	// containerBk resolves the LIVE container backend (nil = a platform
	// with no container backend, where the file tools answer
	// "unavailable"). Set once at startup by the server; it resolves on
	// every use because the backend is (re)built on config save.
	containerBk func() sandbox.Backend

	// diskRecheck re-checks a user's disk usage immediately after a
	// workspace delete (the server's delete endpoint calls the same
	// re-check — a delete frees space, and the frozen state + the disk
	// page should reflect it without waiting for the next monitor walk).
	// Nil-safe (a nil hook = the 45 s monitor walk catches it).
	diskRecheck func(handle string)

	// roster is the in-memory turn-start awareness state (concurrent-
	// instances, layers 2+4): the in-flight set, the last-announced roster
	// snapshots, and the purpose-nudge counters (see roster.go).
	roster *rosterState

	// tasks is the minimal task system (tier 2): harness-spawned units of
	// work the agent works done in owned conversations, through the same
	// per-model slot pools (see tasks.go).
	tasks *taskSystem

	// ptySessions is the task-bound pty session tracker (the design log's
	// WS2 contract: a pty session's lifetime is bound to its task — task
	// end, whatever the outcome, ends the sessions). conversation id →
	// the session ids the conversation started (env_pty_start records
	// here; the task runner's settle path closes them).
	ptySessMu   sync.Mutex
	ptySessions map[string]map[string]bool

	// NotifyMessage, if set, is called after EVERY row persisted into a
	// user's conversation (any role — user, assistant, checkin, summary).
	// The server wires it to the per-user event hub: the nudge that tells
	// the user's other open tabs (desktop + phone) the conversation
	// changed, so they re-pull (multi-device sync). The agent reaches it
	// through each user store's AfterAppend lifecycle hook (set in
	// userStore), so EVERY persistence site fires it — a new call site
	// can't forget. It runs synchronously in the persisting goroutine —
	// keep it non-blocking and cheap (the client reconciles from the
	// store, so a dropped nudge only delays the other tab by one nudge; it
	// can never lose or duplicate content).
	NotifyMessage func(handle, convID string, seq int)

	// NotifyConversation, if set, is called after one of the user's
	// conversation rows changed in a way the conversation LIST must
	// reflect (currently: the rolling purpose, via the set_purpose tool —
	// the purpose renders in place of the title as the list label). The
	// server wires it to the per-user event hub's "conversation" nudge;
	// the client already refreshes its list on that event. Same contract
	// as NotifyMessage: the caller's goroutine, non-blocking,
	// concurrency-safe.
	NotifyConversation func(handle, convID string)

	// NotifyTaskTerminal, if set, is called when a background task
	// settles into a terminal state (done/blocked — after the
	// TransitionTask + completion-summary post). The server wires it to
	// the web-push fan-out (the PWA stage 2 "Task 'X' is done"
	// notification). summary is the raw report (the advance_task note
	// when given, else the final reply) — framing + truncation happen on
	// the server side. A task the user CANCELED fires nothing (the user
	// initiated it). Same contract as the nudges: the caller's goroutine,
	// non-blocking, concurrency-safe, best-effort.
	NotifyTaskTerminal func(handle, taskTitle, convID, state, summary string)

	cfgMu sync.Mutex // guards reads/writes of cfg.Models (the auto-disable state)
}

// New builds an Agent from a loaded config. It fails if no model is enabled.
func New(cfg *config.ServerConfig, dataDir string) (*Agent, error) {
	// The aux pool ids are a derived relationship (config.AssignAuxPoolIDs
	// normally runs in Validate); re-derive defensively so a config that
	// skipped validation still carries correct pool keys.
	config.AssignAuxPoolIDs(cfg)
	a := &Agent{
		cfg:         cfg,
		dataDir:     dataDir,
		providers:   map[string]provider.Provider{},
		users:       map[string]*store.UserStore{},
		ptySessions: map[string]map[string]bool{},
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
	// The workspace tools (the design log's WORKSPACE TOOLS section): the
	// agent manages workspace STATE on the user's behalf — create/edit/
	// delete/list. Registered unconditionally like edit_file (a user's own
	// workspaces have no admin toggle).
	if err := registerWorkspaceTools(a.tools, a); err != nil {
		return nil, fmt.Errorf("register workspace tools: %w", err)
	}
	// The skill system (the design log's SKILL SYSTEM + WORKSPACE LOADING
	// sections): the six skill tools + set_workspace. Registered
	// unconditionally — the user's own data, like the workspace tools.
	if err := registerSkillTools(a.tools, a); err != nil {
		return nil, fmt.Errorf("register skill tools: %w", err)
	}
	// exec (the design log's AGENT SANDBOX TOOL section): the blocking
	// one-shot command in the user's persistent environment (auto-ensured
	// on first use; a non-zero exit is a result, not an error).
	if err := registerExecTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register exec: %w", err)
	}
	// The pty tools (the design log's WS2 section): the interactive half
	// (interactive_exec + env_pty_wait/input/close) — the pty-session
	// conversation on top of the same environment.
	if err := registerPtyTools(a.tools, a); err != nil {
		return nil, fmt.Errorf("register pty tools: %w", err)
	}
	// The memory tools (the memory model's write surface: remember /
	// forget over the user's P5 memories — retrieval is the search tool's
	// memory leg, rankMemories).
	if err := registerMemoryTools(a.tools, a); err != nil {
		return nil, fmt.Errorf("register memory tools: %w", err)
	}
	// The search tool (the user's search page, agent-side): one query over
	// all of the user's data — conversations (semantic), memories (the
	// hybrid blend that recall used to do on its own), profile, workspace
	// files — with optional conversation / workspace scopes.
	if err := registerSearchTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register search: %w", err)
	}
	// The concurrent-instances tools (layers 2+4): set_purpose (the rolling
	// purpose the roster shows to siblings) + agent_message (the sibling
	// queue — queued, delivered at the recipient's next user turn).
	if err := registerPurposeTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register set_purpose: %w", err)
	}
	if err := registerAgentMessageTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register agent_message: %w", err)
	}
	// advance_task (the minimal task system): the model's own report of how
	// its task ended (done/blocked). Registered for every conversation like
	// the others; OFFERED only in a task's owned conversation (the wire
	// filter keys on the conversation's owner_task_id — see wireTools).
	if err := registerAdvanceTaskTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register the advance task tool: %w", err)
	}
	if err := registerSetProfileTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register the set profile tool: %w", err)
	}
	// image_analyze (the aux perception loop, stage 1): the vision aux
	// model's chat-attachment surface. Registered unconditionally like the
	// others; OFFERED only while the live config has a vision-capable aux
	// model (the wireTools filter — an admin save applies from the next
	// turn, no restart; see imagetools.go).
	if err := registerImageAnalyzeTool(a.tools, a); err != nil {
		return nil, fmt.Errorf("register image_analyze: %w", err)
	}
	// The turn-start awareness state (the roster/nudge/queue delivery —
	// in-memory only: a restart re-announces the full roster, which is
	// correct for a cold start).
	a.roster = newRosterState()
	// The minimal task system (tier 2). Its boot reset re-works tasks that
	// were in-progress when the process last died (runtime state is
	// in-memory by design — see tasks.go).
	a.tasks = newTaskSystem(a)
	a.tasks.resetStaleOnBoot()
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
// advance_task is filtered out of every NON-task conversation (it can only
// report the outcome of the task the conversation is working — one offered
// in a chat conversation could only be refused), image_analyze is filtered
// out while the live config has no vision-capable aux model (the same
// live-config pattern as web_search — see imagetools.go), and the
// web_browser description carries a per-turn SEARCH note matching the
// web_search state (prefer web_search when it is on the wire; when it is
// not, search-engine tabs — user directive: the agent uses web_browser to
// search). The registry entries are untouched — the wire copies carry the
// note (Wire() returns fresh copies each turn).
func (a *Agent) wireTools(us *store.UserStore, conv *store.Conversation) []provider.Tool {
	wsOn := a.webSearchState().Configured()
	visionOn := a.HasVisionAux()
	list := a.tools.Wire()
	var ownerTaskID *string
	curationConv := false
	if conv != nil {
		ownerTaskID = conv.OwnerTaskID
		// set_profile is the curation task's sole-writer surface: offered
		// ONLY in the curation task's own conversation (one store lookup,
		// and only in task conversations — the common case is one nil).
		if ownerTaskID != nil && us != nil {
			if t, err := us.GetTask(*ownerTaskID); err == nil && t.Title == CurationTaskTitle {
				curationConv = true
			}
		}
	}
	filtered := make([]provider.Tool, 0, len(list))
	for _, t := range list {
		if !wsOn && t.Name == "web_search" {
			continue
		}
		if !visionOn && t.Name == "image_analyze" {
			continue
		}
		if ownerTaskID == nil && t.Name == "advance_task" {
			continue
		}
		if !curationConv && t.Name == "set_profile" {
			continue
		}
		// The pty split (the design log's WS3 reshape): interactive_exec
		// is the USER agent's task-spawner — a task conversation must
		// not nest another task (v1). The env_pty_* tools are the TASK
		// agent's session surface — the user's live turn never drives a
		// pty in-turn (that is what interactive_exec's background task is
		// for).
		if ownerTaskID != nil && t.Name == "interactive_exec" {
			continue
		}
		if ownerTaskID == nil && isPtyTool(t.Name) {
			continue
		}
		filtered = append(filtered, t)
	}
	list = filtered
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
// takes no explicit effort, and the UI hides the control). The argument is a
// model REFERENCE (a display name or a concrete model ID) — it resolves to
// the concrete provider first (a display name takes the least-loaded; all
// providers of a display name share the model's profile, so the efforts are
// the same regardless of which provider it lands on). The live-config
// resolution is the same as profileFor: a profile reassignment applies from
// the next read, no restart.
func (a *Agent) ReasoningEffortsFor(ref string) []string {
	resolved, ok := a.resolveModelRef(ref)
	if !ok {
		return []string{}
	}
	efforts := a.profileFor(resolved).ReasoningEfforts
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
	// The aux pool ids track the live model set (a main model added or
	// removed changes which pool an aux joins) — re-derive before the swap
	// so the pokes below reach the right pools.
	config.AssignAuxPoolIDs(cfg)
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
	// Every pool that may have changed capacity: the main models' + the aux
	// pools (an aux slot edit, or an aux joining/leaving a main model's pool
	// as the catalog changes, must reach the scheduler live). A pool id only
	// ever gets poked once (a shared aux reuses the main's id).
	poked := map[string]bool{}
	for _, m := range cfg.Models {
		if !poked[m.ID] {
			a.sched.Poke(m.ID)
			poked[m.ID] = true
		}
	}
	for _, x := range cfg.Aux {
		if x.PoolID != "" && !poked[x.PoolID] {
			a.sched.Poke(x.PoolID)
			poked[x.PoolID] = true
		}
	}
	return nil
}

// SetDiskFrozen wires the disk-cap frozen check into the scheduler: a user
// over their disk cap has their queued work deferred (kept in the queue)
// until they're back under the cap. The host (the server's disk monitor)
// installs this. Nil-safe: passing nil defers no one.
func (a *Agent) SetDiskFrozen(f func(handle string) bool) {
	a.sched.SetFrozen(f)
}

// SetContainerBackend wires a resolver for the LIVE container backend into
// the file tools (edit_file operates INSIDE the user's container — the
// container is the shared medium between the user's parallel
// conversations). The resolver is called on every use: the backend is
// (re)built on config save, and the file tools must always reach the live
// one.
func (a *Agent) SetContainerBackend(resolve func() sandbox.Backend) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	a.containerBk = resolve
}

// containerBackend resolves the live container backend (nil = unavailable).
func (a *Agent) containerBackend() sandbox.Backend {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if a.containerBk == nil {
		return nil
	}
	return a.containerBk()
}

// SetDiskRecheck wires the immediate post-delete disk re-check (the
// server's workspace-delete endpoint's hook; the agent's workspace_delete
// tool rides it too). Nil-safe: passing nil defers the re-check to the
// next monitor walk.
func (a *Agent) SetDiskRecheck(f func(handle string)) {
	a.diskRecheck = f
}

// recheckDiskUsage runs the wired post-delete re-check (nil-safe).
func (a *Agent) recheckDiskUsage(handle string) {
	if a.diskRecheck != nil {
		a.diskRecheck(handle)
	}
}

// diskIsFrozen reports whether the user is over their disk cap (the same
// check the scheduler defers on — file writes refuse while frozen).
func (a *Agent) diskIsFrozen(handle string) bool {
	return a.sched.Frozen(handle)
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

// DisplayModel is one row of the user's model picker: a DISPLAY NAME (the
// stable user-facing model identity — the picker's key) + the POOLED
// execution-slot count (the sum of the slots of every available provider
// serving that display name — adding a provider adds to the display model's
// pool) + the representative context/scores/effort (from the first provider
// in catalog order). The concrete provider a conversation actually lands on
// is chosen at selection time (the least-loaded — see resolveModelRef) and
// is an internal detail; the picker shows the display model, not the
// providers.
type DisplayModel struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	MaxContext       int      `json:"max_context"`
	Slots            int      `json:"slots"`
	Reasoning        int      `json:"reasoning"`
	Knowledge        int      `json:"knowledge"`
	Speed            int      `json:"speed"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
}

// DisplayModels returns the user's model-picker rows: one per DISPLAY NAME
// (deduped — a display name maps one-to-many to model providers, and the
// picker surfaces each display name once), with the pooled slot count (the
// sum of the slots of every available provider serving it) + the
// representative context/scores/effort (from the first provider in catalog
// order).
func (a *Agent) DisplayModels() []DisplayModel {
	models := a.Models() // available (concrete) models, catalog order
	type group struct {
		repID string // the representative provider's concrete model ID (first in catalog order)
		slots int    // the pooled slot count
	}
	order := []string{}
	groups := map[string]*group{}
	for _, m := range models {
		g, ok := groups[m.DisplayName]
		if !ok {
			g = &group{repID: m.ID}
			groups[m.DisplayName] = g
			order = append(order, m.DisplayName)
		}
		slots := m.Slots
		if slots < 1 {
			slots = 1
		}
		g.slots += slots
	}
	out := make([]DisplayModel, 0, len(order))
	for _, dn := range order {
		g := groups[dn]
		var rep config.Model
		found := false
		for _, m := range models {
			if m.ID == g.repID {
				rep = m
				found = true
				break
			}
		}
		if !found {
			continue
		}
		out = append(out, DisplayModel{
			ID:               dn,
			DisplayName:      dn,
			MaxContext:       rep.MaxContext,
			Slots:            g.slots,
			Reasoning:        rep.Metadata.Reasoning,
			Knowledge:        rep.Metadata.Knowledge,
			Speed:            rep.Metadata.Speed,
			ReasoningEfforts: a.ReasoningEffortsFor(g.repID),
		})
	}
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

// isContextOverflow reports whether a provider error means the prompt exceeded
// the model's context (as opposed to a transient failure or an unavailable
// model). It inspects the provider's error string — "<provider>: <status>:
// <body>" — for the phrases each backend actually emits:
//
//   - llama.cpp: "input (N tokens) is larger than the max context size (M
//     tokens)" / "request (N tokens) exceeds the available context size (M
//     tokens)" (ERROR_TYPE_EXCEED_CONTEXT_SIZE), and "context size exceeded"
//     on older builds.
//   - OpenAI-compatible: "maximum context length".
//   - Anthropic: "prompt is too long" / "too many tokens".
//
// A false positive is harmless (at most one extra forced compaction + one
// retry), so the match is deliberately broad.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	phrases := []string{
		"larger than the max context size",   // llama.cpp
		"exceeds the available context size", // llama.cpp
		"context size exceeded",              // llama.cpp (older)
		"maximum context length",             // OpenAI-compatible
		"prompt is too long",                 // Anthropic
		"too many tokens",                    // Anthropic / generic
		"context length exceeded",
		"context overflow",
		"n_ctx", // llama.cpp (any message naming the limit)
	}
	for _, p := range phrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
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
	// The multi-device nudge rides on the store's own lifecycle hook: every
	// AppendMessage on this user's store — from ANY layer or future call
	// site — fires NotifyMessage (the server wires it to the event hub).
	// The handle is captured per store, so concurrent turns on different
	// users (and parallel turns on this user) each nudge the right tabs.
	us.AfterAppend = func(convID string, seq int) {
		if a.NotifyMessage != nil {
			a.NotifyMessage(handle, convID, seq)
		}
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

// NewConversationModel resolves the model REFERENCE a brand-new conversation
// for the user takes when the request does not pin one: their per-user
// default from agent.toml (set in first-time onboarding — a DISPLAY NAME,
// since the picker is keyed by display name) while it still resolves to an
// available model, else the server default's display name ("" when no model
// is available at all). It returns a display name so the client's
// new-conversation preview (which mirrors it by matching /api/me's
// default_model_id against the deduped /api/models rows' ids) agrees. The
// create endpoint and the preview MUST agree — this is the single source of
// truth for the resolution; the concrete provider is chosen when the
// reference is resolved (newUserConversation → resolveModelRef).
func (a *Agent) NewConversationModel(handle string) string {
	if ac, err := config.LoadAgent(filepath.Join(a.dataDir, "users", handle)); err == nil && ac.DefaultModelID != "" {
		if resolved, ok := a.resolveModelRef(ac.DefaultModelID); ok {
			return a.displayNameFor(resolved)
		}
	}
	return a.DefaultModelDisplayName()
}

// newUserConversation creates a conversation with modelID + an optional
// reasoning effort, the frozen system prompt set at creation. modelID "" =
// the user's preferred model (NewConversationModel, a display name); a
// pinned modelID is a model reference (a display name OR a concrete model
// ID). The reference is RESOLVED to a concrete model ID (a display name
// takes the least-loaded available provider that serves it — the execution
// pool; a concrete id keeps itself), and the conversation KEEPS that
// concrete id (a stable provider — its KV cache is preserved across the
// conversation's turns). effort "" = the model's profile default; a pinned
// effort must be one of the model's profile levels.
func (a *Agent) newUserConversation(us *store.UserStore, handle, title, modelID, effort string) (*store.Conversation, error) {
	if modelID == "" {
		modelID = a.NewConversationModel(handle)
	}
	resolved, ok := a.resolveModelRef(modelID)
	if !ok || resolved == "" {
		return nil, fmt.Errorf("model %s is not available", modelID)
	}
	modelID = resolved
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
	// Freeze the system prompt at conversation start: the base core + the
	// user's CURRENT profile section (design log 2026-11-25 "Profile in the
	// system prompt"). The profile is re-baked at every compaction event;
	// the core bytes never change for the life of the conversation.
	ptxt, err := us.ProfileText()
	if err != nil {
		return nil, err
	}
	if err := us.SetFrozenSystemPrompt(c.ID, prompt.SystemPrompt(ptxt)); err != nil {
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

// OnboardingState reports the user's explicit onboarding state (the
// user_state rows in their store — the source of truth; the agent.toml flag
// is legacy, backfilled on first read after an upgrade). complete=false:
// never onboarded. convID: the seeded conversation's id — "" when the flow
// is complete but the conversation no longer exists (the user deleted it;
// it is deliberately NOT re-seeded, the state is the truth).
func (a *Agent) OnboardingState(handle string) (complete bool, convID string, err error) {
	us, err := a.userStore(handle)
	if err != nil {
		return false, "", err
	}
	done, ok, err := us.GetUserState(store.UserStateKeyOnboardingDone)
	if err != nil {
		return false, "", err
	}
	if !ok || done != "1" {
		return false, "", nil
	}
	id, _, err := us.GetUserState(store.UserStateKeyOnboardingConv)
	if err != nil {
		return true, "", err
	}
	return true, id, nil
}

// MarkOnboarded records onboarding completion in the user's store (both
// state rows — the conv id first, the done flag last, so a crash between
// the two leaves the user NOT complete: the next attempt recovers via the
// title lookup in Onboard). convID may be "" (a legacy backfill with no
// findable conversation).
func (a *Agent) MarkOnboarded(handle, convID string) error {
	us, err := a.userStore(handle)
	if err != nil {
		return err
	}
	if err := us.SetUserState(store.UserStateKeyOnboardingConv, convID); err != nil {
		return err
	}
	return us.SetUserState(store.UserStateKeyOnboardingDone, "1")
}

// Onboard completes the user's first-time "Meet Bevin" flow — IDEMPOTENT,
// keyed on the explicit user_state row, never on the conversation's
// existence:
//
//   - complete → returns the seeded conversation's id ("" when the user
//     deleted it — no re-seed; the flow is done).
//   - incomplete → seeds the conversation (chosen default model, standard
//     frozen system prompt, ONE welcome message) and records completion;
//     a crashed prior attempt is ADOPTED (title lookup — best-effort
//     recovery, the state row is the idempotency key) and completed
//     instead of duplicating.
//
// The onboarding content lives in the message, never the prompt, so the
// conversation stays a normal conversation forever. modelID was validated
// by the caller (a model reference — a display name or a concrete model
// ID); it is resolved to a concrete model ID (a display name takes the
// least-loaded available provider that serves it), falling back to the
// server default.
func (a *Agent) Onboard(ctx context.Context, handle, modelID, welcome string) (string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", err
	}
	if done, ok, err := us.GetUserState(store.UserStateKeyOnboardingDone); err != nil {
		return "", err
	} else if ok && done == "1" {
		// Complete: the stored conv id is the answer. It may be gone (the
		// user deleted the conversation — stay complete, don't re-seed).
		stored, _, err := us.GetUserState(store.UserStateKeyOnboardingConv)
		if err != nil {
			return "", err
		}
		if stored != "" {
			if _, err := us.GetConversation(stored); err == nil {
				return stored, nil
			}
		}
		return "", nil
	}
	// Recovery: a prior attempt may have seeded the conversation and died
	// before recording completion (the title lookup is best-effort
	// recovery here — NOT the idempotency key).
	if conv, err := us.FindConversationByTitle(onboardingTitle); err == nil {
		if has, herr := us.HasMessages(conv.ID); herr == nil && !has && welcome != "" {
			if m, aerr := us.AppendMessage(conv.ID, "assistant", welcome, "", "", nil); aerr == nil {
				a.indexMessage(ctx, handle, m, onboardingTitle)
			}
		}
		if err := a.MarkOnboarded(handle, conv.ID); err != nil {
			return "", err
		}
		return conv.ID, nil
	}
	// Fresh seed: resolve the model, create + freeze + welcome, then record
	// completion (a crash between the seed and the record is recovered on
	// the next call by the adoption path above — never a duplicate).
	if modelID == "" || !a.ModelAvailable(modelID) {
		modelID = a.DefaultModelID()
	}
	resolved, ok := a.resolveModelRef(modelID)
	if !ok || resolved == "" {
		return "", fmt.Errorf("no available models to start a conversation")
	}
	c, err := us.CreateConversation(onboardingTitle, resolved)
	if err != nil {
		return "", err
	}
	// Same freeze as every conversation: base core + the user's current
	// profile section (re-baked at each compaction — see the prompt
	// package's SystemPrompt).
	ptxt, err := us.ProfileText()
	if err != nil {
		return "", err
	}
	if err := us.SetFrozenSystemPrompt(c.ID, prompt.SystemPrompt(ptxt)); err != nil {
		return "", err
	}
	if welcome != "" {
		if m, err := us.AppendMessage(c.ID, "assistant", welcome, "", "", nil); err != nil {
			return "", err
		} else {
			a.indexMessage(ctx, handle, m, onboardingTitle)
		}
	}
	if err := a.MarkOnboarded(handle, c.ID); err != nil {
		return "", err
	}
	return c.ID, nil
}

// OnboardingConversationID returns the id of the user's seeded "Meet Bevin"
// conversation, best-effort ("" when it doesn't exist — e.g. the user
// deleted it). Exact-title lookup: a recovery / backfill convenience, NOT
// an idempotency key (the user_state rows are).
func (a *Agent) OnboardingConversationID(handle string) (string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return "", err
	}
	c, err := us.FindConversationByTitle(onboardingTitle)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return c.ID, nil
}

// resolveModelRef resolves a model reference (a display name OR a concrete
// model ID) to a concrete model ID. A concrete model ID (available)
// resolves to itself. A display name resolves to the least-loaded available
// provider that serves it — the many-to-one display-name→provider mapping
// (the execution pool is the union of the providers' slots, and a new
// conversation takes the provider with the most idle capacity, ties broken
// by catalog order via config.ResolveDisplayName). ok=false when the
// reference names no available model.
//
// This is the single entry point for model SELECTION (conversation create +
// switch + the per-user default). The conversation KEEPS the resolved
// concrete model ID (a stable provider — its KV cache is preserved across
// the conversation's turns); it is not re-resolved per turn.
func (a *Agent) resolveModelRef(ref string) (string, bool) {
	a.cfgMu.Lock()
	models := make([]config.Model, len(a.cfg.Models))
	copy(models, a.cfg.Models)
	a.cfgMu.Unlock()
	// 1. A concrete model ID (exact match, available) resolves to itself.
	for _, m := range models {
		if m.ID == ref && m.Available() {
			return m.ID, true
		}
	}
	// 2. A display name resolves to the least-loaded available provider.
	for _, m := range models {
		if m.DisplayName == ref && m.Available() {
			if resolved, ok := config.ResolveDisplayName(models, ref, a.idleSlotsFor); ok && resolved.ID != "" {
				return resolved.ID, true
			}
		}
	}
	return "", false
}

// idleSlotsFor returns a model's idle execution slots (its slot count minus
// how many are busy right now, from the scheduler) — the score
// config.ResolveDisplayName uses to pick the least-loaded provider for a
// display name. Never negative.
func (a *Agent) idleSlotsFor(m config.Model) int {
	slots := a.modelSlots(m.ID)
	busy := 0
	if l, ok := a.sched.Load()[m.ID]; ok {
		busy = l.Busy
	}
	if slots > busy {
		return slots - busy
	}
	return 0
}

// resolveModelRefTracked is resolveModelRef with the bevin-side KV-cache
// backend preference (2026-11-27): when the reference is a display name
// served by several providers, the provider on trackedBackend — the llama
// backend whose --slot-save-path holds the conversation's KV cache file — is
// preferred over the least-loaded one, so long as it is still available: the
// conversation's next model call then loads its cache and prefills only the
// delta instead of a full prefill. A stale tracking (no provider on that base
// URL, or the provider no longer available) falls back to the normal
// least-loaded resolution. Concrete model IDs resolve to themselves, as in
// resolveModelRef. trackedBackend "" = plain resolveModelRef.
func (a *Agent) resolveModelRefTracked(ref, trackedBackend string) (string, bool) {
	a.cfgMu.Lock()
	models := make([]config.Model, len(a.cfg.Models))
	copy(models, a.cfg.Models)
	a.cfgMu.Unlock()
	// 1. A concrete model ID (exact match, available) resolves to itself.
	for _, m := range models {
		if m.ID == ref && m.Available() {
			return m.ID, true
		}
	}
	// 2. The KV-cache holder preference: a provider on the tracked backend
	// that serves the display name and is available wins over
	// least-loaded (cache locality — the conversation's cache file lives
	// on that backend).
	if trackedBackend != "" {
		for _, m := range models {
			if m.DisplayName == ref && m.BaseURL == trackedBackend && m.Available() {
				return m.ID, true
			}
		}
	}
	// 3. Otherwise the least-loaded available provider (the plain rule).
	for _, m := range models {
		if m.DisplayName == ref && m.Available() {
			if resolved, ok := config.ResolveDisplayName(models, ref, a.idleSlotsFor); ok && resolved.ID != "" {
				return resolved.ID, true
			}
		}
	}
	return "", false
}

// kvBackendFor returns the base URL of the llama backend to record as the
// holder of a conversation's KV cache file after a model call on modelID
// completes — "" for a non-llama model (its backend holds no bevin-managed
// KV file) or an unknown model.
func (a *Agent) kvBackendFor(modelID string) string {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, m := range a.cfg.Models {
		if m.ID == modelID && m.Provider == "llama" {
			return m.BaseURL
		}
	}
	return ""
}

// backendInConfig reports whether any model in the live config points at the
// base URL (the tracked KV-cache holder's server is still configured —
// possibly temporarily unavailable, in which case the tracking is kept).
func (a *Agent) backendInConfig(baseURL string) bool {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, m := range a.cfg.Models {
		if m.BaseURL == baseURL {
			return true
		}
	}
	return false
}

// displayNameFor returns a concrete model ID's display name (the user-facing
// model identity) — "" when the ID is unknown (a model removed from the
// config). The conversation summary carries it so the picker (keyed by
// display name) can highlight the active model.
func (a *Agent) displayNameFor(concreteModelID string) string {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	for _, m := range a.cfg.Models {
		if m.ID == concreteModelID {
			return m.DisplayName
		}
	}
	return ""
}

// DefaultModelDisplayName returns the server default model's display name —
// the /api/models "default" field, the client's new-conversation preview
// fallback when the user has no per-user default (now a display name, to
// match the deduped picker rows).
func (a *Agent) DefaultModelDisplayName() string {
	a.mu.Lock()
	id := a.defaultModel
	a.mu.Unlock()
	return a.displayNameFor(id)
}

// ModelAvailable reports whether the model reference (a display name or a
// concrete model ID) resolves to an available model — usable for new work
// right now. A model that is merely offline can still be referenced by a
// conversation in flight.
func (a *Agent) ModelAvailable(ref string) bool {
	_, ok := a.resolveModelRef(ref)
	return ok
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

// SearchConversationsInConversation is SearchConversations restricted to one
// of the user's conversations (the search scope). The restriction happens in
// Qdrant (payload filter on conversation_id). An unconfigured index yields an
// empty result, like SearchConversations.
func (a *Agent) SearchConversationsInConversation(ctx context.Context, handle, conversationID, query string, limit int) ([]SearchHit, error) {
	vi := a.vectorIndex()
	if vi == nil {
		return []SearchHit{}, nil
	}
	return vi.SearchInConversation(ctx, handle, conversationID, query, limit)
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

// profileHit is one profile match (a key or value containing the query) —
// shared by SearchAll's JSON output and the search tool's result text.
type profileHit struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// SearchAll backs the dedicated search page + the search tool: it searches
// the user's data across every source at once — conversations (semantic,
// the per-user vector index), memories (recall's hybrid blend — semantic +
// confidence + recency + the lexical floor; recall was folded in 2026-11-27),
// the profile (case-insensitive substring over keys + values), and, when a
// workspace is named, that workspace's files (name + path match — file
// CONTENTS are not indexed yet, metadata only).
//
// Two independent, optional scopes: conversationID restricts the
// CONVERSATION leg to that conversation (a Qdrant payload filter) and the
// MEMORY leg to memories whose provenance touches it; workspaceID restricts
// the FILE leg to that workspace. The profile is always searched whole — it
// is the user's global context, with no conversation or workspace
// dimension. Empty scopes = all of the user's data.
//
// The user's workspaces are returned in `workspaces` (and their
// conversations' summaries in `conversations_scope`) so the search page's
// scope selectors can offer them. The vector-index leg is best-effort (an
// unconfigured index or a sidecar blip leaves the local sources intact);
// a store failure is a real error.
func (a *Agent) SearchAll(ctx context.Context, handle, query, conversationID, workspaceID string, limit int) (map[string]any, error) {
	if limit <= 0 {
		limit = 10
	}
	out := map[string]any{
		"conversations": []any{},
		"memories":      []any{},
		"profile":       []any{},
		"workspace":     []any{},
	}
	if conversationID != "" {
		if hits, err := a.SearchConversationsInConversation(ctx, handle, conversationID, query, limit); err == nil {
			out["conversations"] = hits
		}
	} else if hits, err := a.SearchConversations(ctx, handle, query, limit); err == nil {
		out["conversations"] = hits
	}
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	// Validate the named scopes before any search: a bad id is a 404, not a
	// silent empty result (mirrors the workspace scope's contract).
	if conversationID != "" {
		if _, err := us.GetConversation(conversationID); err != nil {
			return nil, err
		}
	}
	// The MEMORY leg: recall's hybrid blend (semantic + confidence +
	// recency + the lexical floor) — recall was folded into search
	// (2026-11-27), so SearchAll is the one memory-retrieval surface.
	// Best-effort like the conversation leg: a store failure here leaves
	// the other sources intact.
	if mems, merr := a.rankMemories(ctx, us, query, conversationID, limit); merr == nil && mems != nil {
		out["memories"] = mems
	}
	kv, err := us.GetProfile()
	if err != nil {
		return nil, err
	}
	var profileHits []any
	for k, v := range kv {
		if strings.Contains(strings.ToLower(k), strings.ToLower(query)) || strings.Contains(strings.ToLower(v), strings.ToLower(query)) {
			profileHits = append(profileHits, profileHit{k, v})
		}
	}
	out["profile"] = profileHits
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
	// Delete the conversation's KV cache across every llama provider: each
	// distinct llama server releases the conversation's slot cache (in memory
	// and on disk — the server's delete action owns the filesystem). Best-
	// effort — a failure never affects the conversation deletion itself.
	a.deleteLlamaConversationCache(ctx, convID)
	return nil
}

// deleteLlamaConversationCache issues a KV-cache delete for a deleted
// conversation on every distinct llama server in the config (the server
// releases the conversation's slot cache — memory and disk). It dedupes by
// base URL (several models may share one llama server; the cache is a
// per-server resource) and is best-effort (a server blip is logged inside the
// provider, never returned).
func (a *Agent) deleteLlamaConversationCache(ctx context.Context, convID string) {
	a.cfgMu.Lock()
	type srv struct{ base, key string }
	seen := map[string]struct{}{}
	var servers []srv
	for _, m := range a.cfg.Models {
		if m.Provider != "llama" {
			continue
		}
		if _, ok := seen[m.BaseURL]; ok {
			continue
		}
		seen[m.BaseURL] = struct{}{}
		servers = append(servers, srv{m.BaseURL, m.APIKey})
	}
	a.cfgMu.Unlock()
	for _, s := range servers {
		provider.DeleteLlamaConversationCache(ctx, s.base, convID, s.key)
	}
}

// ConversationSummary is one row of the user's conversation list: the
// identity + scheduling facts the UI needs (no frozen prompt, no messages).
type ConversationSummary struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	CreatedAt      string `json:"created_at"`
	LastActivityAt string `json:"last_activity_at"`
	ActiveModelID  string `json:"active_model_id"`
	// DisplayName: the active model's display name (the user-facing model
	// identity — the picker row key). A display name maps one-to-many to
	// model providers; active_model_id is the CONCRETE provider the
	// conversation is pinned to (its KV cache is preserved), and this is
	// how the picker (keyed by display name) highlights the active model.
	// "" when the model was removed from the config.
	DisplayName string `json:"display_name"`
	// ReasoningEffort: the conversation's stored reasoning-effort choice
	// ("" = the model's own default — the UI's dropdown then shows the
	// default).
	ReasoningEffort string `json:"reasoning_effort"`
	// Purpose: the conversation's rolling self-reported purpose (the
	// set_purpose tool). The UI renders it IN PLACE OF the title as the
	// list label ("a title that has nothing to do with the current state
	// is not useful" — the design decision); the title stays the
	// pre-purpose fallback (the first ~7 turns, before the first purpose
	// check lands).
	Purpose string `json:"purpose"`
	// WorkspaceID: the workspace this conversation is loaded into (the
	// WORKSPACE LOADING section) — the UI renders it on the list row +
	// the conversation's workspace picker. "" = no workspace.
	WorkspaceID string `json:"workspace_id"`
}

// conversationSummary builds a ConversationSummary from a store conversation,
// resolving the display name (the user-facing model identity) from the
// concrete active model ID.
func (a *Agent) conversationSummary(c store.Conversation) ConversationSummary {
	return ConversationSummary{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, LastActivityAt: c.LastActivityAt, ActiveModelID: c.ActiveModelID, DisplayName: a.displayNameFor(c.ActiveModelID), ReasoningEffort: c.ReasoningEffort, Purpose: c.Purpose, WorkspaceID: c.WorkspaceID}
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
		out = append(out, a.conversationSummary(c))
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
	s := a.conversationSummary(*c)
	return &s, nil
}

// UpdateConversation changes a conversation's active model, title, and/or
// reasoning-effort choice (any of them; at least one is required, enforced by
// the caller). The model must be available right now — also enforced by the
// caller; the effort is validated against the (possibly new) model's profile
// levels by the caller too. The changes apply to the NEXT turn: an in-flight
// turn keeps the model + effort it started on (the scheduler already queued
// it in that model's pool). Returns the updated summary. store.ErrNotFound
// when the conversation is not in the user's store.
func (a *Agent) UpdateConversation(handle, convID string, modelID, title, reasoningEffort, workspaceID *string) (*ConversationSummary, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, err
	}
	cur, err := us.GetConversation(convID)
	if err != nil {
		return nil, err
	}
	if modelID != nil && *modelID != "" {
		// Resolve the model reference (a display name or a concrete model ID)
		// to a concrete model ID: a display name takes the least-loaded
		// available provider that serves it (the execution pool), a concrete id
		// keeps itself. The conversation KEEPS the resolved concrete id (a
		// stable provider — its KV cache is preserved).
		// KV-cache holder preference (2026-11-27): when the reference is a
		// display name with several providers, the provider on the backend
		// that HOLDS this conversation's KV cache file wins over
		// least-loaded (the next model call loads the cache and prefills
		// only the delta). A tracking whose backend is gone from the config
		// is stale — cleared here (the next llama completion re-records a
		// live holder) and the plain least-loaded rule applies.
		tracked, terr := us.GetConversationKVBackend(convID)
		if terr != nil {
			tracked = ""
		}
		if tracked != "" && !a.backendInConfig(tracked) {
			tracked = ""
			if cerr := us.SetConversationKVBackend(convID, ""); cerr != nil {
				log.Printf("bevin: clearing stale kv-cache backend tracking %s: %v", convID, cerr)
			}
		}
		resolved, ok := a.resolveModelRefTracked(*modelID, tracked)
		if !ok || resolved == "" {
			return nil, fmt.Errorf("model %s is not available", *modelID)
		}
		if err := us.SetConversationModel(convID, resolved); err != nil {
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
	// The workspace pointer (WORKSPACE LOADING section): the caller
	// validated existence ("" clears). A change away from a workspace
	// evicts that workspace's loaded skills from this conversation's
	// active set (the skill_set column — the switch rule, the user's
	// decision).
	if workspaceID != nil && *workspaceID != cur.WorkspaceID {
		if err := us.SetConversationWorkspace(convID, *workspaceID); err != nil {
			return nil, err
		}
		if cur.WorkspaceID != "" {
			if _, err := a.evictWorkspaceSkills(us, convID, cur.WorkspaceID); err != nil {
				return nil, err
			}
		}
	}
	c, err := us.GetConversation(convID)
	if err != nil {
		return nil, err
	}
	s := a.conversationSummary(*c)
	return &s, nil
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
	// The context bar must count the ACTIVE SKILL SET the same way the
	// live turn does (the block floats at the head of the request's
	// messages).
	skillBlock := a.activeSkillBlock(us, handle, convID)
	hist, err := us.ListMessages(convID)
	if err != nil {
		return nil, err
	}
	prof := a.profileFor(c.ActiveModelID)
	// Estimate EXACTLY the way the live turn does: with the P7 tool-call
	// rows. Passing nil here undercounted by the entire tool transcript —
	// the model's tool calls + their results are often the LARGEST part of
	// the context, which is why the context bar looked "wildly off" (too
	// low) once tool loops existed.
	tcMap, err := toolCallsForMessages(us, hist)
	if err != nil {
		return nil, err
	}
	_, tokens := buildRequest(c, hist, prof, tcMap, skillBlock)
	return map[string]any{"tokens": tokens, "max_tokens": a.modelMaxContext(c.ActiveModelID)}, nil
}

// ToolCallInfo is the display form of one tool call attached to an
// assistant message (the UI renders one indicator row per call).
type ToolCallInfo struct {
	Name    string `json:"name"`
	Success bool   `json:"success"`
}

// ConversationMessage is one displayable message from a conversation. The
// store also keeps bevin-internal rows (summary context artifacts, and tool
// rows once tools exist); those are never rendered as chat, so
// ConversationMessages filters to user + assistant only. Reasoning is the
// assistant's stored thinking trace (empty for user messages and turns
// before reasoning persistence). ToolCalls are the P7 sidecar calls the
// row's turn issued (the UI renders an indicator row per call).
type ConversationMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning,omitempty"`
	ToolCalls []ToolCallInfo `json:"tool_calls,omitempty"`
	CreatedAt string         `json:"created_at"`
	// Seq: the row's conversation sequence (0-based, monotonic). The client's
	// lazy loading uses the oldest loaded seq as the scroll-up cursor (the
	// ?before= value for the next page of older messages).
	Seq int `json:"seq"`
	// CompactedFrom / CompactedTo: on a summary row (a context-compaction
	// event), the prompt size in tokens before / after the compaction (0 = a
	// pre-existing summary row, no tokens recorded). The UI renders a
	// summary row as the persistent "Context compacted from X to Y tokens"
	// marker — never as a message bubble. The row's Content is the compacted
	// context (the model-facing summary), shown in a default-collapsed block.
	CompactedFrom int `json:"compacted_from,omitempty"`
	CompactedTo   int `json:"compacted_to,omitempty"`
}

// displayMessages converts raw store messages into displayable
// ConversationMessages: it attaches the assistant rows' tool calls (P7
// sidecar rows, one query) and surfaces summary rows as compaction markers.
// Shared by the full-history and paged-history paths. An empty assistant row
// WITH calls is the tool-call turn (its text lands on the turn's final reply
// row) — it is INCLUDED so the calls have a place in the flow (the UI renders
// only the indicator rows, no bubble). An empty row WITHOUT calls is a
// genuine empty reply (a reasoning-only completion, or the dropped streamed
// tool call before 7174606): it stays in the history so the user's question
// never looks unanswered, and the UI renders it as "(empty reply)".
func displayMessages(us *store.UserStore, msgs []store.Message) []ConversationMessage {
	var assistantIDs []int64
	for _, m := range msgs {
		if m.Role == roleAssistant {
			assistantIDs = append(assistantIDs, m.ID)
		}
	}
	callsByMsg := map[int64][]ToolCallInfo{}
	if len(assistantIDs) > 0 {
		if rows, err := us.ToolCallsForMessages(assistantIDs); err == nil {
			for id, list := range rows {
				for _, r := range list {
					callsByMsg[id] = append(callsByMsg[id], ToolCallInfo{Name: r.Tool, Success: r.Success})
				}
			}
		}
	}
	out := make([]ConversationMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case roleUser, roleAssistant:
			out = append(out, ConversationMessage{Role: m.Role, Content: m.Content, Reasoning: m.Reasoning, ToolCalls: callsByMsg[m.ID], CreatedAt: m.CreatedAt, Seq: m.Seq})
		case roleSummary:
			// A context-compaction event: the UI renders it as a persistent
			// "Context compacted from X to Y tokens" marker in the flow,
			// with the compacted context (the summary) in a default-collapsed
			// block. The from/to token sizes + the summary text ride along.
			out = append(out, ConversationMessage{Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt, Seq: m.Seq, CompactedFrom: m.CompactedFrom, CompactedTo: m.CompactedTo})
		}
	}
	return out
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
	return displayMessages(us, msgs), nil
}

// ConversationMessagesPaged returns one page of the displayable history (the
// lazy-loading path): the most recent limit messages in seq order, optionally
// restricted to seq < beforeSeq (the scroll-up cursor). hasMore reports
// whether older messages exist — since seq starts at 0, a full page whose
// oldest message is seq 0 is the head of the conversation (no more).
// store.ErrNotFound when the conversation is missing.
func (a *Agent) ConversationMessagesPaged(handle, convID string, limit, beforeSeq int) ([]ConversationMessage, bool, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, false, err
	}
	if _, err := us.GetConversation(convID); err != nil {
		return nil, false, err
	}
	msgs, err := us.ListMessagesPaged(convID, limit, beforeSeq)
	if err != nil {
		return nil, false, err
	}
	out := displayMessages(us, msgs)
	// A full page means older messages may still exist; a short page (fewer
	// than limit displayable rows) means the head of the conversation. (The
	// store counts only displayable rows, so the page size is in visible
	// messages — the seq-0 head check would misfire once check-in rows occupy
	// the low seqs.)
	hasMore := len(msgs) == limit
	return out, hasMore, nil
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

// Every conversation row persisted below goes through the user store's
// AppendMessage, which fires the multi-device nudge (UserStore.AfterAppend,
// wired to NotifyMessage in userStore) — a nudge is owed for ANY persisted
// row, and the store-level hook means no persistence site can miss it (the
// client pulls from the store and reconciles, rendering whatever roles it
// renders).
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
func (a *Agent) runTurn(ctx context.Context, handle, convID, userText string, images []TurnImage, emit func(provider.Chunk) error) (string, string, error) {
	// The conversation id rides the ctx for the memory tools'
	// per-conversation state (remember's provenance) — harness plumbing,
	// see memorytools.go.
	ctx = context.WithValue(ctx, ctxConversationKey, convID)
	// The turn's attached images ride the ctx for the image_analyze
	// executor (same harness plumbing; see imagetools.go).
	if len(images) > 0 {
		ctx = context.WithValue(ctx, ctxTurnImagesKey, images)
	}
	us, err := a.userStore(handle)
	if err != nil {
		return "", "", err
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		return "", "", err
	}
	// The ACTIVE SKILL SET's block floats at the head of this request's
	// message list (the SKILL SYSTEM section's re-injection — see
	// activeSkillBlock for the placement: after the compaction message,
	// never the prefix-cached system prompt).
	skillBlock := a.activeSkillBlock(us, handle, convID)
	p, ok := a.providerFor(conv.ActiveModelID)
	if !ok {
		return conv.ID, "", fmt.Errorf("no enabled provider for active model %q", conv.ActiveModelID)
	}
	// Persist the user message first, so history is always durable/consistent.
	// An image-only message (no text) persists a marker: the transcript
	// carries the fact of the attachment — the image bytes themselves are
	// never persisted (the image_analyze tool result is the record).
	storedText := userText
	if strings.TrimSpace(storedText) == "" && len(images) > 0 {
		storedText = attachedImagesMarker(len(images))
	}
	userMsg, err := us.AppendMessage(convID, roleUser, storedText, "", "", nil)
	if err != nil {
		return conv.ID, "", err
	}
	a.indexMessage(ctx, handle, userMsg, conv.Title)
	// Turn-start awareness (concurrent-instances, layers 2+4): this
	// conversation is in-flight for the turn's duration (its siblings'
	// rosters show it as running), and the roster delta + queued sibling
	// messages + purpose nudge are announced as ONE UI-hidden checkin row
	// when any of them is new (see announceTurnStart).
	a.enterInflight(handle, convID)
	defer a.leaveInflight(handle, convID)
	if err := a.announceTurnStart(us, handle, convID, conv); err != nil {
		return conv.ID, "", err
	}
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
	// filtered out while disabled/unconfigured, advance_task out of
	// non-task conversations, and web_browser carries the matching search
	// note (see wireTools).
	wireTools := a.wireTools(us, conv)
	var res *provider.Result
	// lastUsage: the API-reported TotalTokens of the most recent model call
	// in this turn (0 until the backend reports usage). It is a lower bound
	// on the next call's prompt (the conversation history only grows within
	// a turn) and — unlike the char/4 estimate — a true measurement of the
	// context the model actually consumed. It feeds the in-loop compaction
	// trigger (below) so a long tool loop compacts on REAL prompt size, in
	// time, instead of on an estimate that undercounts dense content.
	lastUsage := 0
	// compactRetried: the overflow backstop (below) force-compacts + retries
	// a context-overflow call AT MOST ONCE per turn — no retry loops.
	compactRetried := false
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
		req, tokens := buildRequest(conv, hist, prof, tcMap, skillBlock)
		req.Tools = tools
		// Pre-compact trigger: the LARGER of the char/4 estimate and the
		// previous model call's real API-reported usage (lastUsage). The
		// estimate undercounts dense content — web pages, JSON tool
		// transcripts and replayed reasoning traces tokenize closer to
		// ~3 chars/token than the 4-char rule, so on a long tool loop it can
		// sit well below the real prompt size while the real context runs
		// out; lastUsage is the only true measurement, and it is a lower
		// bound on this call's prompt (history only grows within a turn), so
		// max() never under-triggers. When the backend reports no usage,
		// lastUsage is 0 and the estimate alone decides (the pre-usage
		// fallback).
		trigger := tokens
		if lastUsage > trigger {
			trigger = lastUsage
		}
		if trigger > summarizeBudget(a.modelMaxContext(conv.ActiveModelID), prof) {
			// Pre-compact: the prompt is over budget before the call. Fold the
			// older turns (incl. this message) into a summary so the call fits.
			if cerr := a.compactIfNeeded(ctx, us, handle, conv, p, trigger, prof, false, emit); cerr != nil {
				return conv.ID, "", cerr
			}
			// The prompt just shrank (or there was nothing to fold) — the
			// previous call's usage is no longer a bound on it; the next
			// iteration's fresh estimate + usage take over.
			lastUsage = 0
			hist, err = us.ListMessages(convID)
			if err != nil {
				return conv.ID, "", err
			}
			tcMap, err = toolCallsForMessages(us, hist)
			if err != nil {
				return conv.ID, "", err
			}
			req, _ = buildRequest(conv, hist, prof, tcMap, skillBlock)
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
			if isContextOverflow(err) && !compactRetried {
				// Overflow backstop: the real prompt exceeded the model's
				// context. The trigger above should have compacted first (it
				// runs on the previous call's real usage), but a single large
				// tool result can push the prompt past the context in one
				// step, ahead of both the estimate and the recorded usage.
				// Force-compact the older turns and retry the call once by
				// re-entering the loop — it re-reads the now-shorter history,
				// rebuilds the request, and calls the model again. One retry
				// per turn: if the compaction fails or the retry overflows
				// too, the error surfaces.
				compactRetried = true
				lastUsage = 0
				if cerr := a.compactIfNeeded(ctx, us, handle, conv, p, 0, prof, true, emit); cerr != nil {
					return conv.ID, "", err
				}
				continue
			}
			return conv.ID, "", err
		}
		// A successful call proves the model is available again — re-enable it
		// if it had been auto-disabled.
		if a.isAutoDisabled(conv.ActiveModelID) {
			a.SetModelAutoDisabled(conv.ActiveModelID, false)
		}
		// Record the real usage for the next iteration's trigger (see
		// lastUsage above). Backends that omit usage leave it at its current
		// (stale-but-lower-bound) value — still safe, since the estimate is
		// always maxed in as well.
		if res.Usage != nil {
			lastUsage = res.Usage.TotalTokens
		}
		// Bevin-side KV-cache backend tracking (2026-11-27): a completed
		// model call on a llama backend means the conversation's KV cache
		// file belongs to that backend (the always-save rule just
		// wrote/updated it there). Record the holder durably so model
		// resolution can route the conversation back to it (cache locality
		// beats least-loaded). Best-effort: a tracking write failure never
		// affects the turn.
		if kb := a.kvBackendFor(conv.ActiveModelID); kb != "" {
			if cerr := us.SetConversationKVBackend(convID, kb); cerr != nil {
				log.Printf("bevin: kv-cache backend tracking %s: %v", convID, cerr)
			}
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
				if emit != nil {
					if err := emit(provider.Chunk{ToolName: tc.Name}); err != nil {
						return conv.ID, "", err
					}
				}
				p7id, err := us.AppendToolCall(assistantMsg.ID, tc.Name, tc.Arguments)
				if err != nil {
					return conv.ID, "", err
				}
				if cerr := us.CompleteToolCall(p7id, "not executed: the turn was stopped (no observed progress — the no-progress confidence reached its threshold)", false); cerr != nil {
					return conv.ID, "", cerr
				}
				if emit != nil {
					if err := emit(provider.Chunk{ToolName: tc.Name, ToolDone: true, ToolSuccess: false}); err != nil {
						return conv.ID, "", err
					}
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
			// The UI sees each issued call as an indicator row in the
			// chat flow (one row per call; the next assistant reply
			// renders below the rows). The cap-excess calls get a row
			// too — the model DID issue them.
			if emit != nil {
				if err := emit(provider.Chunk{ToolName: tc.Name}); err != nil {
					return conv.ID, "", err
				}
			}
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
			if emit != nil {
				if err := emit(provider.Chunk{ToolName: tc.Name, ToolDone: true, ToolSuccess: ok}); err != nil {
					return conv.ID, "", err
				}
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
	// budget, compact the older turns so the next call fits. Use the LARGER
	// of the char/4 estimate (of the now-persisted reply) and the real
	// API-reported usage (res.Usage — from the final SSE chunk when
	// streaming; the next call's prompt is ~this call's total). The max keeps
	// the trigger from under-firing on either an undercounting estimate or a
	// backend that omits usage — same rule as the in-loop trigger.
	compHist, herr := us.ListMessages(convID)
	postTokens := 0
	if herr == nil {
		compTC, terr := toolCallsForMessages(us, compHist)
		if terr == nil {
			// The skill block rides the next turn's request too — count it
			// or the post-compaction check undercounts.
			_, postTokens = buildRequest(conv, compHist, prof, compTC, a.activeSkillBlock(us, handle, conv.ID))
		}
	}
	actualTokens := postTokens
	if res.Usage != nil && res.Usage.TotalTokens > actualTokens {
		actualTokens = res.Usage.TotalTokens
	}
	if cerr := a.compactIfNeeded(ctx, us, handle, conv, p, actualTokens, prof, false, emit); cerr != nil {
		log.Printf("compact after turn: %v", cerr)
	}
	return conv.ID, res.Text, nil
}

// modelSlots returns a model's current execution-slot count (min 1) from live
// config — the scheduler consults it on every dispatch, so the admin's slot
// edits apply without a restart. It resolves BOTH pool kinds: a main model's
// own id and an aux model's pool id (its own derived pool, or the main model
// it exactly matches — the shared pool, so perception requests and that
// model's conversations draw on the same slot count). A pool removed from the
// config while a job is queued gets 1, so its queued jobs complete quickly
// (with an error) instead of waiting on a pool that no longer exists.
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
	for _, x := range a.cfg.Aux {
		if x.PoolID == modelID {
			if x.Slots < 1 {
				return 1
			}
			return x.Slots
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

// PoolStatus is one pool as seen by the admin dashboard: the slot count +
// how many are busy + the turns waiting for a slot. Every pool gets a row:
// each main model's pool, and each aux model's OWN pool (Aux=true) — a
// shared aux adds no row (it runs in the main model's pool); that main row
// then names the aux models that share it (Auxes).
type PoolStatus struct {
	ID           string      `json:"id"`
	DisplayName  string      `json:"display_name"`
	Provider     string      `json:"provider"`
	Slots        int         `json:"slots"`
	Busy         int         `json:"busy"`
	Enabled      bool        `json:"enabled"`
	AutoDisabled bool        `json:"auto_disabled"`
	Aux          bool        `json:"aux"`
	Auxes        []string    `json:"auxes,omitempty"` // main pool: the aux models sharing it (exact match)
	Waiting      []QueuedJob `json:"waiting"`
}

// Pools returns the live state of every configured pool (for the admin
// dashboard): the main models' execution-slot pools + each aux model's own
// pool. A shared aux (an exact match of a main model) is folded into that
// main model's row (Auxes) — its perception requests and the model's
// conversations draw on the same slots, so it is one pool, not two.
func (a *Agent) Pools() []PoolStatus {
	load := a.sched.Load()
	a.cfgMu.Lock()
	models := make([]config.Model, len(a.cfg.Models))
	copy(models, a.cfg.Models)
	aux := make([]config.AuxModel, len(a.cfg.Aux))
	copy(aux, a.cfg.Aux)
	a.cfgMu.Unlock()
	mainIDs := make(map[string]bool, len(models))
	for _, m := range models {
		mainIDs[m.ID] = true
	}
	// Split the aux models: those sharing a main model's pool (PoolID is a
	// main id) vs. those with their own.
	sharedBy := map[string][]string{}
	var own []config.AuxModel
	for _, x := range aux {
		if x.PoolID == "" {
			continue
		}
		if mainIDs[x.PoolID] {
			sharedBy[x.PoolID] = append(sharedBy[x.PoolID], x.ModelID)
		} else {
			own = append(own, x)
		}
	}
	out := []PoolStatus{}
	for _, m := range models {
		slots := m.Slots
		if slots < 1 {
			slots = 1
		}
		wait := []QueuedJob{}
		busy := 0
		if l, ok := load[m.ID]; ok {
			wait = l.Waiting
			if wait == nil {
				wait = []QueuedJob{}
			}
			busy = l.Busy
		}
		out = append(out, PoolStatus{ID: m.ID, DisplayName: m.DisplayName, Provider: m.Provider, Slots: slots, Busy: busy, Enabled: m.Enabled, AutoDisabled: m.AutoDisabled, Auxes: sharedBy[m.ID], Waiting: wait})
	}
	for _, x := range own {
		slots := x.Slots
		if slots < 1 {
			slots = 1
		}
		wait := []QueuedJob{}
		busy := 0
		if l, ok := load[x.PoolID]; ok {
			wait = l.Waiting
			if wait == nil {
				wait = []QueuedJob{}
			}
			busy = l.Busy
		}
		out = append(out, PoolStatus{ID: x.PoolID, DisplayName: x.ModelID, Provider: x.Provider, Slots: slots, Busy: busy, Enabled: true, Aux: true, Waiting: wait})
	}
	return out
}

// Turn runs one chat turn (non-streaming) through the per-model slot pool:
// it queues on the conversation's active model and holds one slot for the
// duration of the turn. images are the user's attached images for the turn
// (the image_analyze tool's input; nil = a text-only message). See runTurn
// for the full flow.
func (a *Agent) Turn(ctx context.Context, handle, convID, userText string, images []TurnImage) (string, string, error) {
	return a.runScheduled(ctx, handle, convID, userText, images, nil)
}

// TurnStream runs one chat turn (streaming) through the per-model slot pool,
// emitting each delta via emit once the turn holds a slot and the model call
// starts. images are the user's attached images for the turn (nil = a
// text-only message). See runTurn for the full flow.
func (a *Agent) TurnStream(ctx context.Context, handle, convID, userText string, images []TurnImage, emit func(provider.Chunk) error) (string, string, error) {
	return a.runScheduled(ctx, handle, convID, userText, images, emit)
}

// runScheduled resolves the conversation (so the scheduler knows which model
// pool to queue on) and runs the turn under that pool's slot limit. A
// conversation that no longer exists fails before anything is queued; a
// canceled context drops the job out of the queue (the user message is not
// persisted — the turn never ran).
func (a *Agent) runScheduled(ctx context.Context, handle, convID, userText string, images []TurnImage, emit func(provider.Chunk) error) (string, string, error) {
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
		_, reply, retErr = a.runTurn(jobCtx, handle, convID, userText, images, emit)
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
