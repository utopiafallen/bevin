package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/utopiafallen/bevin/internal/provider"
)

// maxToolIterations caps the tool loop: a turn makes at most this many
// model→tool round trips before the loop forces a plain-text answer (one
// final call with NO tools on the wire). A model that tool-loops on a
// broken page or a flapping tool must not hold a model slot forever — the
// cap bounds the damage and the final answer gets one clean shot to say what
// happened. If the model STILL returns tool calls on the no-tools call
// (runaway), the turn ends there: the calls are recorded as failed P7 rows
// (wire-valid complete pairs) and are never executed.
const maxToolIterations = 20

// maxToolCallsPerMessage caps the calls executed from ONE assistant message
// (a single message with hundreds of calls would burn the turn's budget in
// one shot). Excess calls are recorded as failed rows like the iteration
// cap — executed work is bounded on both axes.
const maxToolCallsPerMessage = 10

// Tool is one callable capability the model may invoke (the plan's WS1
// registry entry): a name + description + JSON schema for the model, and an
// executor for the harness.
type Tool struct {
	Name        string
	Description string
	Parameters  any // JSON Schema for the tool's arguments
	// Execute runs one call. handle is the user whose turn is running (the
	// tool's per-user resources — e.g. their dedicated browser — key on it);
	// args is the model's RAW JSON arguments (the provider hands them over as
	// an opaque string; the executor unmarshals into its own shape and
	// reports a structured error on malformed input — a bad argument is a
	// tool result back to the model, never a crash). The returned string is
	// the tool result the model sees (on success AND on failure — the
	// failure text is the model's self-healing signal).
	Execute func(ctx context.Context, handle string, args json.RawMessage) (string, error)
}

// ToolRegistry is the name→tool table for the user agent. The registry is
// shared across users (the executors resolve per-user state from the
// handle), and a turn's wire request carries the whole registry — the
// current-registry dispatch invariant: an unknown tool name (including a
// model imitating a removed/renamed tool from its own history) gets a
// structured error naming the available tools, never a silent success.
type ToolRegistry struct {
	mu     sync.RWMutex
	list   []*Tool
	byName map[string]*Tool
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{byName: map[string]*Tool{}}
}

// Register adds a tool; a duplicate name is a wiring bug and fails loudly
// (two tools with the same name would make the model's choice ambiguous).
func (r *ToolRegistry) Register(t Tool) error {
	if t.Name == "" || t.Execute == nil {
		return fmt.Errorf("tool: name and Execute are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[t.Name]; ok {
		return fmt.Errorf("tool %q already registered", t.Name)
	}
	tt := t
	r.list = append(r.list, &tt)
	r.byName[t.Name] = &tt
	return nil
}

// Get resolves a tool by name (the dispatch path).
func (r *ToolRegistry) Get(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// Wire returns the registry as the provider wire's tool list (registration
// order — stable for the prompt).
func (r *ToolRegistry) Wire() []provider.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Tool, 0, len(r.list))
	for _, t := range r.list {
		out = append(out, provider.Tool{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

// Names returns the tool names (sorted — the unknown-tool error lists them).
func (r *ToolRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.list))
	for _, t := range r.list {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Len reports how many tools are registered (0 = the wire carries no tools
// — exactly today's no-tool behavior).
func (r *ToolRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.list)
}
