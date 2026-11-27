package server

import (
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SherClockHolmes/webpush-go"

	"github.com/utopiafallen/bevin/internal/agent"
	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/setup"
	"github.com/utopiafallen/bevin/internal/store"
)

// testAppWithPush builds a testApp + a live agent + [push] keys, so the
// push paths can resolve the per-user store and actually send.
func testAppWithPush(t *testing.T) (*App, string) {
	t.Helper()
	_, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := testApp(t, config.Email{})
	a.cfg.Push = config.Push{VAPIDPublic: pub, VAPIDPrivate: "test-private-key"}
	a.cfg.Models = []config.Model{{
		ID: "m1", Provider: "openai", BaseURL: "http://127.0.0.1:1/v1",
		APIKey: "k", ModelID: "m1", Enabled: true,
	}}
	ag, err := agent.New(a.cfg, a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a.ag = ag
	return a, pub
}

// waitPush polls until cond() is true (the push fan-out is fire-and-forget
// goroutines — the tests must wait, not sleep-and-hope).
func waitPush(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the push")
}

// storeSub is a subscription row for the tests (endpoint is what the push
// goes to — the fake push service's URL). The device keys must be REAL
// (the library does the ECDH with the device's P-256 key server-side and
// rejects a fake point).
func storeSub(endpoint string) store.PushSubscription {
	return store.PushSubscription{Endpoint: endpoint, P256dh: realP256dh(), Auth: realAuth(), UserAgent: "test"}
}

func realP256dh() string {
	_, x, y, err := elliptic.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), x, y))
}

func realAuth() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// TestPushStatusEndpoint — configured vs unconfigured.
func TestPushStatusEndpoint(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	srv := httptest.NewServer(a)
	defer srv.Close()

	get := func() struct {
		Configured bool   `json:"configured"`
		PublicKey  string `json:"public_key"`
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/push/status", nil)
		req.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-test"})
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var st struct {
			Configured bool   `json:"configured"`
			PublicKey  string `json:"public_key"`
		}
		_ = json.NewDecoder(res.Body).Decode(&st)
		return st
	}

	st := get()
	if st.Configured || st.PublicKey != "" {
		t.Fatalf("unconfigured: got %+v", st)
	}

	a.cfg.Push = config.Push{VAPIDPublic: "pub", VAPIDPrivate: "priv"}
	st = get()
	if !st.Configured || st.PublicKey != "pub" {
		t.Fatalf("configured: got %+v", st)
	}
}

// TestPushSubscribeEndpoint — the per-user subscription lifecycle.
func TestPushSubscribeEndpoint(t *testing.T) {
	a, _ := testAppWithPush(t)
	srv := httptest.NewServer(a)
	defer srv.Close()

	// Unauthenticated → 401.
	res, _ := http.Get(srv.URL + "/api/push/status")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", res.StatusCode)
	}

	p256dh, authKey := realP256dh(), realAuth()
	body := fmt.Sprintf(`{"endpoint":"https://push.example/ep1","keys":{"p256dh":%q,"auth":%q},"user_agent":"test"}`, p256dh, authKey)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/push/subscribe", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-test"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("subscribe status = %d, want 200", res.StatusCode)
	}

	us, _ := a.ag.UserStoreFor("admin")
	subs, _ := us.ListPushSubscriptions()
	if len(subs) != 1 || subs[0].Endpoint != "https://push.example/ep1" || subs[0].P256dh != p256dh {
		t.Fatalf("stored subscriptions = %+v", subs)
	}

	// Unsubscribe.
	del, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/push/subscribe", strings.NewReader(`{"endpoint":"https://push.example/ep1"}`))
	del.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-test"})
	res, _ = http.DefaultClient.Do(del)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unsubscribe status = %d, want 200", res.StatusCode)
	}
	subs, _ = us.ListPushSubscriptions()
	if len(subs) != 0 {
		t.Fatalf("after unsubscribe: %+v", subs)
	}
}

// TestPushSendDeliversAndPrunes — a fake push service stands in for
// FCM/Mozilla (the stored endpoint URL is what we POST to, so the test
// points it at a local server): the wire format is a VAPID JWS, and a
// 4xx prunes the dead subscription.
func TestPushSendDeliversAndPrunes(t *testing.T) {
	a, _ := testAppWithPush(t)

	var mu sync.Mutex
	var gotAuth, gotBody string
	code := 201 // FCM's accepted
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotBody = string(buf)
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer fake.Close()

	us, _ := a.ag.UserStoreFor("admin")
	if err := us.AddPushSubscription(storeSub(fake.URL + "/sub1")); err != nil {
		t.Fatal(err)
	}

	a.pushSend("admin", "The label", "first line", "c1", "turn")
	waitPush(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotAuth != ""
	})
	mu.Lock()
	authHdr, body := gotAuth, gotBody
	mu.Unlock()
	if !strings.HasPrefix(authHdr, "vapid ") || !strings.Contains(authHdr, "t=") || !strings.Contains(authHdr, "k=") {
		t.Fatalf("Authorization header = %q, want the VAPID vapid-scheme header", authHdr)
	}
	// The body is the RAW encrypted payload (the library's wire format
	// puts the JWS in the Authorization header's t= parameter).
	if len(body) == 0 {
		t.Fatal("empty push body")
	}
	jwtPart := authHdr[strings.Index(authHdr, "t=")+2:]
	if i := strings.Index(jwtPart, " "); i >= 0 {
		jwtPart = jwtPart[:i]
	}
	if strings.Count(jwtPart, ".") != 2 {
		t.Fatalf("VAPID JWT = %q, want a three-part JWS", jwtPart)
	}

	// A 410 from the push service prunes the dead subscription.
	mu.Lock()
	code = 410
	mu.Unlock()
	a.pushSend("admin", "again", "body", "c1", "turn")
	waitPush(t, func() bool {
		subs, _ := us.ListPushSubscriptions()
		return len(subs) == 0
	})
}

// TestPushSendUnconfiguredNoop — no keys = no sends, no panic.
func TestPushSendUnconfiguredNoop(t *testing.T) {
	a, _ := testAppWithPush(t)
	a.cfg.Push = config.Push{} // wipe the keys
	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer fake.Close()

	us, _ := a.ag.UserStoreFor("admin")
	_ = us.AddPushSubscription(storeSub(fake.URL + "/sub1"))
	a.pushSend("admin", "T", "B", "c1", "turn")
	time.Sleep(150 * time.Millisecond) // let a (buggy) send have time to arrive
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("unconfigured push sent %d times", hits)
	}
}

// TestPushTurnComposes — the notification subject + body: the label is
// purpose || title (the UI's rule), the body is the first line of the
// newest assistant reply, capped.
func TestPushTurnComposes(t *testing.T) {
	a, _ := testAppWithPush(t)
	us, _ := a.ag.UserStoreFor("admin")
	conv, err := us.CreateConversation("My Title", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := us.AppendMessage(conv.ID, "assistant", "First line here.\nSecond line.", "", "", nil); err != nil {
		t.Fatal(err)
	}

	// Pre-purpose: the title is the label.
	label, first := a.convNotification("admin", conv.ID)
	if label != "My Title" || first != "First line here." {
		t.Fatalf("pre-purpose = %q / %q", label, first)
	}
	// The purpose supersedes the title (the conversation-label rule).
	if err := us.SetConversationSummary(conv.ID, "The rolling purpose", ""); err != nil {
		t.Fatal(err)
	}
	label, _ = a.convNotification("admin", conv.ID)
	if label != "The rolling purpose" {
		t.Fatalf("label = %q, want the purpose", label)
	}
	// A deleted conversation → nothing to notify about.
	if err := us.DeleteConversation(conv.ID); err == nil {
		label, _ = a.convNotification("admin", conv.ID)
		if label != "" {
			t.Fatalf("deleted conversation label = %q, want empty", label)
		}
	}
}

// TestAdminPushKeysEndpoint — the server-side VAPID keygen: keys land in
// the config, only the public one comes back over the wire, and the
// admin config read masks the private one.
func TestAdminPushKeysEndpoint(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	a.cfg.Auth.SessionSecret = "test-session-secret-0123456789"
	a.cfg.Models = []config.Model{{
		ID: "m1", Provider: "openai", BaseURL: "http://127.0.0.1:1/v1",
		APIKey: "k", ModelID: "m1", Enabled: true,
	}}
	if err := config.Save(a.dataDir, a.cfg); err != nil {
		t.Fatal(err)
	}
	sm := setup.NewSidecarManager(nil, nil)
	a.edit = setup.New(a.dataDir, "127.0.0.1:0", 0, srv, auth.NewInternalProvider(a.srv, time.Hour), sm)
	ts := httptest.NewServer(a)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/push/keys", nil)
	req.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-test"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK        bool   `json:"ok"`
		PublicKey string `json:"public_key"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !out.OK || out.PublicKey == "" {
		t.Fatalf("keygen = %d %+v", res.StatusCode, out)
	}
	if !a.cfg.Push.Configured() {
		t.Fatal("the keygen did not land in the live config")
	}
	if a.cfg.Push.VAPIDPublic != out.PublicKey {
		t.Fatal("the returned public key does not match the stored one")
	}

	// The masked read: has_vapid_private, never the raw secret.
	cfgReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/config", nil)
	cfgReq.AddCookie(&http.Cookie{Name: "bevin_session", Value: "tok-test"})
	cfgRes, err := http.DefaultClient.Do(cfgReq)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(cfgRes.Body)
	cfgRes.Body.Close()
	if strings.Contains(string(raw), `"vapid_private":`) {
		t.Fatalf("the config read leaks the private key: %s", raw)
	}
	if !strings.Contains(string(raw), "has_vapid_private") {
		t.Fatalf("the config read is missing has_vapid_private: %s", raw)
	}
}
