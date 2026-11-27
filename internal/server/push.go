package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/SherClockHolmes/webpush-go"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/store"
)

// Web push (the PWA stage 2): the server-side notification transport.
//
// Fire points (see the design log's PWA section):
//   - pushTurnDone / pushTurnFailed — an interactive turn settled (the
//     streaming handler's settle point, success AND error)
//   - the NotifyTaskTerminal hook — a task settled done/blocked
//   - App.notify — system notifications (restart, disk unfreeze)
//
// Deliberately NOT the store's per-row AfterAppend nudge: a turn
// persists several rows (user + one assistant per tool-loop iteration +
// the reply), so per-row would push per tool call. The event channel's
// pointer-only contract is untouched — the push module composes its
// notification from the store at send time.

// pushPayload is what rides in the notification. Small, and all content
// composed server-side at send time (the lock screen shows the title +
// body — a personal single-user app, the message-app pattern). The
// browser auto-decrypts the standard aes128gcm payload; the SW parses
// this JSON and shows it (with its own visibility gate).
type pushPayload struct {
	ConvID string `json:"conv_id,omitempty"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	Kind   string `json:"kind"` // "turn" | "task" | "system"
}

// pushVAPID resolves the live [push] config into send-ready VAPID
// material. ok=false = web push is off (no keys) — every send path is a
// no-op, and /api/push/status reports configured:false so the UI shows
// the "not configured on this server yet." state.
func (a *App) pushVAPID() (pub, priv, subject string, ok bool) {
	cfg := a.liveCfg()
	p := cfg.Push
	if !p.Configured() {
		return "", "", "", false
	}
	// The VAPID Subject header: the instance's public base URI (an https
	// URL is a valid subject); a local-only setup (no base URI) falls
	// back to a mailto.
	subject = cfg.BaseURI
	if subject == "" {
		subject = "mailto:bevin@localhost"
	}
	return p.VAPIDPublic, p.VAPIDPrivate, subject, true
}

// pushSend fans one notification out to ALL of the user's subscribed
// devices. Fire-and-forget, exactly the email-notify contract: a push
// must never block or break the calling path. One goroutine per device;
// a 4xx from the push service prunes the dead subscription (the device
// is gone — 404/410 and friends), a 5xx is logged (no retry queue in
// v1). Never returns an error.
func (a *App) pushSend(handle string, title, body, convID, kind string) {
	pub, priv, subject, ok := a.pushVAPID()
	if !ok {
		return
	}
	us, err := a.ag.UserStoreFor(handle)
	if err != nil {
		return
	}
	subs, err := us.ListPushSubscriptions()
	if err != nil {
		log.Printf("push %s: list subscriptions: %v", handle, err)
		return
	}
	if len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(pushPayload{ConvID: convID, Title: title, Body: body, Kind: kind})
	if err != nil {
		log.Printf("push %s: marshal: %v", handle, err)
		return
	}
	for _, s := range subs {
		go func(s store.PushSubscription) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
				Endpoint: s.Endpoint,
				Keys:     webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
			}, &webpush.Options{
				TTL:             60 * 60,
				Subscriber:      subject,
				VAPIDPublicKey:  pub,
				VAPIDPrivateKey: priv,
			})
			if err != nil {
				log.Printf("push %s (%s): %v", handle, kind, err)
				return
			}
			defer resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
				(resp.StatusCode >= 400 && resp.StatusCode < 500):
				_ = us.RemovePushSubscription(s.Endpoint)
				log.Printf("push %s: subscription pruned (HTTP %d)", handle, resp.StatusCode)
			case resp.StatusCode >= 500:
				log.Printf("push %s (%s): push service HTTP %d", handle, kind, resp.StatusCode)
			}
		}(s)
	}
}

// convNotification resolves a conversation's notification parts: its
// user-facing label + the first line of its newest assistant reply.
// The label follows the UI rule — the rolling PURPOSE replaces the
// title wherever the conversation's label is displayed (the purpose
// decision); the title is the pre-purpose fallback. This is the single
// seam if the label rule ever changes. label == "" means the
// conversation no longer exists (deleted mid-turn — nothing to notify
// about).
func (a *App) convNotification(handle, convID string) (label, firstLine string) {
	if convID == "" {
		return "", ""
	}
	us, err := a.ag.UserStoreFor(handle)
	if err != nil {
		return "", ""
	}
	c, err := us.GetConversation(convID)
	if err != nil {
		return "", ""
	}
	label = c.Purpose
	if label == "" {
		label = c.Title
	}
	if m, err := us.LatestAssistantReply(convID); err == nil && m != nil {
		firstLine = firstLineOf(m.Content, 120)
	}
	return label, firstLine
}

// pushTurnDone notifies that an interactive turn settled (the primary
// PWA event). No-op when push is off, the user has no subscriptions, or
// the conversation is gone.
func (a *App) pushTurnDone(handle, convID, reply string) {
	label, body := a.convNotification(handle, convID)
	if label == "" {
		return
	}
	if body == "" {
		body = firstLineOf(reply, 120)
	}
	a.pushSend(handle, label, body, convID, "turn")
}

// pushTurnFailed is the error variant — an errored turn pushes too
// (the user must know the agent could not finish).
func (a *App) pushTurnFailed(handle, convID string, err error) {
	label, _ := a.convNotification(handle, convID)
	if label == "" {
		return
	}
	a.pushSend(handle, label, "Turn failed: "+firstLineOf(err.Error(), 120), convID, "turn")
}

// firstLineOf returns the first line of s, trimmed and capped at max
// runes (the notification-body cap — the lock screen shows one line).
func firstLineOf(s string, max int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// ---- the user-facing API (session-gated, per-user). -----------------

// pushStatus reports whether the server has web-push keys + the public
// key (the client's subscribe needs it as the applicationServerKey).
func (a *App) pushStatus(w http.ResponseWriter, r *http.Request) {
	pub, _, _, ok := a.pushVAPID()
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": ok,
		"public_key": pub,
	})
}

// pushSubscribe (POST) upserts the current user's device subscription;
// (DELETE) removes it (unsubscribe — the only way a device leaves;
// there is no per-user delivery flag). The body is the browser's
// PushSubscription shape + the user agent (diagnostic).
func (a *App) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		UserAgent string `json:"user_agent"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	us, err := a.ag.UserStoreFor(u.Handle)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no user store"})
		return
	}
	if r.Method == http.MethodDelete {
		if err := us.RemovePushSubscription(req.Endpoint); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err := us.AddPushSubscription(store.PushSubscription{
		Endpoint:  req.Endpoint,
		P256dh:    req.Keys.P256dh,
		Auth:      req.Keys.Auth,
		UserAgent: req.UserAgent,
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// A subscription is a delivery grant: confirm it arrived with a test
	// notification (the user sees the banner the moment they enable —
	// and a broken keypair fails HERE, not on the first real turn).
	a.pushSend(u.Handle, "bevin", "Notifications are on for this device.", "", "system")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
