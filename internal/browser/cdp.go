package browser

// A minimal CDP (Chrome DevTools Protocol) client over one WebSocket — the
// subset the web_browser tool needs (Target/Page/Runtime/Input calls). The
// plan's WS5 left the client choice open (chromedp or a raw subset); this is
// the raw subset: one dependency (nhooyr.io/websocket), a transport seam
// the tests drive with a fake, and no event machinery — v1 waits by
// POLLING (document.readyState, selector presence), which is what a
// deterministic tool wants anyway.
//
// One connection per browser, at the BROWSER-level endpoint (the
// /json/version webSocketDebuggerUrl). Per-tab commands ride the same
// connection on a flattened session id (Target.attachToTarget with
// flatten:true) — one CDP endpoint per user, no cross-user tab contention,
// no per-tab connections.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"nhooyr.io/websocket"
)

// cdpMessage is one CDP frame: a response (id + result or error) or an
// event (method + params, no id). Commands (id + method, no result) are
// outgoing only.
type cdpMessage struct {
	ID     *int            `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
	// SessionID is present on frames from a flattened per-tab session
	// (the page-level events — the Network-domain events the settle
	// counter consumes). Empty for browser-level frames.
	SessionID string `json:"sessionId,omitempty"`
}

// cdpError is a CDP protocol-level error (method ran but failed — e.g. a
// Runtime.evaluate exception, a Page.navigate failure).
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("CDP error %d: %s: %s", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// Client is one CDP connection with in-flight call matching (each command
// gets a unique id; the reader loop delivers its response on a per-call
// channel). Events (no id) go to the optional event sink — nil = dropped
// (v1's default: waits poll, not on events; the settle counter is the
// first consumer, see settle.go).
type Client struct {
	conn   *websocket.Conn
	ctx    context.Context // the read context — canceled on Close
	cancel context.CancelFunc

	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpMessage

	eventMu   sync.Mutex
	eventSink func(sessionID, method string, params json.RawMessage)

	done     chan struct{} // closed when the connection is gone
	doneOnce sync.Once
	cause    error
}

// Dial connects the CDP endpoint (the browser-level ws URL) and starts the
// reader loop.
func Dial(ctx context.Context, wsURL string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp connect %s: %w", wsURL, err)
	}
	rctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		conn:    conn,
		ctx:     rctx,
		cancel:  cancel,
		pending: map[int]chan cdpMessage{},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// readLoop pumps frames until the connection dies, matching responses to
// their callers.
func (c *Client) readLoop() {
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.finish(err)
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue // a CDP endpoint only ever sends well-formed frames
		}
		if msg.ID != nil {
			c.mu.Lock()
			ch := c.pending[*msg.ID]
			c.mu.Unlock()
			if ch != nil { // buffered(1) — never blocks, even if the caller gave up
				ch <- msg
			}
			continue
		}
		// Event (no id): deliver it to the sink when one is registered —
		// with none, it is dropped (v1 waits by polling, not on events;
		// the Network-domain counter is the first consumer).
		if msg.Method == "" {
			continue
		}
		c.eventMu.Lock()
		sink := c.eventSink
		c.eventMu.Unlock()
		if sink != nil {
			sink(msg.SessionID, msg.Method, msg.Params)
		}
	}
}

// SetEventSink installs the connection's event consumer (nil = drop all
// events — the v1 default). At most one sink: the browser instance's
// Network-domain counter (settle.go) is the sole consumer.
func (c *Client) SetEventSink(fn func(sessionID, method string, params json.RawMessage)) {
	c.eventMu.Lock()
	c.eventSink = fn
	c.eventMu.Unlock()
}

// finish marks the connection dead (first cause wins) and unblocks every
// pending Call + the alive check.
func (c *Client) finish(err error) {
	c.mu.Lock()
	if c.cause == nil {
		c.cause = err
	}
	c.mu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
}

// removePending forgets an in-flight call (the caller gave up).
func (c *Client) removePending(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Alive reports whether the connection is still up (the manager's
// liveness check for a per-user instance — a browser the user closed takes
// its CDP endpoint down with it).
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Cause returns why the connection died ("" while alive; the read/write
// error that took it down, or the Close sentinel).
func (c *Client) Cause() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// Call issues one CDP command and waits for its response. sessionID is ""
// for browser-level commands (Target.*) or the flattened session id for
// page-level ones (Page.*/Runtime.*/Input.*). A protocol error (cdpError)
// comes back as a Go error with the CDP message + data (e.g. an evaluate
// exception).
func (c *Client) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	if err := c.dead(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan cdpMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	frame := map[string]any{"id": id, "method": method}
	if sessionID != "" {
		frame["sessionId"] = sessionID
	}
	if params != nil {
		// Assign the value DIRECTLY (json.Marshal of the frame encodes it as
		// a JSON object). NEVER store a pre-marshaled []byte here: Go
		// base64-encodes []byte in JSON, and the browser would reject the
		// params ("may have object 'params' property").
		frame["params"] = params
	}
	data, err := json.Marshal(frame)
	if err != nil {
		c.removePending(id)
		return nil, fmt.Errorf("cdp %s frame: %w", method, err)
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		c.removePending(id)
		c.finish(err)
		return nil, fmt.Errorf("cdp %s: %w", method, err)
	}
	select {
	case msg := <-ch:
		c.removePending(id)
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-c.done:
		c.removePending(id)
		return nil, c.dead()
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	}
}

// dead returns the connection's death error, or nil while alive.
func (c *Client) dead() error {
	select {
	case <-c.done:
		c.mu.Lock()
		cause := c.cause
		c.mu.Unlock()
		if cause == nil {
			return errors.New("cdp connection closed")
		}
		return cause
	default:
		return nil
	}
}

// Close tears the connection down (normal closure; the browser keeps
// running — killing it is the Instance's job).
func (c *Client) Close() {
	c.finish(errors.New("cdp connection closed"))
	c.cancel()
	_ = c.conn.Close(websocket.StatusNormalClosure, "closing")
}
