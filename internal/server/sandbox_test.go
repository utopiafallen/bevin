package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/sandbox"
	"github.com/utopiafallen/bevin/internal/store"
)

// TestSandboxAPINoBackend: testApp never builds a sandbox backend (it
// constructs the App directly, skipping buildSandbox — in production the
// backend is only nil on platforms with no container backend, e.g.
// darwin), so every sandbox endpoint answers 503 for ANY role (the
// envGate admits both users and the admin — the admin is the one who
// diagnoses a broken runtime, and only it sees the runtime detail). An
// unauthenticated request never reaches the gate.
func TestSandboxAPINoBackend(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	if _, err := srv.CreateUser("alice", "Alice", "user"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-alice", "alice", time.Hour, "test"); err != nil {
		t.Fatal(err)
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
		a.ServeHTTP(w, newRequestAs(tc.method, tc.target, "tok-alice", map[string]any{}))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503: %s", tc.method, tc.target, w.Code, w.Body.String())
		}
	}
	// The admin is admitted by the envGate and gets the runtime detail
	// (key presence — the value is host-dependent).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/api/sandbox", map[string]any{}))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin = %d, want 503: %s", w.Code, w.Body.String())
	}
	var adminRes map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &adminRes); err != nil {
		t.Fatal(err)
	}
	if _, has := adminRes["runtime"]; !has {
		t.Fatalf("admin response must carry the runtime detail: %s", w.Body.String())
	}
	// Unauthenticated: the gate rejects before anything else.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/sandbox", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", w.Code)
	}
}

// TestEnvErrVisibility — the environment errors hide the container
// internals from regular users (the containerization is a hidden detail):
// a user gets only the neutral wording, the admin gets the same error +
// the runtime detail + the raw infrastructure error.
func TestEnvErrVisibility(t *testing.T) {
	rt := &sandbox.Runtime{OK: false, Detail: "nspawn binary not found"}
	raw := errors.New("machinectl copy-from failed: exit 1")

	w := httptest.NewRecorder()
	envError(w, http.StatusPreconditionFailed, &store.User{Handle: "bob", Role: "user"}, "your environment is not ready yet", rt, raw)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("user code = %d, want 412", w.Code)
	}
	var userRes map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &userRes); err != nil {
		t.Fatal(err)
	}
	if userRes["error"] != "your environment is not ready yet" {
		t.Fatalf("user error = %v, want the neutral wording", userRes["error"])
	}
	if _, has := userRes["runtime"]; has {
		t.Fatal("regular users must not see the runtime detail")
	}
	if body := w.Body.String(); strings.Contains(body, "nspawn") || strings.Contains(body, "machinectl") {
		t.Fatalf("user body leaks container internals: %s", body)
	}

	w = httptest.NewRecorder()
	envError(w, http.StatusPreconditionFailed, &store.User{Handle: "admin", Role: "admin"}, "your environment is not ready yet", rt, raw)
	var adminRes struct {
		Error   string `json:"error"`
		Runtime struct {
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"runtime"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &adminRes); err != nil {
		t.Fatal(err)
	}
	if adminRes.Runtime.Detail != "nspawn binary not found" {
		t.Fatalf("admin runtime detail = %q, want the raw detail", adminRes.Runtime.Detail)
	}
	if adminRes.Error != "your environment is not ready yet" {
		t.Fatalf("admin error = %q, want the neutral wording", adminRes.Error)
	}
	if adminRes.Detail != "machinectl copy-from failed: exit 1" {
		t.Fatalf("admin detail = %q, want the raw infrastructure error", adminRes.Detail)
	}
}
