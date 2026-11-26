package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// TestEventHubBroadcast: one event reaches every subscriber of the handle
// (and no one else).
func TestEventHubBroadcast(t *testing.T) {
	h := newEventHub()
	ch1, cancel1 := h.subscribe("u1")
	defer cancel1()
	ch2, cancel2 := h.subscribe("u1")
	defer cancel2()
	h.broadcast("u1", event{Type: "message", ConversationID: "c1", Seq: 7})
	for i, c := range []<-chan event{ch1, ch2} {
		select {
		case e := <-c:
			if e.Type != "message" || e.ConversationID != "c1" || e.Seq != 7 {
				t.Fatalf("subscriber %d got %+v", i, e)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: no event", i)
		}
	}
	// Another handle gets nothing.
	other, cancelOther := h.subscribe("u2")
	defer cancelOther()
	select {
	case e := <-other:
		t.Fatalf("u2 received an event meant for u1: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestEventHubCancel: a cancelled subscriber stops receiving, and a
// broadcast with no subscribers is a no-op (no block, no panic).
func TestEventHubCancel(t *testing.T) {
	h := newEventHub()
	ch, cancel := h.subscribe("u1")
	cancel()
	h.broadcast("u1", event{Type: "message", ConversationID: "c1", Seq: 1})
	select {
	case e := <-ch:
		t.Fatalf("cancelled subscriber still receives: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestEventHubFullBufferKeepsNewest: a reader that stops draining — the
// buffer (16) absorbs the burst, and an overflow drops the OLDEST nudge,
// keeping the newest (the only one that tells the client anything it
// doesn't already have: its pull fetches everything after its watermark).
// The broadcast itself never blocks on the slow reader.
func TestEventHubFullBufferKeepsNewest(t *testing.T) {
	h := newEventHub()
	ch, cancel := h.subscribe("u1")
	defer cancel()
	for i := 1; i <= 20; i++ { // 16 buffered + 4 overflows
		h.broadcast("u1", event{Type: "message", ConversationID: "c1", Seq: i})
	}
	var seqs []int
	for len(seqs) < 16 {
		select {
		case e := <-ch:
			seqs = append(seqs, e.Seq)
		case <-time.After(time.Second):
			t.Fatalf("drained only %d of 16 buffered", len(seqs))
		}
	}
	if seqs[0] != 5 || seqs[15] != 20 {
		t.Fatalf("full buffer must keep the 16 newest (5..20): got %v", seqs)
	}
	// The channel still holds exactly the buffer — nothing leaked.
	select {
	case e := <-ch:
		t.Fatalf("more than the buffer survived: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestUserEventsEndpoint: the SSE endpoint authenticates, streams the
// broadcast events in wire format (event: <type>\ndata: <json>), and
// rejects unauthenticated requests.
func TestUserEventsEndpoint(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	srv := httptest.NewServer(a)
	defer srv.Close()

	// Unauthenticated → 401.
	res, err := http.Get(srv.URL + "/api/me/events")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", res.StatusCode)
	}

	// The test session (handle "admin") opens the channel.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me/events", nil)
	req.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-test"})
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}

	// Broadcast one nudge and read it off the wire.
	a.events.broadcast("admin", event{Type: "message", ConversationID: "c1", Seq: 3})
	br := bufio.NewReader(res.Body)
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue // blanks + keepalive comments
		}
		got = append(got, line)
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	if len(got) < 2 || !strings.HasPrefix(got[0], "event: message") {
		t.Fatalf("wire = %q, want an 'event: message' line", got)
	}
	data := got[1]
	if !strings.Contains(data, `"conversation_id":"c1"`) || !strings.Contains(data, `"seq":3`) {
		t.Fatalf("data line = %q, want the conversation id + seq", data)
	}
}
