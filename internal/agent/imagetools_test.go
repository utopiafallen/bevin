package agent

import (
	"context"
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

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/provider"
)

// pngBytes is a tiny standing-in image (the executor and the OAI wire are
// byte-transparent — the assertions are on the BYTES, not the pixels).
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3, 4}

// setVisionAux swaps the live config's aux set to a single vision model at
// the fake endpoint (the npCfg pattern the admin save exercises), deriving
// the pool id the way Validate does.
func setVisionAux(t *testing.T, a *Agent, aux config.AuxModel) {
	t.Helper()
	a.cfgMu.Lock()
	aux.PoolID = ""
	a.cfg.Aux = []config.AuxModel{aux}
	config.AssignAuxPoolIDs(a.cfg)
	a.cfgMu.Unlock()
}

// clearAuxes drops every aux model from the live config.
func clearAuxes(t *testing.T, a *Agent) {
	t.Helper()
	a.cfgMu.Lock()
	a.cfg.Aux = nil
	config.AssignAuxPoolIDs(a.cfg)
	a.cfgMu.Unlock()
}

// TestImageAnalyzeWireOffer pins the offer contract: image_analyze is
// registered always (a slipped-through call must not 404) but offered on
// the wire ONLY while the live config has a vision-capable aux model — an
// audio-only aux doesn't count, and an admin save (the live-config swap)
// applies from the next turn, no restart.
func TestImageAnalyzeWireOffer(t *testing.T) {
	a, cfg, _ := newTestAgent(t)
	defer a.Close()

	// Registered by default (the wire may hide it, never unregister).
	if _, ok := a.tools.Get("image_analyze"); !ok {
		t.Fatal("image_analyze must be registered by default")
	}

	// No aux: not offered.
	names, _ := wireToolNames(a.wireTools(nil, nil))
	if names["image_analyze"] {
		t.Fatal("image_analyze must not be offered without a vision aux")
	}

	// An audio-only aux: still not offered (the kind must include vision).
	setVisionAux(t, a, config.AuxModel{Kinds: []string{"audio"}, Provider: "openai", BaseURL: "http://u", ModelID: "whisper", Slots: 1})
	names, _ = wireToolNames(a.wireTools(nil, nil))
	if names["image_analyze"] {
		t.Fatal("image_analyze must not be offered for an audio-only aux")
	}
	if a.HasVisionAux() {
		t.Fatal("HasVisionAux must be false for an audio-only aux")
	}

	// A vision aux: offered.
	setVisionAux(t, a, config.AuxModel{Kinds: []string{"vision", "audio"}, Provider: "openai", BaseURL: "http://u", ModelID: "llava", Slots: 1})
	names, _ = wireToolNames(a.wireTools(nil, nil))
	if !names["image_analyze"] {
		t.Fatal("image_analyze must be offered with a vision aux")
	}
	if !a.HasVisionAux() {
		t.Fatal("HasVisionAux must be true with a vision aux")
	}

	// Admin removes the aux: the next turn's wire drops it (live, no
	// restart).
	clearAuxes(t, a)
	names, _ = wireToolNames(a.wireTools(nil, nil))
	if names["image_analyze"] {
		t.Fatal("image_analyze must drop off the wire once the aux is removed")
	}
	_ = cfg
}

// TestImageAnalyzeExecutorResults pins the executor's result contract: a
// call with no attachments, an out-of-range attachment number, and a call
// after the aux was removed are all RESULTS (the model adapts within the
// turn), while malformed arguments are an error (executeToolCall shapes
// the failure row).
func TestImageAnalyzeExecutorResults(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	tl, ok := a.tools.Get("image_analyze")
	if !ok {
		t.Fatal("image_analyze must resolve")
	}

	// No attachments: a result, not an error.
	out, err := tl.Execute(context.Background(), "u1", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("no-attachments must be a result, not an error: %v", err)
	}
	if !strings.Contains(out, "no images are attached") {
		t.Fatalf("no-attachments result = %q", out)
	}

	// An out-of-range attachment: a result naming what exists.
	ctx := context.WithValue(context.Background(), ctxTurnImagesKey, []TurnImage{{Name: "a.png", MimeType: "image/png", Data: pngBytes}})
	out, err = tl.Execute(ctx, "u1", json.RawMessage(`{"image":5}`))
	if err != nil {
		t.Fatalf("out-of-range must be a result, not an error: %v", err)
	}
	if !strings.Contains(out, "attachment 5 does not exist") || !strings.Contains(out, "1 image") {
		t.Fatalf("out-of-range result = %q", out)
	}

	// No vision aux configured: a result, not an error (the model can
	// answer from the text or ask the user to retry).
	out, err = tl.Execute(ctx, "u1", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("no-aux must be a result, not an error: %v", err)
	}
	if !strings.Contains(out, "no vision model is configured") {
		t.Fatalf("no-aux result = %q", out)
	}

	// Malformed arguments: an error (the failure row's text).
	if _, err := tl.Execute(context.Background(), "u1", json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed arguments must be an error")
	}
}

// fakeVisionServer is a fake OpenAI-compatible endpoint standing in for the
// vision aux model: it records every request and answers with a fixed
// description.
type fakeVisionServer struct {
	mu       sync.Mutex
	bodies   []string
	desc     string
	requests int
}

func newFakeVisionServer(t *testing.T, desc string) (*fakeVisionServer, *httptest.Server) {
	t.Helper()
	fv := &fakeVisionServer{desc: desc}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fv.mu.Lock()
		fv.bodies = append(fv.bodies, string(b))
		fv.requests++
		fv.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, fv.desc)
	}))
	t.Cleanup(srv.Close)
	return fv, srv
}

func (f *fakeVisionServer) lastBody(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("the vision endpoint was never called")
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeVisionServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// TestImageAnalyzeTurnEndToEnd drives the full loop: a message with an
// attached image, the model calling image_analyze, the perception request
// reaching the vision aux (the wire assertions — its model id, the
// perception system prompt + focus, and the image's EXACT bytes as a
// data-URL part), and the description coming back as the P7 result the
// final call sees.
func TestImageAnalyzeTurnEndToEnd(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fv, vision := newFakeVisionServer(t, "A red error dialog reads 'Disk full' on a C: drive.")
	setVisionAux(t, a, config.AuxModel{Kinds: []string{"vision"}, Provider: "openai", BaseURL: vision.URL + "/v1", APIKey: "k", ModelID: "llava", Slots: 1})

	// The main model: call 1 asks for the image, call 2 is the final reply.
	fp := &scriptedProvider{script: []provider.Result{
		{Text: "let me look", ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "image_analyze", Arguments: `{"focus":"the error text"}`}}},
		{Text: "It says 'Disk full'."},
	}}
	a.providers["m1"] = fp

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	_, reply, err := a.Turn(context.Background(), "u1", convID, "what does it say?", []TurnImage{
		{Name: "shot.png", MimeType: "image/png", Data: pngBytes},
	})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if reply != "It says 'Disk full'." {
		t.Fatalf("reply = %q, want the final call's text", reply)
	}

	// The wire offered image_analyze (a vision aux is live).
	if len(fp.allCalls()) != 2 {
		t.Fatalf("main model calls = %d, want 2", len(fp.allCalls()))
	}
	offered := false
	for _, tl := range fp.allCalls()[0].Tools {
		if tl.Name == "image_analyze" {
			offered = true
		}
	}
	if !offered {
		t.Fatal("the first call must offer image_analyze")
	}

	// The perception wire: the aux's model id, the perception prompt +
	// focus, and the image's exact bytes.
	body := fv.lastBody(t)
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("perception request is not JSON: %v", err)
	}
	if req.Model != "llava" {
		t.Fatalf("perception model = %q, want the aux's model id", req.Model)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("perception messages = %d, want 2 (system + user): %s", len(req.Messages), body)
	}
	sys, _ := req.Messages[0].Content.(string)
	if !strings.Contains(sys, "perception model") || !strings.Contains(sys, "especially interested in: the error text") {
		t.Fatalf("perception system prompt = %q, want the perception job + the focus", sys)
	}
	user, ok := req.Messages[1].Content.([]any)
	if !ok {
		t.Fatalf("perception user content = %T, want the structured part list", req.Messages[1].Content)
	}
	var sawText, sawImage bool
	for _, part := range user {
		p, _ := part.(map[string]any)
		switch p["type"] {
		case "text":
			sawText = true
		case "image_url":
			sawImage = true
			iu, _ := p["image_url"].(map[string]any)
			url, _ := iu["url"].(string)
			const prefix = "data:image/png;base64,"
			if !strings.HasPrefix(url, prefix) {
				t.Fatalf("image_url = %q, want the data: URL of the attachment's bytes", url)
			}
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix))
			if err != nil {
				t.Fatalf("image_url payload is not base64: %v", err)
			}
			if string(decoded) != string(pngBytes) {
				t.Fatalf("perceived %d bytes, want the attachment's exact %d bytes", len(decoded), len(pngBytes))
			}
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("perception user parts = %v, want text + image_url", user)
	}

	// The P7 row: one successful image_analyze row carrying the
	// description (the persistent record of the image).
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	var p7 string
	for _, msg := range msgs {
		if msg.Role != roleAssistant {
			continue
		}
		rows, err := us.ToolCallsForMessage(msg.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Tool == "image_analyze" {
				if !row.Success {
					t.Fatalf("image_analyze P7 row failed: %q", row.Result)
				}
				p7 = row.Result
			}
		}
	}
	if !strings.Contains(p7, "Description of shot.png") || !strings.Contains(p7, "Disk full") {
		t.Fatalf("P7 result = %q, want the named attachment's description", p7)
	}

	// The second call's wire carried the description back to the model.
	if !strings.Contains(fp.allCalls()[1].Messages[2].Text, "Disk full") {
		t.Fatalf("second main-model call lacks the perception result: %+v", fp.allCalls()[1].Messages)
	}
}

// TestImageOnlyMessageMarker pins the transcript contract for an image-only
// message: the persisted user row carries the attachment marker (the bytes
// themselves are never persisted), and the marker is what the model sees.
func TestImageOnlyMessageMarker(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	a.providers["m1"] = &scriptedProvider{script: []provider.Result{{Text: "ok"}}}

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	_, _, err = a.Turn(context.Background(), "u1", convID, "", []TurnImage{
		{Name: "a.png", MimeType: "image/png", Data: pngBytes},
		{Name: "b.png", MimeType: "image/png", Data: pngBytes},
	})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	msgs, err := us.ListMessages(convID)
	if err != nil {
		t.Fatal(err)
	}
	var userContent string
	for _, m := range msgs {
		if m.Role == roleUser {
			userContent = m.Content
		}
	}
	if userContent != "(2 images attached)" {
		t.Fatalf("persisted user row = %q, want the 2-image marker", userContent)
	}
}

// TestImageAnalyzeSharedPoolSelfDeadlock pins the one case where the
// perception must NOT submit to the scheduler: the aux exactly matches the
// conversation's active model and that pool has 1 slot — the turn holds the
// pool's only slot, so a submission would queue behind the turn's own held
// slot forever. The perception runs inline in the turn instead (liveness).
// A regression to the submit path deadlocks: the outer job waits for the
// inner, the inner waits for the slot — the test fails on the deadline.
func TestImageAnalyzeSharedPoolSelfDeadlock(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fv, vision := newFakeVisionServer(t, "A screenshot of a terminal.")

	// An aux that EXACTLY matches m1 (same provider + base + model id):
	// it reuses m1's pool (the shared-pool case).
	m1 := a.cfg.Models[0]
	setVisionAux(t, a, config.AuxModel{Kinds: []string{"vision"}, Provider: m1.Provider, BaseURL: vision.URL + "/v1", APIKey: "k", ModelID: m1.ModelID, Slots: 1})
	// m1's config row must agree with the aux for the match to hold.
	a.cfgMu.Lock()
	a.cfg.Models[0].BaseURL = vision.URL + "/v1"
	config.AssignAuxPoolIDs(a.cfg)
	auxPoolID := a.cfg.Aux[0].PoolID
	a.cfgMu.Unlock()
	if auxPoolID != m1.ID {
		t.Fatalf("aux pool id = %q, want %q (the exact match must reuse the main pool)", auxPoolID, m1.ID)
	}
	a.providers["m1"] = &scriptedProvider{script: []provider.Result{{Text: "ok"}}}

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	// Run the executor while HOLDING m1's only slot — exactly the
	// in-turn situation (a turn runs inside a slot of its conversation's
	// pool). A deadline bounds the deadlock case: if the executor submits
	// to m1's pool, it queues behind the slot we hold and never returns.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var (
		out   string
		exErr error
	)
	err = a.sched.Submit(ctx, JobInfo{Handle: "u1", ConvID: convID, ModelID: m1.ID}, func(jobCtx context.Context) error {
		c := context.WithValue(jobCtx, ctxConversationKey, convID)
		c = context.WithValue(c, ctxTurnImagesKey, []TurnImage{{Name: "a.png", MimeType: "image/png", Data: pngBytes}})
		tl, ok := a.tools.Get("image_analyze")
		if !ok {
			t.Error("image_analyze missing from the registry")
			return nil
		}
		out, exErr = tl.Execute(c, "u1", json.RawMessage(`{}`))
		return nil
	})
	if err != nil {
		t.Fatalf("the turn (holding the shared pool's only slot) could not complete: %v — the perception must run inline, not queue behind its own slot", err)
	}
	if exErr != nil {
		t.Fatalf("inline perception failed: %v", exErr)
	}
	if !strings.Contains(out, "terminal") {
		t.Fatalf("inline perception result = %q, want the description", out)
	}
	if n := fv.count(); n != 1 {
		t.Fatalf("vision endpoint calls = %d, want 1", n)
	}
}

// TestImageAnalyzeOwnPoolQueues pins the other path: an aux with its OWN
// pool (not the conversation's model) submits to the scheduler like any
// other job — it waits while the pool's slot is held, and runs (the vision
// endpoint is called) once it frees.
func TestImageAnalyzeOwnPoolQueues(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	fv, vision := newFakeVisionServer(t, "A photo of a cat.")

	// An aux that does NOT match m1: it gets its own pool.
	m1 := a.cfg.Models[0]
	setVisionAux(t, a, config.AuxModel{Kinds: []string{"vision"}, Provider: m1.Provider, BaseURL: vision.URL + "/v1", APIKey: "k", ModelID: "llava", Slots: 1})
	a.cfgMu.Lock()
	auxPoolID := a.cfg.Aux[0].PoolID
	a.cfgMu.Unlock()
	if auxPoolID == m1.ID {
		t.Fatalf("the aux unexpectedly shares m1's pool (%q)", auxPoolID)
	}
	a.providers["m1"] = &scriptedProvider{script: []provider.Result{{Text: "ok"}}}

	convID, err := a.EnsureConversation(context.Background(), "u1", "", "test")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	// Hold the aux pool's only slot (a sibling perception in flight).
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- a.sched.Submit(context.Background(), JobInfo{Handle: "u1", ConvID: convID, ModelID: auxPoolID}, func(context.Context) error {
			<-release
			return nil
		})
	}()
	// Wait until the holder actually holds the slot.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if l := a.sched.Load()[auxPoolID]; l.Busy >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder never got the aux pool's slot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The executor queues (it must NOT run while the slot is held — the
	// vision endpoint must still be quiet).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var (
		out   string
		exErr error
	)
	done := make(chan struct{})
	go func() {
		c := context.WithValue(ctx, ctxConversationKey, convID)
		c = context.WithValue(c, ctxTurnImagesKey, []TurnImage{{Name: "cat.png", MimeType: "image/png", Data: pngBytes}})
		tl, ok := a.tools.Get("image_analyze")
		if !ok {
			t.Error("image_analyze missing from the registry")
			return
		}
		out, exErr = tl.Execute(c, "u1", json.RawMessage(`{}`))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the perception ran while the aux pool's slot was held — it must queue")
	case <-time.After(300 * time.Millisecond):
	}
	if n := fv.count(); n != 0 {
		t.Fatalf("vision endpoint calls while the pool was held = %d, want 0", n)
	}

	// The slot frees: the queued perception runs + completes.
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued perception never ran after the slot freed")
	}
	if err := <-holderDone; err != nil {
		t.Fatalf("holder job: %v", err)
	}
	if exErr != nil {
		t.Fatalf("queued perception failed: %v", exErr)
	}
	if !strings.Contains(out, "cat") {
		t.Fatalf("queued perception result = %q, want the description", out)
	}
	if n := fv.count(); n != 1 {
		t.Fatalf("vision endpoint calls = %d, want 1", n)
	}
}
