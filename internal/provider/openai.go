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

// OpenAI is a provider for OpenAI-compatible /v1 endpoints (OpenAI itself and
// any drop-in). It does not advertise video input in chat (OpenAI has no
// in-chat video part); use the Llama provider for llama.cpp.
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	HTTP    *http.Client
}

func NewOpenAI(baseURL, apiKey string) *OpenAI {
	return &OpenAI{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: defaultHTTP()}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, o.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", o.Name(), resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (o *OpenAI) Chat(ctx context.Context, model string, req ChatRequest) (*Result, error) {
	ocr, err := buildOAIChatRequest(req, model, false, false)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(ocr)
	var cr oaiChatResponse
	if err := o.do(ctx, http.MethodPost, "/chat/completions", b, &cr); err != nil {
		return nil, err
	}
	return decodeOAIResult(cr), nil
}

func (o *OpenAI) Stream(ctx context.Context, model string, req ChatRequest, emit func(Chunk) error) (*Result, error) {
	ocr, err := buildOAIChatRequest(req, model, true, false)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(ocr)
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "text/event-stream")
	if o.APIKey != "" {
		req2.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.HTTP.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s: %s", o.Name(), resp.Status, strings.TrimSpace(string(bb)))
	}

	var res Result
	scanner := newLineScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		c := Chunk{}
		for _, ch := range chunk.Choices {
			c.TextDelta += ch.Delta.Content
			c.ReasoningDelta += ch.Delta.ReasoningContent
			c.StopReason = ch.FinishReason
		}
		if chunk.Usage != nil {
			c.Usage = &Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens}
			res.Usage = c.Usage
		}
		res.Text += c.TextDelta
		res.Reasoning += c.ReasoningDelta
		if c.StopReason != "" {
			res.StopReason = c.StopReason
			c.Done = true
		}
		if emit != nil {
			if err := emit(c); err != nil {
				return &res, err
			}
		}
	}
	return &res, scanner.Err()
}

func (o *OpenAI) Models(ctx context.Context) ([]ModelInfo, error) {
	var lm oaiModelList
	if err := o.do(ctx, http.MethodGet, "/models", nil, &lm); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(lm.Data))
	for _, m := range lm.Data {
		out = append(out, ModelInfo{
			ID:               m.ID,
			InputModalities:  m.Architecture.InputModalities,
			OutputModalities: m.Architecture.OutputModalities,
		})
	}
	return out, nil
}

func (o *OpenAI) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	var er oaiEmbedRequest
	er.Model = model
	er.Input = inputs
	b, _ := json.Marshal(er)
	var ev oaiEmbedResponse
	if err := o.do(ctx, http.MethodPost, "/embeddings", b, &ev); err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(ev.Data))
	for _, d := range ev.Data {
		out = append(out, d.Embedding)
	}
	return out, nil
}