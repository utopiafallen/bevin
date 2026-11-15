package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// TestSandboxAPIDisabled: with [sandbox] off (the default — and testApp
// never builds a backend) every sandbox endpoint answers 503 + the
// runtime detail, and an unauthenticated request never reaches the gate.
// The sandbox API is user-facing (a regular user's OWN container), so the
// test session is a regular user — the admin account is rejected by the
// chat gate before the sandbox gate runs.
func TestSandboxAPIDisabled(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	if _, err := srv.CreateUser("alice", "Alice", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-alice", "alice", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	reqAs := func(method, target string, body any) *http.Request {
		return newRequestAs(method, target, "tok-alice", body)
	}
	for _, tc := range []struct {
		method, target string
	}{
		{http.MethodGet, "/api/sandbox"},
		{http.MethodPost, "/api/sandbox/ensure"},
		{http.MethodPost, "/api/sandbox/exec"},
		{http.MethodPost, "/api/sandbox/stop"},
		{http.MethodDelete, "/api/sandbox"},
	} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, reqAs(tc.method, tc.target, map[string]any{}))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503: %s", tc.method, tc.target, w.Code, w.Body.String())
		}
	}
	// Unauthenticated: the chat gate rejects before the sandbox gate.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/sandbox", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", w.Code)
	}
}
