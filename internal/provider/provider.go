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

// Tool describes a callable tool the model may invoke.
type Tool struct {
	Name        string
	Description string
	Parameters  any // JSON Schema for the tool's arguments
}

// Message is a provider-neutral chat message.
type Message struct {
	Role       Role
	Text       string        // plain text; for RoleTool this is the tool result
	Parts      []ContentPart // structured text/media; used when non-empty
	ToolCalls  []ToolCall    // RoleAssistant: tool calls the model requested
	ToolCallID string        // RoleTool: which tool call this result answers
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
}

// Result is the aggregated result of a completion (streaming or not).
type Result struct {
	Text       string
	Reasoning  string
	ToolCalls  []ToolCall
	StopReason string
	Usage      *Usage
}

// Chunk is a streaming delta.
type Chunk struct {
	TextDelta      string
	ReasoningDelta string
	StopReason     string
	Usage          *Usage
	Done           bool
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
