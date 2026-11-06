// Package modelclient talks to an external OpenAI-compatible chat model.
package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Message is a chat message in the OpenAI wire format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client is a minimal OpenAI-compatible chat client.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// New builds a Client. baseURL is the OpenAI-compatible root, e.g.
// "http://localhost:11434/v1" (Ollama) or "https://api.openai.com/v1".
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

// Chat sends a non-streaming chat completion request and returns the
// assistant's reply. TODO(streaming): add a streaming variant so tokens
// can be pushed to the web UI as they arrive.
func (c *Client) Chat(ctx context.Context, msgs []Message) (string, error) {
	body, err := json.Marshal(chatRequest{Model: c.model, Messages: msgs})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return "", fmt.Errorf("model returned %s: %s", resp.Status, b)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("model returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}
