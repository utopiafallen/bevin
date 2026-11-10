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

// Llama is a provider for llama.cpp (llama-server) endpoints. It speaks the
// OpenAI-compatible wire but adds llama's multimodal content parts (image /
// audio / video) and reads the real modalities from /v1/models. Video needs
// ffmpeg on the server side (--video-fps / --video-ffmpeg-dir); that is an
// operator concern, not an bevin one.
type Llama struct {
	BaseURL string // e.g. http://127.0.0.1:8080/v1
	APIKey  string
	HTTP    *http.Client
}

func NewLlama(baseURL, apiKey string) *Llama {
	return &Llama{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: defaultHTTP()}
}

func (l *Llama) Name() string { return "llama" }

func (l *Llama) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, l.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", l.Name(), resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (l *Llama) Chat(ctx context.Context, model string, req ChatRequest) (*Result, error) {
	ocr, err := buildOAIChatRequest(req, model, false, true) // videoOK
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(ocr)
	var cr oaiChatResponse
	if err := l.do(ctx, http.MethodPost, "/chat/completions", b, &cr); err != nil {
		return nil, err
	}
	return decodeOAIResult(cr), nil
}

func (l *Llama) Stream(ctx context.Context, model string, req ChatRequest, emit func(Chunk) error) (*Result, error) {
	ocr, err := buildOAIChatRequest(req, model, true, true) // videoOK
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(ocr)
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "text/event-stream")
	if l.APIKey != "" {
		req2.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	resp, err := l.HTTP.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s: %s", l.Name(), resp.Status, strings.TrimSpace(string(bb)))
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

// Models lists models with their modalities (llama exposes
// architecture.input_modalities / output_modalities).
func (l *Llama) Models(ctx context.Context) ([]ModelInfo, error) {
	var lm oaiModelList
	if err := l.do(ctx, http.MethodGet, "/models", nil, &lm); err != nil {
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

// Embed returns embeddings (multimodal-capable on llama-server).
func (l *Llama) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	var er oaiEmbedRequest
	er.Model = model
	er.Input = inputs
	b, _ := json.Marshal(er)
	var ev oaiEmbedResponse
	if err := l.do(ctx, http.MethodPost, "/embeddings", b, &ev); err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(ev.Data))
	for _, d := range ev.Data {
		out = append(out, d.Embedding)
	}
	return out, nil
}