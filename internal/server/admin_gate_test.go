package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAdminIndexRedirect — the admin is NOT a regular user: requesting the
// user chat UI (GET / or /index.html) with an admin session redirects to the
// dashboard. Unauthenticated visitors still get the login page, regular users
// get the chat UI, and static assets are never redirected (the dashboard
// pages load them under the admin session).
func TestAdminIndexRedirect(t *testing.T) {
	a, _ := configApp(t)
	if _, err := a.srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated: the login page (200, no redirect).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous GET / = %d, want 200", w.Code)
	}

	// Admin: redirected to the dashboard (both index spellings).
	for _, path := range []string{"/", "/index.html"} {
		w = httptest.NewRecorder()
		a.ServeHTTP(w, newRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("admin GET %s = %d, want 303", path, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/admin" {
			t.Fatalf("admin GET %s Location = %q, want /admin", path, loc)
		}
	}

	// Regular user: the chat UI (200, no redirect).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/", "tok-bob", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("user GET / = %d, want 200", w.Code)
	}

	// Static assets: served as-is even for the admin (no redirect).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/config-forms.js", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET /config-forms.js = %d, want 200 (assets must not be redirected)", w.Code)
	}
}

// TestChatAPIAdminForbidden — the chat endpoints are the user interaction
// surface: an admin session is rejected (pointing at the dashboard), a
// regular user's session passes.
func TestChatAPIAdminForbidden(t *testing.T) {
	a, _ := configApp(t)
	if _, err := a.srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}

	// Admin: forbidden + the error names the dashboard.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/api/conversations", map[string]string{"title": "t"}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("admin POST /api/conversations = %d, want 403: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dashboard") {
		t.Fatalf("admin rejection should point at the dashboard: %s", w.Body.String())
	}

	// Regular user: the conversation is created.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodPost, "/api/conversations", "tok-bob", map[string]string{"title": "t"}))
	if w.Code != http.StatusOK {
		t.Fatalf("user POST /api/conversations = %d, want 200: %s", w.Code, w.Body.String())
	}
}
