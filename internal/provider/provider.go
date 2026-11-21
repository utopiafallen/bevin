// Package provider isolates the wire details of each model API (OpenAI,
// Anthropic Messages, llama.cpp) behind a single Provider interface, so the
// rest of bevin never special-cases a vendor. Each implementation maps the
// neutral types defined here to its own request/response shape.
package provider

import (
	"context"
	"net/http"
	"time"
)

// Role is a provider-neutral message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// MediaKind is a kind of non-text attachment.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaAudio MediaKind = "audio"
	MediaVideo MediaKind = "video"
)

// Media is a provider-neutral media attachment. Set exactly one of Data
// (inline bytes) or URL (a data:/file:// or http(s) reference).
type Media struct {
	Kind     MediaKind
	MimeType string // e.g. "image/png", "video/mp4"; may be empty
	Data     []byte
	URL      string
}

// ContentPart is one piece of a message body: text or media.
type ContentPart struct {
	Text  string
	Media *Media
}

// ToolCall is a tool invocation requested by the model (assistant) or reported
// as completed.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON-encoded arguments
}

// ToolCallDelta is one streamed tool-call fragment (the OAI wire's
// delta.tool_calls entries). A call's id + name ride on its first fragment;
// Arguments is THIS fragment's piece of the JSON arguments (the full
// arguments usually arrive across many fragments). The provider accumulates
// the fragments per call index and exposes the complete calls on
// Result.ToolCalls; the raw fragments are here for consumers that want live
// tool-call progress (the Anthropic wire has no equivalent — its stream
// carries whole tool_use blocks, accumulated the same way on Result).
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Tool describes a callable tool the model may invoke.
type Tool struct {
	Name        string
	Description string
	Parameters  any // JSON Schema for the tool's arguments
}

// Message is a provider-neutral chat message.
type Message struct {
	Role               Role
	Text               string        // plain text; for RoleTool this is the tool result
	Parts              []ContentPart // structured text/media; used when non-empty
	Reasoning          string        // RoleAssistant: the model's thinking trace for this turn, replayed in the API's native field when non-empty (OAI wire: reasoning_content; Anthropic: a thinking content block, which additionally needs the signature); empty when the model didn't think (or thinking is disabled)
	ReasoningSignature string        // RoleAssistant: the per-turn signature of the trace (Anthropic extended thinking — replay is invalid without it); empty for APIs without signatures
	ToolCalls          []ToolCall    // RoleAssistant: tool calls the model requested
	ToolCallID         string        // RoleTool: which tool call this result answers
}

// Usage reports token accounting.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatRequest is a provider-neutral completion request.
type ChatRequest struct {
	System      []ContentPart
	Messages    []Message
	Tools       []Tool
	Temperature *float64
	MaxTokens   *int
	// ReasoningEffort: the requested reasoning effort (one of the active
	// model's profile levels — profile.Profile.ReasoningEfforts, e.g.
	// "low"/"medium"/"xhigh"; set by the request builder only when the
	// profile supports the conversation's stored choice). The provider
	// adapts it to its API's native thinking control (the OAI wire's
	// reasoning_effort) or drops it when its API has none (Anthropic).
	// Empty = the model's own default.
	ReasoningEffort string
	// ConversationID: the bevin-level conversation the completion belongs
	// to (the store's 24-hex id — globally unique across users). Carried
	// verbatim into the OAI wire's `conversation_id` field, where a
	// llama.cpp proxy in front of the model can use it to save/load the
	// slot's KV cache per conversation (turn N's prompt is the prefix of
	// turn N+1's, so the cached KV is directly reusable). Set by the
	// request builder for the conversation's completions only — internal
	// meta-calls (the summarization call) leave it empty because their
	// prompt is NOT a prefix of the conversation's prompt (a different
	// system prompt): loading or saving KV under the conversation's key
	// there would clobber the reusable cache or load a mismatched one.
	// Providers whose API has no such concept (Anthropic) drop it.
	ConversationID string
}

// Result is the aggregated result of a completion (streaming or not).
type Result struct {
	Text               string
	Reasoning          string
	ReasoningSignature string // Anthropic: the thinking block's per-turn signature (required to replay the trace); empty for APIs without signatures
	ToolCalls          []ToolCall
	StopReason         string
	Usage              *Usage
}

// Chunk is a streaming delta.
type Chunk struct {
	TextDelta          string          // incremental text
	ReasoningDelta     string          // incremental reasoning trace
	ReasoningSignature string          // Anthropic: per-turn signature when the thinking block finalizes
	ToolCallDeltas     []ToolCallDelta // streamed tool-call fragments (OAI wire; empty when the chunk carries none)
	StopReason         string          // stop_reason from message_delta
	Usage              *Usage
	Done               bool

	// Harness-level tool-call indicators (the backends never set these;
	// the agent's turn loop emits them so the UI can render a row per
	// issued tool call — the model's next reply lands below the rows).
	// ToolName non-empty = an indicator: ToolDone false = the call was
	// issued, true = it finished (ToolSuccess carries the outcome).
	ToolName    string
	ToolDone    bool
	ToolSuccess bool
}

// ModelInfo describes one model a backend serves, incl. its modalities.
type ModelInfo struct {
	ID               string
	InputModalities  []string
	OutputModalities []string
	MaxContext       int
}

// Provider is implemented by the OpenAI, Anthropic, and llama.cpp backends.
// The model id is passed per call so a single endpoint (which may serve several
// models) can be reused.
type Provider interface {
	// Name returns a short identifier (e.g. "openai", "anthropic", "llama").
	Name() string
	// Chat performs a non-streaming completion with the given model.
	Chat(ctx context.Context, model string, req ChatRequest) (*Result, error)
	// Stream performs a streaming completion, calling emit for each delta, and
	// returns the aggregated Result.
	Stream(ctx context.Context, model string, req ChatRequest, emit func(Chunk) error) (*Result, error)
	// Models lists the models the backend serves (with modalities where known).
	Models(ctx context.Context) ([]ModelInfo, error)
	// Embed returns one embedding per input (multimodal where the backend
	// supports it). Not all backends support embeddings (e.g. Anthropic).
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
}

// defaultHTTP returns the shared http.Client used by providers.
func defaultHTTP() *http.Client { return &http.Client{Timeout: 10 * time.Minute} }
