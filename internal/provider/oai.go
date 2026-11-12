package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// ---- OpenAI-compatible wire types (shared by OpenAI and llama.cpp) ----

type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiInputAudio struct {
	Data   string `json:"data,omitempty"`
	URL    string `json:"url,omitempty"`
	Format string `json:"format,omitempty"`
}

type oaiMediaRef struct {
	Data string `json:"data,omitempty"`
	URL  string `json:"url,omitempty"`
}

type oaiContentPart struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	ImageURL   *oaiImageURL   `json:"image_url,omitempty"`
	InputAudio *oaiInputAudio `json:"input_audio,omitempty"`
	InputVideo *oaiMediaRef   `json:"input_video,omitempty"`
	VideoURL   *oaiMediaRef   `json:"video_url,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiChatRequest struct {
	Model         string           `json:"model"`
	Messages      []oaiMessage     `json:"messages"`
	Tools         []oaiTool        `json:"tools,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	MaxTokens     *int             `json:"max_tokens,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiChatResponse struct {
	Choices []struct {
		Message struct {
			Role             string        `json:"role"`
			Content          string        `json:"content"`
			ReasoningContent string        `json:"reasoning_content"`
			ToolCalls        []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
}

// mediaURL returns a URL for a Media (using the URL directly, or a data: URL
// for inline bytes).
func mediaURL(m *Media) (string, error) {
	if m == nil {
		return "", fmt.Errorf("nil media")
	}
	if m.URL != "" {
		return m.URL, nil
	}
	if len(m.Data) > 0 {
		mt := m.MimeType
		if mt == "" {
			mt = defaultMime(m.Kind)
		}
		return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(m.Data), nil
	}
	return "", fmt.Errorf("media has neither Data nor URL")
}

func defaultMime(k MediaKind) string {
	switch k {
	case MediaImage:
		return "image/png"
	case MediaAudio:
		return "audio/mpeg"
	case MediaVideo:
		return "video/mp4"
	}
	return "application/octet-stream"
}

func mimeExt(m *Media) string {
	if m.MimeType == "" {
		return ""
	}
	switch m.MimeType {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav":
		return "wav"
	case "audio/flac":
		return "flac"
	}
	return m.MimeType
}

// encodeOAIMessage maps a neutral message to an OAI message. videoOK enables
// video content parts (llama.cpp supports them; OpenAI does not).
func encodeOAIMessage(m Message, videoOK bool) (oaiMessage, error) {
	om := oaiMessage{Role: string(m.Role)}
	if len(m.ToolCalls) > 0 {
		om.ToolCalls = encodeOAIToolCalls(m.ToolCalls)
	}
	if m.Role == RoleTool {
		om.ToolCallID = m.ToolCallID
	}

	parts := m.Parts
	if len(parts) == 0 && m.Text != "" {
		parts = []ContentPart{{Text: m.Text}}
	}

	if len(parts) == 0 {
		om.Content = json.RawMessage(`""`)
		return om, nil
	}

	// A pure-text message serializes as a plain string.
	allText := true
	for _, p := range parts {
		if p.Media != nil {
			allText = false
			break
		}
	}
	if allText {
		var sb []byte
		for i, p := range parts {
			if i > 0 {
				sb = append(sb, '\n')
			}
			sb = append(sb, p.Text...)
		}
		b, _ := json.Marshal(string(sb))
		om.Content = b
		return om, nil
	}

	arr := make([]oaiContentPart, 0, len(parts))
	for _, p := range parts {
		if p.Media != nil {
			cp, err := encodeOAIMedia(p.Media, videoOK)
			if err != nil {
				return om, err
			}
			arr = append(arr, cp)
			continue
		}
		if p.Text != "" {
			arr = append(arr, oaiContentPart{Type: "text", Text: p.Text})
		}
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return om, err
	}
	om.Content = b
	return om, nil
}

func encodeOAIMedia(m *Media, videoOK bool) (oaiContentPart, error) {
	switch m.Kind {
	case MediaImage:
		u, err := mediaURL(m)
		if err != nil {
			return oaiContentPart{}, err
		}
		return oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: u}}, nil
	case MediaAudio:
		if m.URL != "" {
			return oaiContentPart{Type: "input_audio", InputAudio: &oaiInputAudio{URL: m.URL, Format: mimeExt(m)}}, nil
		}
		return oaiContentPart{Type: "input_audio", InputAudio: &oaiInputAudio{
			Data:   base64.StdEncoding.EncodeToString(m.Data),
			Format: mimeExt(m),
		}}, nil
	case MediaVideo:
		if !videoOK {
			return oaiContentPart{}, fmt.Errorf("video input is not supported by this provider")
		}
		if m.URL != "" {
			return oaiContentPart{Type: "video_url", VideoURL: &oaiMediaRef{URL: m.URL}}, nil
		}
		return oaiContentPart{Type: "input_video", InputVideo: &oaiMediaRef{
			Data: base64.StdEncoding.EncodeToString(m.Data),
		}}, nil
	}
	return oaiContentPart{}, fmt.Errorf("unsupported media kind %q", m.Kind)
}

func encodeOAIToolCalls(tcs []ToolCall) []oaiToolCall {
	out := make([]oaiToolCall, 0, len(tcs))
	for _, tc := range tcs {
		var o oaiToolCall
		o.ID = tc.ID
		o.Type = "function"
		o.Function.Name = tc.Name
		o.Function.Arguments = tc.Arguments
		out = append(out, o)
	}
	return out
}

func encodeOAITools(tools []Tool) []oaiTool {
	out := make([]oaiTool, 0, len(tools))
	for _, t := range tools {
		var o oaiTool
		o.Type = "function"
		o.Function.Name = t.Name
		o.Function.Description = t.Description
		o.Function.Parameters = t.Parameters
		if o.Function.Parameters == nil {
			o.Function.Parameters = map[string]any{"type": "object"}
		}
		out = append(out, o)
	}
	return out
}

// buildOAIChatRequest assembles the OAI request body from a neutral request.
// When streaming, it requests include_usage so the final SSE chunk carries the
// token accounting (OpenAI + compatible APIs omit usage otherwise).
func buildOAIChatRequest(req ChatRequest, model string, stream, videoOK bool) (oaiChatRequest, error) {
	ocr := oaiChatRequest{
		Model:       model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
	if stream {
		ocr.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	msgs := make([]oaiMessage, 0, len(req.System)+len(req.Messages))
	for _, sp := range req.System {
		om, err := encodeOAIMessage(Message{Role: RoleSystem, Parts: []ContentPart{sp}}, videoOK)
		if err != nil {
			return ocr, err
		}
		msgs = append(msgs, om)
	}
	for _, m := range req.Messages {
		om, err := encodeOAIMessage(m, videoOK)
		if err != nil {
			return ocr, err
		}
		msgs = append(msgs, om)
	}
	ocr.Messages = msgs
	if len(req.Tools) > 0 {
		ocr.Tools = encodeOAITools(req.Tools)
	}
	return ocr, nil
}

// ---- model list + embeddings (OpenAI-compatible) ----

type oaiModel struct {
	ID           string `json:"id"`
	Architecture struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
}

type oaiModelList struct {
	Data []oaiModel `json:"data"`
}

type oaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type oaiEmbedData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type oaiEmbedResponse struct {
	Data []oaiEmbedData `json:"data"`
}

// b64encode is a small base64 helper.
func b64encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// decodeOAIResult parses an OAI chat completion response into a Result.
func decodeOAIResult(cr oaiChatResponse) *Result {
	if len(cr.Choices) == 0 {
		return &Result{}
	}
	c := cr.Choices[0]
	res := &Result{
		Text:       c.Message.Content,
		Reasoning:  c.Message.ReasoningContent,
		StopReason: c.FinishReason,
	}
	for _, tc := range c.Message.ToolCalls {
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	if cr.Usage != nil {
		res.Usage = &Usage{
			PromptTokens:     cr.Usage.PromptTokens,
			CompletionTokens: cr.Usage.CompletionTokens,
			TotalTokens:      cr.Usage.TotalTokens,
		}
	}
	return res
}
