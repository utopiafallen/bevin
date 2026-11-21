package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// TestElementTable exercises the server-side #N resolution (the plan's WS5
// element model), which is PER-CONVERSATION (the security boundary): a
// conversation with no reads has no table; a read's elements become
// resolvable by #N (a newer read REPLACES that conversation's table —
// stale ids from an older page fail, they don't silently point at the
// wrong element); and one conversation's table never leaks into another's.
func TestElementTable(t *testing.T) {
	i := &Instance{convRead: map[string]map[int]elementRef{}}
	if _, ok := i.ElementByN("conv", 1); ok {
		t.Fatal("a fresh instance must have no element table")
	}
	i.convRead["conv"] = map[int]elementRef{
		1: {N: 1, Role: "button", Name: "Save", Selector: "body > form > button", Tag: "BUTTON"},
		2: {N: 2, Role: "link", Name: "Home", Selector: "body > a", Tag: "A"},
	}
	e, ok := i.ElementByN("conv", 2)
	if !ok || e.Selector != "body > a" || e.Role != "link" {
		t.Fatalf("ElementByN(2) = %+v, %v", e, ok)
	}
	// A NEW read replaces the conversation's table: #1 from the old read is
	// gone.
	i.convRead["conv"] = map[int]elementRef{7: {N: 7, Role: "textbox", Name: "q", Selector: "#q", Tag: "INPUT"}}
	if _, ok := i.ElementByN("conv", 1); ok {
		t.Fatal("stale #N from a replaced read must not resolve")
	}
	if _, ok := i.ElementByN("conv", 7); !ok {
		t.Fatal("the new read's #N must resolve")
	}
	// Another conversation never sees this one's table.
	if _, ok := i.ElementByN("other", 7); ok {
		t.Fatal("one conversation's element table must not leak into another's")
	}
}

// TestPerConversationBrowserState pins the per-conversation browser security
// boundary (the audit finding: the browser instance is per-USER, shared by
// all of the user's conversations): a conversation must not resolve another
// conversation's #N table, and a tab-less command must never resolve to
// another conversation's tab.
func TestPerConversationBrowserState(t *testing.T) {
	ctx := context.Background()

	// A fake CDP /json endpoint: ResolveTab's live verification reads the
	// tab list over HTTP (no real browser needed).
	var mu sync.Mutex
	openTabs := []map[string]string{{"id": "tabA", "url": "https://a.example/", "title": "A", "type": "page"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		rows := make([]map[string]any, 0, len(openTabs))
		for _, t := range openTabs {
			rows = append(rows, map[string]any{"id": t["id"], "url": t["url"], "title": t["title"], "type": t["type"]})
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	i := &Instance{port: port, convTab: map[string]string{}, convRead: map[string]map[int]elementRef{}}

	// 1. The #N table is per-conversation: each conversation resolves its
	//    OWN last read; a conversation that never read has no table.
	i.convRead["convA"] = map[int]elementRef{1: {N: 1, Role: "button", Name: "bank", Selector: "#bank", Tag: "BUTTON"}}
	i.convRead["convB"] = map[int]elementRef{1: {N: 1, Role: "button", Name: "ad", Selector: "#ad", Tag: "BUTTON"}}
	if e, ok := i.ElementByN("convA", 1); !ok || e.Selector != "#bank" {
		t.Fatalf("convA #1 = %+v, %v — want convA's own element", e, ok)
	}
	if e, ok := i.ElementByN("convB", 1); !ok || e.Selector != "#ad" {
		t.Fatalf("convB #1 = %+v, %v — want convB's own element", e, ok)
	}
	if _, ok := i.ElementByN("convC", 1); ok {
		t.Fatal("convC never read — its #1 must not resolve")
	}

	// 2. A tab-less command never inherits another conversation's tab.
	i.convTab["convA"] = "tabA" // convA opened tabA (OpenTab's pin)
	if _, err := i.ResolveTab(ctx, "convB", ""); err == nil || !strings.Contains(err.Error(), "no tab yet") {
		t.Fatalf("convB's tab-less resolution = %v — want the openTab-first error (never convA's tab)", err)
	}
	if id, err := i.ResolveTab(ctx, "convA", ""); err != nil || id != "tabA" {
		t.Fatalf("convA's tab-less resolution = %q, %v — want its own tab tabA", id, err)
	}

	// 3. An explicit tab choice pins the conversation's working tab.
	if id, err := i.ResolveTab(ctx, "convB", "tabA"); err != nil || id != "tabA" {
		t.Fatalf("convB's explicit tab = %q, %v — want tabA", id, err)
	}
	if id, err := i.ResolveTab(ctx, "convB", ""); err != nil || id != "tabA" {
		t.Fatalf("convB's tab-less resolution after an explicit choice = %q, %v — want tabA", id, err)
	}

	// 4. A pinned tab closed meanwhile (by another conversation, or by the
	//    user watching the window) fails cleanly + unpins — not a raw CDP
	//    error.
	mu.Lock()
	openTabs = nil
	mu.Unlock()
	if _, err := i.ResolveTab(ctx, "convA", ""); err == nil || !strings.Contains(err.Error(), "was closed") {
		t.Fatalf("convA after tabA closed = %v — want the closed-tab error", err)
	}
	if _, err := i.ResolveTab(ctx, "convB", ""); err == nil {
		t.Fatal("convB's pin for the closed tabA must fail cleanly too")
	}
}

// TestFormatRead checks the tool result shape: the title/url header, the
// FULL content (unbounded — a page far beyond the old 32k budget comes
// back whole), the numbered element table, and the partial-clean notice
// when the cleaner's node ceiling was hit.
func TestFormatRead(t *testing.T) {
	pr := &PageRead{
		Title: "Example", URL: "https://example.com",
		Markdown: "# Example Domain\n\nThis domain is for use in examples.\n",
		Elements: []elementRef{
			{N: 1, Role: "link", Name: "More information", Selector: "body > a", Tag: "A"},
		},
	}
	out := FormatRead(pr)
	for _, want := range []string{
		"title: Example",
		"url: https://example.com",
		"content:\n",
		"# Example Domain",
		"#1 link \"More information\" <A>",
		"interactive elements",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("FormatRead output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "TRUNCATED") || strings.Contains(out, "PARTIAL") {
		t.Fatalf("a complete clean must carry no truncation or partial notice:\n%s", out)
	}

	// The budget is gone: a page 7x over the old cap comes back WHOLE.
	big := strings.Repeat("line of page text\n", 10000) // 220k chars
	pr2 := &PageRead{Title: "T", URL: "u", Markdown: big}
	out2 := FormatRead(pr2)
	if !strings.Contains(out2, big) {
		t.Fatalf("over-budget page must come back whole (read is unbounded): got %d chars total, page is %d", len(out2), len(big))
	}
	if strings.Contains(out2, "TRUNCATED") {
		t.Fatal("an unbounded read must never mark the content truncated")
	}

	// A partial clean (the cleaner's node ceiling was hit) says so.
	out3 := FormatRead(&PageRead{Title: "T", URL: "u", Markdown: "partial content", Aborted: true})
	if !strings.Contains(out3, "PARTIAL") {
		t.Fatalf("aborted clean must carry the partial notice:\n%s", out3)
	}
}

// fakeCDP is a CDP endpoint in a test server: it answers each {id, method}
// frame with a canned result (or a canned error for method "boom") and
// records the raw request bodies so the test can assert on the wire (the
// sessionId field is present exactly when a session was given).
func fakeCDP(t *testing.T, got *sync.Mutex, bodies *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req struct {
				ID        int    `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
				// A real JSON object (NOT a base64 string — json.RawMessage
				// would silently decode either, hiding a client that
				// base64-mangles its params).
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				t.Errorf("decode request: %v", err)
				return
			}
			got.Lock()
			*bodies = append(*bodies, string(data))
			got.Unlock()
			var resp string
			switch req.Method {
			case "boom":
				resp = fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"boom failed"}}`, req.ID)
			case "Runtime.evaluate":
				resp = fmt.Sprintf(`{"id":%d,"result":{"result":{"type":"string","value":"hello from fake"}}}`, req.ID)
			default:
				resp = fmt.Sprintf(`{"id":%d,"result":{}}`, req.ID)
			}
			if err := c.Write(r.Context(), websocket.MessageText, []byte(resp)); err != nil {
				return
			}
			// One event frame after each response (the shape of a
			// flattened per-tab event). With no sink registered it is
			// dropped (the v1 default — this also exercises the drop
			// path in TestCDPClientAgainstFakeEndpoint);
			// TestCDPEventRouting registers a sink and asserts delivery.
			ev := `{"method":"Network.loadingFinished","sessionId":"SESS123","params":{"requestId":"1.1"}}`
			if err := c.Write(r.Context(), websocket.MessageText, []byte(ev)); err != nil {
				return
			}
		}
	}))
}

// TestCDPClientAgainstFakeEndpoint drives the CDP client against a fake
// endpoint (the transport seam the cdp.go header promises): id-matched
// request/response, result passthrough, CDP error propagation, the
// sessionId field on the wire (flattened per-tab sessions), and Close.
func TestCDPClientAgainstFakeEndpoint(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := fakeCDP(t, &mu, &bodies)
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	c, err := Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if !c.Alive() {
		t.Fatal("a fresh connection must be alive")
	}

	// A plain call (no session): result passthrough.
	out, err := c.Call(ctx, "", "Runtime.evaluate", map[string]any{"expression": "1+1"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), "hello from fake") {
		t.Fatalf("result = %s, want the fake's value", out)
	}

	// A call WITH a session: the sessionId rides on the wire.
	if _, err := c.Call(ctx, "SESS123", "Runtime.evaluate", map[string]any{"expression": "2+2"}); err != nil {
		t.Fatalf("Call(session): %v", err)
	}

	// A CDP error: propagated with the server's message.
	_, err = c.Call(ctx, "", "boom", nil)
	if err == nil || !strings.Contains(err.Error(), "boom failed") {
		t.Fatalf("boom err = %v, want the CDP error message", err)
	}

	// Two IN-FLIGHT calls resolve independently (no cross-talk between the
	// pending channels).
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Call(ctx, "", "Runtime.evaluate", nil); err != nil {
				t.Errorf("concurrent Call: %v", err)
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent calls did not resolve")
	}

	// The wire: the no-session call carries NO sessionId field; the
	// session call carries it verbatim.
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 3 {
		t.Fatalf("fake saw %d requests, want >= 3", len(bodies))
	}
	if strings.Contains(bodies[0], "sessionId") {
		t.Fatalf("no-session request carries sessionId: %s", bodies[0])
	}
	// The params ride as a real JSON object (the base64-mangling bug the
	// fake's map[string]any decode would also have caught).
	if !strings.Contains(bodies[0], `"params":{"expression":"1+1"}`) {
		t.Fatalf("params are not a plain JSON object on the wire: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"sessionId":"SESS123"`) {
		t.Fatalf("session request missing the sessionId: %s", bodies[1])
	}

	// Close: the connection reports dead.
	c.Close()
	if c.Alive() {
		t.Fatal("a closed connection must not be alive")
	}
}

// TestCDPClientDeadAfterServerGone: when the endpoint dies (the server
// closes the connection), in-flight/next calls fail with a usable error —
// the browser process going away must surface as a tool failure, not a
// hang. (The server-side conn is closed directly: httptest's Close tears
// down the listener but leaves an already-upgraded connection serving.)
func TestCDPClientDeadAfterServerGone(t *testing.T) {
	var serverConn *websocket.Conn
	var gotConn sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		gotConn.Do(func() { serverConn = c })
		// A no-op reader: keep the connection alive until the test closes it.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	c, err := Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Wait for the server side to hold the connection.
	deadline := time.Now().Add(5 * time.Second)
	for serverConn == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if serverConn == nil {
		t.Fatal("the server never accepted the connection")
	}
	// The endpoint dies (the browser process going away looks like this).
	serverConn.Close(websocket.StatusGoingAway, "bye")

	// The next call must fail (not hang): the read loop sees the close.
	if _, err := c.Call(ctx, "", "Runtime.evaluate", nil); err == nil {
		t.Fatal("a call after the endpoint died must fail")
	}
	if c.Alive() {
		t.Fatal("the client must report dead after the endpoint is gone")
	}
}
