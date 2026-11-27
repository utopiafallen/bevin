package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/store"
)

// visionAuxApp builds a chat app with a main model (a scripted SSE fake:
// the first call issues an image_analyze tool call, the second is the
// final text) + a vision aux model (a plain OAI fake recording the
// requests it receives).
func visionAuxApp(t *testing.T) (*App, *config.ServerConfig, *store.ServerStore, *string, *int) {
	t.Helper()
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	prov := auth.NewInternalProvider(srv, time.Hour)

	var mu sync.Mutex
	var lastVisionBody string
	var visionCalls int
	// The main model's script keys off the tool transcript: call 1 has no
	// tool result on the wire yet (it issues the image_analyze call); call
	// 2 carries the paired role=tool result (the final text follows).
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		second := strings.Contains(string(b), `"role":"tool"`)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		write := func(s string) { _, _ = w.Write([]byte(s)); f.Flush() }
		if !second {
			write(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"image_analyze","arguments":"{}"}}]}}]}` + "\n\n")
			write(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}` + "\n\n")
		} else {
			write(`data: {"choices":[{"delta":{"content":"I see a cat."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}` + "\n\n")
		}
		write("data: [DONE]\n\n")
	}))
	t.Cleanup(main.Close)
	vision := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastVisionBody = string(b)
		visionCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"A photo of a cat."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	t.Cleanup(vision.Close)

	cfg := &config.ServerConfig{
		DataDir: dir,
		Auth:    config.Auth{SessionTTLSeconds: 3600},
		Models: []config.Model{{
			ID: "m1", Provider: "openai", BaseURL: main.URL + "/v1",
			APIKey: "k-main", ModelID: "gpt-4o", DisplayName: "GPT-4o",
			MaxContext: 128000, Slots: 1, Enabled: true,
		}},
		Aux: []config.AuxModel{{
			Kinds: []string{"vision"}, Provider: "openai", BaseURL: vision.URL + "/v1",
			APIKey: "k-vision", ModelID: "llava", Slots: 1,
		}},
	}
	ag, err := newAgentForTest(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	a := &App{cfg: cfg, prov: prov, ag: ag, srv: srv, ttl: time.Hour, dataDir: dir}
	a.routes()
	return a, cfg, srv, &lastVisionBody, &visionCalls
}

// pngAttachment is a tiny standing-in image (byte-transparent — the
// assertion is on the BYTES reaching the vision endpoint).
var pngAttachment = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3, 4}

func pngImageMap() map[string]any {
	return map[string]any{"name": "cat.png", "mime_type": "image/png", "data": base64.StdEncoding.EncodeToString(pngAttachment)}
}

// TestChatImageAttachments — the chat API's attachment contract: a
// text+image message streams through and the image's exact bytes reach the
// vision aux (the model called image_analyze); an image-only message is
// accepted and persists the marker row; and the rejection matrix (a
// non-image mime, an empty payload, an over-limit image, more than 4
// images) refuses BEFORE the turn (no message persisted, the vision
// endpoint never called).
func TestChatImageAttachments(t *testing.T) {
	a, _, _, lastVisionBody, visionCalls := visionAuxApp(t)

	mkConv := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob", map[string]string{"title": "t"}))
		if w.Code != http.StatusOK {
			t.Fatalf("create code = %d body=%s", w.Code, w.Body.String())
		}
		var res map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return res["id"].(string)
	}

	// /api/models reports the vision surface (the UI renders its
	// attachment controls from it).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/api/models", "tok-bob", nil))
	var modelsResp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &modelsResp)
	if modelsResp["vision"] != true {
		t.Fatalf("models response vision = %v, want true (a vision aux is configured)", modelsResp["vision"])
	}

	// 1. Text + image: the turn streams, the model's image_analyze call
	// reaches the vision aux with the image's exact bytes, and the reply
	// lands.
	convID := mkConv()
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations/"+convID+"/messages/stream", "tok-bob",
		map[string]any{"message": "what is this?", "images": []any{pngImageMap()}}))
	if w.Code != http.StatusOK {
		t.Fatalf("text+image stream code = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: done") || !strings.Contains(body, `"reply":"I see a cat."`) {
		t.Fatalf("stream missing done/final reply: %.400s", body)
	}
	if n := *visionCalls; n != 1 {
		t.Fatalf("vision endpoint calls = %d, want 1 (the image_analyze call)", n)
	}
	// The perception request carried the image's exact bytes as a data URL.
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngAttachment)
	if !strings.Contains(*lastVisionBody, wantURL) {
		t.Fatalf("perception request lacks the image's exact bytes:\n%s", *lastVisionBody)
	}

	// 2. Image-only: accepted; the persisted user row is the marker (the
	// bytes are never persisted).
	convID2 := mkConv()
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations/"+convID2+"/messages/stream", "tok-bob",
		map[string]any{"message": "", "images": []any{pngImageMap()}}))
	if w.Code != http.StatusOK {
		t.Fatalf("image-only stream code = %d body=%s", w.Code, w.Body.String())
	}
	msgs, err := a.ag.ConversationMessages("bob", convID2)
	if err != nil {
		t.Fatalf("ConversationMessages: %v", err)
	}
	var userText string
	for _, m := range msgs {
		if m.Role == "user" {
			userText = m.Content
		}
	}
	if userText != "(1 image attached)" {
		t.Fatalf("persisted user row = %q, want the image-only marker", userText)
	}
	if n := *visionCalls; n != 2 {
		t.Fatalf("vision endpoint calls = %d, want 2", n)
	}

	// 3. Rejection matrix: nothing runs, nothing persists.
	reject := func(name string, images []any, wantCode int, wantBody string) {
		t.Helper()
		c := mkConv()
		w := httptest.NewRecorder()
		a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations/"+c+"/messages/stream", "tok-bob",
			map[string]any{"message": "hi", "images": images}))
		if w.Code != wantCode {
			t.Fatalf("%s: code = %d, want %d (body=%s)", name, w.Code, wantCode, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), wantBody) {
			t.Fatalf("%s: body = %s, want it containing %q", name, w.Body.String(), wantBody)
		}
		msgs, err := a.ag.ConversationMessages("bob", c)
		if err != nil {
			t.Fatalf("%s: ConversationMessages: %v", name, err)
		}
		if len(msgs) != 0 {
			t.Fatalf("%s: a rejected send persisted %d rows, want 0", name, len(msgs))
		}
	}
	before := *visionCalls
	reject("non-image mime", []any{map[string]any{"name": "x.txt", "mime_type": "text/plain", "data": base64.StdEncoding.EncodeToString([]byte("hi"))}},
		http.StatusBadRequest, "not an image")
	reject("empty payload", []any{map[string]any{"name": "x.png", "mime_type": "image/png", "data": ""}},
		http.StatusBadRequest, "empty")
	over := make([]byte, 8<<20+1) // over the 8 MiB per-image limit
	reject("over-limit image", []any{map[string]any{"name": "big.png", "mime_type": "image/png", "data": base64.StdEncoding.EncodeToString(over)}},
		http.StatusRequestEntityTooLarge, "8 MiB")
	var five []any
	for i := 0; i < 5; i++ {
		five = append(five, pngImageMap())
	}
	reject("five images", five, http.StatusBadRequest, "at most 4")
	if n := *visionCalls; n != before {
		t.Fatalf("rejected sends still called the vision endpoint (%d -> %d)", before, n)
	}

	// 4. No message AND no images: the old blank-rejection stands.
	c := mkConv()
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations/"+c+"/messages/stream", "tok-bob", map[string]any{}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank send code = %d, want 400", w.Code)
	}
}
