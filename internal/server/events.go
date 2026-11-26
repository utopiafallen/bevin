package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
)

// event is a nudge over the per-user event channel (GET /api/me/events,
// multi-device sync): "this conversation changed — pull it." It carries a
// pointer (conversation id + the seq of the row that landed), never a
// payload: the client reconciles from the store (GET messages), so a
// dropped or out-of-order nudge can only DELAY the other tab by one nudge —
// it can never lose or duplicate content.
type event struct {
	Type           string `json:"type"` // "message" | "conversation"
	ConversationID string `json:"conversation_id"`
	Seq            int    `json:"seq,omitempty"`    // type=message: the persisted row's seq
	Action         string `json:"action,omitempty"` // type=conversation: created|updated|deleted
}

// eventHub is the in-memory per-user nudge fan-out. Single-process server:
// the hub is process memory — a multi-instance deployment is the one thing
// that would need a shared transport, and the hub is the only seam.
type eventHub struct {
	mu   sync.Mutex
	subs map[string]map[chan event]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[string]map[chan event]struct{}{}}
}

// subscribe returns a buffered nudge channel for the handle plus a cancel
// that removes it. The buffer (16 nudges) absorbs brief reader pauses. A
// full buffer is handled by broadcast (it drops the OLDEST nudge, keeping
// the newest — the one with the highest seq, which is the only one that
// tells the client anything it doesn't already have: its pull fetches
// everything after its watermark, new and old alike).
func (h *eventHub) subscribe(handle string) (<-chan event, func()) {
	ch := make(chan event, 16)
	h.mu.Lock()
	if h.subs[handle] == nil {
		h.subs[handle] = map[chan event]struct{}{}
	}
	h.subs[handle][ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if set, ok := h.subs[handle]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, handle)
			}
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

// broadcast sends e to all of the handle's subscribers without blocking the
// publisher — a slow or dead subscriber must never stall a turn or a CRUD
// write. It is safe to broadcast to a handle with no subscribers (no-op).
func (h *eventHub) broadcast(handle string, e event) {
	h.mu.Lock()
	set := h.subs[handle]
	ks := make([]chan event, 0, len(set))
	for k := range set {
		ks = append(ks, k)
	}
	h.mu.Unlock()
	for _, k := range ks {
		select {
		case k <- e:
		default: // full: drop the oldest, keep the newest (see subscribe)
			select {
			case <-k:
			default:
			}
			select {
			case k <- e:
			default:
			}
		}
	}
}

// sseEventWriter returns the event writer for one SSE stream: it formats
// a single event ("event: <name>\ndata: <json>") and flushes it — the
// shared wire format of every SSE endpoint here (the turn stream and the
// per-user nudge channel).
func sseEventWriter(w http.ResponseWriter, fl http.Flusher) func(string, any) error {
	return func(name string, payload any) error {
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
}

// userEvents is the per-user nudge channel (multi-device sync): one
// long-lived SSE per signed-in tab. A second tab of the same user — desktop
// + phone — learns a conversation changed and re-pulls it; the persisted
// conversation is the source of truth, so the channel itself only keeps
// breathing (a comment every 20s, against reverse-proxy and carrier idle
// drops). A dropped channel costs nothing: EventSource auto-reconnects, and
// the client re-pulls on (re)connect and on the tab becoming visible again.
func (a *App) userEvents(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	ch, cancel := a.events.subscribe(u.Handle)
	defer cancel()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	writeEvent := sseEventWriter(w, flusher)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case e := <-ch:
			if err := writeEvent(e.Type, e); err != nil {
				return
			}
		}
	}
}
