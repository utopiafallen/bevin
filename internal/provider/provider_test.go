package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func mustDecodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return b
}

// TestFactoryRejectsSchemelessBase — a base URL without an http(s) scheme
// must be rejected at construction (it used to slip through url.Parse and
// only fail at request time with "unsupported protocol scheme").
func TestFactoryRejectsSchemelessBase(t *testing.T) {
	for _, base := range []string{"localhost.lan:1235", "127.0.0.1:8080", "not a url"} {
		if _, err := New("openai", base, "k"); err == nil {
			t.Errorf("New(openai, %q) accepted, want error", base)
		}
	}
	if _, err := New("openai", "http://h:1/v1", "k"); err != nil {
		t.Fatalf("valid base rejected: %v", err)
	}
	if _, err := New("nope", "http://h:1/v1", "k"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestOpenAIChatRequestFormat(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body = mustDecodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "sk-test")
	res, err := p.Chat(context.Background(), "gpt-4o", ChatRequest{
		System:   []ContentPart{{Text: "You are terse."}},
		Messages: []Message{{Role: RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hi" || res.StopReason != "stop" {
		t.Fatalf("res = %+v", res)
	}
	if res.Usage == nil || res.Usage.TotalTokens != 4 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("auth = %q", auth)
	}
	if body["model"] != "gpt-4o" {
		t.Fatalf("model = %v", body["model"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	m0 := msgs[0].(map[string]any)
	m1 := msgs[1].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "You are terse." {
		t.Fatalf("system msg = %v", m0)
	}
	if m1["role"] != "user" || m1["content"] != "hello" {
		t.Fatalf("user msg = %v", m1)
	}
}

// TestOpenAIReasoningContentWire verifies the reasoning replay on the OAI
// wire: an assistant message carrying a Reasoning trace serializes with the
// trace in the API's native field (reasoning_content, alongside content),
// and a message WITHOUT a trace (the model didn't think / thinking disabled)
// omits the field entirely.
func TestOpenAIReasoningContentWire(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = mustDecodeBody(t, r)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "k")
	_, err := p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{
			{Role: RoleAssistant, Text: "the answer is 42", Reasoning: "step one\nstep two"},
			{Role: RoleUser, Text: "and now what"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "assistant" || m0["content"] != "the answer is 42" {
		t.Fatalf("assistant msg = %v (content must stay the plain reply)", m0)
	}
	if m0["reasoning_content"] != "step one\nstep two" {
		t.Fatalf("reasoning_content = %v, want the stored trace in the API's native field", m0["reasoning_content"])
	}
	// A traceless message omits the field (not an empty string).
	m1 := msgs[1].(map[string]any)
	if _, present := m1["reasoning_content"]; present {
		t.Fatalf("traceless message must omit reasoning_content, got %v", m1)
	}
}

func TestOpenAIVideoUnsupported(t *testing.T) {
	p := NewOpenAI("http://127.0.0.1:0", "k")
	_, err := p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{{Role: RoleUser, Parts: []ContentPart{{Media: &Media{Kind: MediaVideo, URL: "http://x/y.mp4"}}}}},
	})
	if err == nil {
		t.Fatal("expected error for video input on OpenAI")
	}
}

func TestOpenAIStreamAggregation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		f.Flush()
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "k")
	var deltas []string
	res, err := p.Stream(context.Background(), "m", ChatRequest{Messages: []Message{{Role: RoleUser, Text: "hi"}}}, func(c Chunk) error {
		if c.TextDelta != "" {
			deltas = append(deltas, c.TextDelta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.StopReason != "stop" {
		t.Fatalf("stop = %q", res.StopReason)
	}
	if len(deltas) != 2 || deltas[0] != "Hel" || deltas[1] != "lo" {
		t.Fatalf("deltas = %v", deltas)
	}
}

func TestLlamaMultimodalRequestFormat(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = mustDecodeBody(t, r)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := NewLlama(srv.URL, "")
	_, err := p.Chat(context.Background(), "model", ChatRequest{
		Messages: []Message{{Role: RoleUser, Parts: []ContentPart{
			{Text: "describe"},
			{Media: &Media{Kind: MediaVideo, URL: "http://host/v.mp4"}},
			{Media: &Media{Kind: MediaImage, Data: []byte("abc"), MimeType: "image/png"}},
			{Media: &Media{Kind: MediaAudio, Data: []byte("xyz"), MimeType: "audio/mpeg"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 4 {
		t.Fatalf("content = %v", content)
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("c0 = %v", content[0])
	}
	c1 := content[1].(map[string]any)
	if c1["type"] != "video_url" || c1["video_url"].(map[string]any)["url"] != "http://host/v.mp4" {
		t.Fatalf("video part = %v", c1)
	}
	c2 := content[2].(map[string]any)
	if c2["type"] != "image_url" {
		t.Fatalf("image part = %v", c2)
	}
	iu := c2["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(iu, "data:image/png;base64,") {
		t.Fatalf("image data url = %q", iu)
	}
	c3 := content[3].(map[string]any)
	if c3["type"] != "input_audio" {
		t.Fatalf("audio part = %v", c3)
	}
	ia := c3["input_audio"].(map[string]any)
	if ia["format"] != "mp3" || ia["data"] == "" {
		t.Fatalf("audio = %v", ia)
	}
}

func TestLlamaModelsModalities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"llava","architecture":{"input_modalities":["text","image","video"],"output_modalities":["text"]}}]}`))
	}))
	defer srv.Close()
	p := NewLlama(srv.URL, "")
	ms, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != "llava" {
		t.Fatalf("models = %+v", ms)
	}
	if !contains(ms[0].InputModalities, "video") {
		t.Fatalf("modalities = %v", ms[0].InputModalities)
	}
}

func TestLlamaEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := mustDecodeBody(t, r)
		if b["model"] != "emb" {
			t.Errorf("model = %v", b["model"])
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0},{"embedding":[0.3,0.4],"index":1}]}`))
	}))
	defer srv.Close()
	p := NewLlama(srv.URL, "")
	emb, err := p.Embed(context.Background(), "emb", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(emb) != 2 || len(emb[0]) != 2 || emb[0][0] != 0.1 {
		t.Fatalf("emb = %v", emb)
	}
}

func TestAnthropicChatRequestFormat(t *testing.T) {
	var body map[string]any
	var apikey, ver string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apikey = r.Header.Get("x-api-key")
		ver = r.Header.Get("anthropic-version")
		body = mustDecodeBody(t, r)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"yo"},{"type":"tool_use","id":"tu1","name":"get_time","input":{"tz":"UTC"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer srv.Close()

	p := NewAnthropic(srv.URL, "sk-ant")
	res, err := p.Chat(context.Background(), "claude-3-5", ChatRequest{
		System:   []ContentPart{{Text: "You are terse."}},
		Messages: []Message{{Role: RoleUser, Text: "time?"}},
		Tools:    []Tool{{Name: "get_time", Description: "Get time", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if apikey != "sk-ant" || ver == "" {
		t.Fatalf("headers: x-api-key=%q version=%q", apikey, ver)
	}
	if body["system"] != "You are terse." {
		t.Fatalf("system = %v", body["system"])
	}
	if body["max_tokens"] == nil {
		t.Fatal("max_tokens must be present (Anthropic requires it)")
	}
	tools := body["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "get_time" {
		t.Fatalf("tool = %v", tools[0])
	}
	if res.Text != "yo" || res.StopReason != "tool_use" {
		t.Fatalf("res = %+v", res)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "get_time" {
		t.Fatalf("toolcalls = %+v", res.ToolCalls)
	}
	if res.Usage == nil || res.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestAnthropicToolRoundTrip(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = mustDecodeBody(t, r)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	p := NewAnthropic(srv.URL, "k")
	_, err := p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{
			{Role: RoleUser, Text: "time?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "tu1", Name: "get_time", Arguments: `{"tz":"UTC"}`}}},
			{Role: RoleTool, ToolCallID: "tu1", Text: "12:00 UTC"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	// assistant message carries a tool_use block
	ab := msgs[1].(map[string]any)["content"].([]any)
	if ab[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant block = %v", ab[0])
	}
	// tool result is a USER-role message with a tool_result block
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "user" {
		t.Fatalf("tool result role = %v", m2["role"])
	}
	rb := m2["content"].([]any)
	if rb[0].(map[string]any)["type"] != "tool_result" || rb[0].(map[string]any)["tool_use_id"] != "tu1" {
		t.Fatalf("result block = %v", rb[0])
	}
}

func TestAnthropicImageBlock(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = mustDecodeBody(t, r)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"a cat"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()
	p := NewAnthropic(srv.URL, "k")
	_, err := p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{{Role: RoleUser, Parts: []ContentPart{
			{Text: "what is this?"},
			{Media: &Media{Kind: MediaImage, Data: []byte("img"), MimeType: "image/jpeg"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	blocks := msgs[0].(map[string]any)["content"].([]any)
	var img map[string]any
	for _, b := range blocks {
		if b.(map[string]any)["type"] == "image" {
			img = b.(map[string]any)
		}
	}
	if img == nil {
		t.Fatalf("no image block: %v", blocks)
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/jpeg" {
		t.Fatalf("source = %v", src)
	}
}

// TestAnthropicThinkingCapture verifies the Anthropic decoder accumulates
// extended-thinking blocks into Result.Reasoning (persisted + shown in the
// UI like any reasoning) — and that the encoder side deliberately does NOT
// send a thinking block back (replay needs the per-turn signature we don't
// persist yet — see decodeAntResponse).
func TestAnthropicThinkingCapture(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = mustDecodeBody(t, r)
		_, _ = w.Write([]byte(`{"content":[{"type":"thinking","thinking":"hmm, let me work it out","signature":"sig-abc"},{"type":"text","text":"the answer is 42"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	p := NewAnthropic(srv.URL, "k")
	res, err := p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{{Role: RoleUser, Text: "solve it"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "the answer is 42" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.Reasoning != "hmm, let me work it out" {
		t.Fatalf("reasoning = %q, want the thinking block text", res.Reasoning)
	}
	// Replay: send the captured trace back — the encoder must NOT emit a
	// thinking block (no signature to send with it).
	_, err = p.Chat(context.Background(), "m", ChatRequest{
		Messages: []Message{
			{Role: RoleAssistant, Text: "the answer is 42", Reasoning: "hmm, let me work it out"},
			{Role: RoleUser, Text: "why?"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	ab := msgs[0].(map[string]any)["content"].([]any)
	for _, b := range ab {
		if b.(map[string]any)["type"] == "thinking" {
			t.Fatalf("anthropic encoder sent a thinking block (it must not — replay needs the signature): %v", b)
		}
	}
}

func TestAnthropicEmbedUnsupported(t *testing.T) {
	p := NewAnthropic("http://127.0.0.1:0", "k")
	_, err := p.Embed(context.Background(), "m", []string{"x"})
	if err == nil {
		t.Fatal("expected embeddings to be unsupported on Anthropic")
	}
}
