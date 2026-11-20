package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// postLogin posts a login and returns the recorder. ip is the synthetic
// X-Forwarded-For (so the per-IP cap is controlled independently).
func postLogin(t *testing.T, a *App, ip, handle, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"handle": handle, "password": password})
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	if ip != "" {
		r.Header.Set("X-Forwarded-For", ip)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

// TestLoginRateLimitHandle: more than defaultLoginLimit attempts for one
// handle (spread across IPs so the per-IP cap stays silent) throttle —
// 429 with a Retry-After, before any credential check.
func TestLoginRateLimitHandle(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	for i := 0; i < defaultLoginLimit; i++ {
		w := postLogin(t, a, fmt.Sprintf("10.0.%d.%d", i/256, i%256), "admin", "wrong")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code = %d, want 401", i+1, w.Code)
		}
	}
	w := postLogin(t, a, "10.9.9.9", "admin", "wrong")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: code = %d, want 429 (handle cap)", defaultLoginLimit+1, w.Code)
	}
	ra := w.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("429 missing Retry-After")
	}
	if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Fatalf("Retry-After = %q, want >= 1s", ra)
	}
	// A throttled handle stays throttled even with a good-looking request.
	if w := postLogin(t, a, "10.9.9.8", "admin", "whatever"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled handle: code = %d, want 429", w.Code)
	}
}

// TestLoginRateLimitIP: one IP spread across distinct handles hits the
// per-IP cap.
func TestLoginRateLimitIP(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	for i := 0; i < defaultLoginLimit; i++ {
		w := postLogin(t, a, "10.1.1.1", fmt.Sprintf("user%d", i), "wrong")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code = %d, want 401", i+1, w.Code)
		}
	}
	w := postLogin(t, a, "10.1.1.1", "user11", "wrong")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d from the same IP: code = %d, want 429 (IP cap)", defaultLoginLimit+1, w.Code)
	}
	// A different IP for the same fresh handle is NOT throttled.
	if w := postLogin(t, a, "10.2.2.2", "user11", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("fresh IP: code = %d, want 401", w.Code)
	}
}

// TestLoginRateLimitResetOnSuccess: a successful login clears the handle's
// failure counter, so earlier typos don't count toward the cap.
func TestLoginRateLimitResetOnSuccess(t *testing.T) {
	a, _ := testApp(t, config.Email{})
	if _, err := a.prov.CreateUserProvisioned(t.Context(), "carol", "Carol", "user", "carol-pass"); err != nil {
		t.Fatal(err)
	}
	// Two typos, then the right password (distinct IPs; only the handle
	// counter matters here).
	if w := postLogin(t, a, "10.3.0.1", "carol", "typo"); w.Code != http.StatusUnauthorized {
		t.Fatalf("attempt 1: code = %d, want 401", w.Code)
	}
	if w := postLogin(t, a, "10.3.0.2", "carol", "typo2"); w.Code != http.StatusUnauthorized {
		t.Fatalf("attempt 2: code = %d, want 401", w.Code)
	}
	w := postLogin(t, a, "10.3.0.3", "carol", "carol-pass")
	if w.Code != http.StatusOK {
		t.Fatalf("login: code = %d, want 200", w.Code)
	}
	// After the reset the full window is available again: all of the cap's
	// worth of failures go through...
	for i := 0; i < defaultLoginLimit; i++ {
		if w := postLogin(t, a, fmt.Sprintf("10.4.%d.%d", i/256, i%256), "carol", "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: code = %d, want 401", i+1, w.Code)
		}
	}
	// ...and the one past the cap throttles.
	if w := postLogin(t, a, "10.4.9.9", "carol", "wrong"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("post-reset attempt %d: code = %d, want 429", defaultLoginLimit+1, w.Code)
	}
}

// TestLoginWindowExpires: a throttled handle is usable again once the window
// has passed (the limiter's own window, not the 5-minute default — the
// limiter is built by the app lazily, so exercise the unit directly).
func TestLoginWindowExpires(t *testing.T) {
	l := newLoginLimiter(3, 50*time.Millisecond)
	for i := 0; i < 3; i++ {
		if ok, _ := l.attempt("1.1.1.1", "h"); !ok {
			t.Fatalf("attempt %d: not allowed", i+1)
		}
	}
	if ok, _ := l.attempt("2.2.2.2", "h"); ok {
		t.Fatal("attempt 4: allowed, want throttled")
	}
	time.Sleep(60 * time.Millisecond)
	if ok, _ := l.attempt("2.2.2.2", "h"); !ok {
		t.Fatal("after the window: still throttled")
	}
}
