package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	anthropicVersion          = "2023-06-01"
	anthropicDefaultMaxTokens = 4096
)

// Anthropic is a provider for the Anthropic Messages API (/v1/messages). Its
// wire differs materially from OpenAI: system is a top-level field (not a
// message role), max_tokens is required, auth is via x-api-key, and content is
// a block list (text / image / tool_use / tool_result). It has no embeddings
// endpoint — the bevin embedder is always the local llama-server.
type Anthropic struct {
	BaseURL string // e.g. https://api.anthropic.com
	APIKey  string
	HTTP    *http.Client
}

func NewAnthropic(baseURL, apiKey string) *Anthropic {
	return &Anthropic{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: defaultHTTP()}
}

func (a *Anthropic) Name() string { return "anthropic" }

// ---- wire types ----

type antSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type antContentBlock struct {
	Type      string          `json:"type"` // text | image | tool_use | tool_result
	Text      string          `json:"text,omitempty"`
	Source    *antSource      `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type antMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type antTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type antRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	System      string       `json:"system,omitempty"`
	Messages    []antMessage `json:"messages"`
	Tools       []antTool    `json:"tools,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
}

type antOutBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking,omitempty"`  // type=thinking: the model's trace
	Signature string          `json:"signature,omitempty"` // type=thinking: per-turn signature (replay needs it; see decodeAntResponse)
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type antUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type antResponse struct {
	Content    []antOutBlock `json:"content"`
	StopReason string        `json:"stop_reason"`
	Usage      *antUsage     `json:"usage"`
}

type antDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"` // type=thinking_delta
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

type antStreamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	ContentBlock antContentBlock `json:"content_block"`
	Delta        antDelta        `json:"delta"`
	Usage        *antUsage       `json:"usage"`
	Message      struct {
		Usage *antUsage `json:"usage"`
	} `json:"message"`
}

// ---- helpers ----

func (a *Anthropic) header(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

func (a *Anthropic) doJSON(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s: %s", a.Name(), resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func encodeAntContent(m Message) ([]antContentBlock, error) {
	// A tool result is a single tool_result block whose content is m.Text.
	if m.Role == RoleTool {
		return []antContentBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Text}}, nil
	}
	var blocks []antContentBlock
	if len(m.Parts) == 0 && m.Text != "" {
		blocks = append(blocks, antContentBlock{Type: "text", Text: m.Text})
	}
	for _, p := range m.Parts {
		if p.Media != nil {
			b, err := encodeAntMedia(p.Media)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, b)
		} else if p.Text != "" {
			blocks = append(blocks, antContentBlock{Type: "text", Text: p.Text})
		}
	}
	for _, tc := range m.ToolCalls {
		in := strings.TrimSpace(tc.Arguments)
		if in == "" {
			in = "{}"
		}
		blocks = append(blocks, antContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: json.RawMessage(in)})
	}
	if len(blocks) == 0 {
		blocks = []antContentBlock{{Type: "text", Text: ""}}
	}
	return blocks, nil
}

func encodeAntMedia(m *Media) (antContentBlock, error) {
	if m.Kind != MediaImage {
		return antContentBlock{}, fmt.Errorf("anthropic provider only supports image media (got %s)", m.Kind)
	}
	b := antContentBlock{Type: "image"}
	if m.URL != "" {
		b.Source = &antSource{Type: "url", URL: m.URL}
	} else {
		mt := m.MimeType
		if mt == "" {
			mt = defaultMime(m.Kind)
		}
		b.Source = &antSource{Type: "base64", MediaType: mt, Data: b64encode(m.Data)}
	}
	return b, nil
}

func buildAntRequest(req ChatRequest, model string, stream bool) (antRequest, error) {
	maxTok := anthropicDefaultMaxTokens
	if req.MaxTokens != nil {
		maxTok = *req.MaxTokens
	}
	areq := antRequest{Model: model, MaxTokens: maxTok, Temperature: req.Temperature, Stream: stream}
	var sys []string
	for _, sp := range req.System {
		sys = append(sys, sp.Text)
	}
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			if m.Text != "" {
				sys = append(sys, m.Text)
			}
			for _, p := range m.Parts {
				sys = append(sys, p.Text)
			}
			continue
		}
		blocks, err := encodeAntContent(m)
		if err != nil {
			return areq, err
		}
		role := string(m.Role)
		if m.Role == RoleTool {
			role = "user" // Anthropic has no tool role; tool results are user messages
		}
		areq.Messages = append(areq.Messages, antMessage{Role: role, Content: blocks})
	}
	if len(sys) > 0 {
		areq.System = strings.Join(sys, "\n")
	}
	for _, t := range req.Tools {
		schema := t.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		areq.Tools = append(areq.Tools, antTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return areq, nil
}

// decodeAntResponse parses an Anthropic response into a Result. Thinking
// blocks (extended thinking) are accumulated into Result.Reasoning — their
// text is the model's trace, persisted + shown in the UI like any other
// reasoning. Replay is deliberately NOT wired on the Anthropic encoder
// (encodeAntContent ignores Message.Reasoning): the API requires each
// thinking block back VERBATIM with its per-turn signature (which we do not
// persist), and a thinking block without a valid signature is rejected — so
// until signature capture lands, Anthropic turns simply don't replay the
// trace rather than send an invalid one.
func decodeAntResponse(ar antResponse) *Result {
	res := &Result{StopReason: ar.StopReason}
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			res.Text += b.Text
		case "thinking":
			res.Reasoning += b.Thinking
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			res.ToolCalls = append(res.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	if ar.Usage != nil {
		res.Usage = &Usage{PromptTokens: ar.Usage.InputTokens, CompletionTokens: ar.Usage.OutputTokens, TotalTokens: ar.Usage.InputTokens + ar.Usage.OutputTokens}
	}
	return res
}

// ---- Provider methods ----

func (a *Anthropic) Chat(ctx context.Context, model string, req ChatRequest) (*Result, error) {
	areq, err := buildAntRequest(req, model, false)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(areq)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	a.header(r)
	resp, err := a.doJSON(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ar antResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, err
	}
	return decodeAntResponse(ar), nil
}

func (a *Anthropic) Stream(ctx context.Context, model string, req ChatRequest, emit func(Chunk) error) (*Result, error) {
	areq, err := buildAntRequest(req, model, true)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(areq)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	a.header(r)
	r.Header.Set("Accept", "text/event-stream")
	resp, err := a.doJSON(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var res Result
	type openTool struct{ id, name, jsonBuf string }
	openTools := map[int]*openTool{}
	var inputTokens int

	scanner := newLineScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev antStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		var c Chunk
		switch ev.Type {
		case "message_start":
			if ev.Message.Usage != nil {
				inputTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				openTools[ev.Index] = &openTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				res.Text += ev.Delta.Text
				c.TextDelta = ev.Delta.Text
			case "thinking_delta":
				res.Reasoning += ev.Delta.Thinking
				c.ReasoningDelta = ev.Delta.Thinking
			case "input_json_delta":
				if ot, ok := openTools[ev.Index]; ok {
					ot.jsonBuf += ev.Delta.PartialJSON
				}
			}
		case "content_block_stop":
			if ot, ok := openTools[ev.Index]; ok {
				args := strings.TrimSpace(ot.jsonBuf)
				if args == "" {
					args = "{}"
				}
				res.ToolCalls = append(res.ToolCalls, ToolCall{ID: ot.id, Name: ot.name, Arguments: args})
				delete(openTools, ev.Index)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				res.StopReason = ev.Delta.StopReason
				c.StopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				res.Usage = &Usage{PromptTokens: inputTokens, CompletionTokens: ev.Usage.OutputTokens, TotalTokens: inputTokens + ev.Usage.OutputTokens}
			}
		}
		if c.TextDelta != "" || c.ReasoningDelta != "" || c.StopReason != "" {
			if emit != nil {
				if err := emit(c); err != nil {
					return &res, err
				}
			}
		}
	}
	return &res, scanner.Err()
}

// Models lists Anthropic's static model catalog (no per-model modalities).
func (a *Anthropic) Models(ctx context.Context) ([]ModelInfo, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	a.header(r)
	resp, err := a.doJSON(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ml struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ml); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(ml.Data))
	for _, m := range ml.Data {
		out = append(out, ModelInfo{ID: m.ID})
	}
	return out, nil
}

// Embed is not supported by Anthropic; the bevin embedder is always the
// local llama-server. Present to satisfy the Provider interface.
func (a *Anthropic) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	return nil, fmt.Errorf("%s: embeddings are not supported (use the local llama-server embedder)", a.Name())
}
