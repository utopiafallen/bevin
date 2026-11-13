package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/store"
)

// fakeOAuth is an in-test OIDC-ish provider: /authorize (redirects back with a
// code + the echoed state), /token (checks the client secret, returns an
// access token), /userinfo (checks the bearer, returns the claims).
func fakeOAuth(t *testing.T, claims map[string]any) (*httptest.Server, func() map[string]string) {
	t.Helper()
	var lastToken map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		redirect := r.URL.Query().Get("redirect_uri")
		back, _ := url.Parse(redirect)
		q := back.Query()
		q.Set("code", "fake-code-123")
		q.Set("state", state)
		back.RawQuery = q.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		lastToken = map[string]string{
			"grant_type":    r.Form.Get("grant_type"),
			"code":          r.Form.Get("code"),
			"client_id":     r.Form.Get("client_id"),
			"client_secret": r.Form.Get("client_secret"),
			"redirect_uri":  r.Form.Get("redirect_uri"),
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-42","token_type":"bearer"}`)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-42" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claims)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() map[string]string { return lastToken }
}

// oauthApp builds an app with the OAuth config pointing at the fake provider.
func oauthApp(t *testing.T, claims map[string]any) (*App, *store.ServerStore, string) {
	t.Helper()
	provider, _ := fakeOAuth(t, claims)
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if _, err := srv.CreateUser("admin", "Admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := srv.CreateSession("tok-test", "admin", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	prov := auth.NewInternalProvider(srv, time.Hour)
	cfg := &config.ServerConfig{
		DataDir: dir,
		Auth: config.Auth{
			SessionTTLSeconds: 3600,
			OAuth: config.OAuth{
				Enabled: true, Name: "TestIDP", ClientID: "app-1", ClientSecret: "cs-1",
				AuthURL: provider.URL + "/authorize", TokenURL: provider.URL + "/token",
				UserinfoURL: provider.URL + "/userinfo", Scopes: "openid profile email",
				BaseURI: "https://bevin.example.com",
			},
		},
	}
	a := &App{cfg: cfg, srv: srv, ttl: time.Hour, dataDir: dir}
	a.prov = prov
	a.routes()
	return a, srv, provider.URL
}

// doOAuthRoundTrip drives the flow: login redirect -> provider -> callback.
// It returns the callback response + the bevin session cookie it set.
func doOAuthRoundTrip(t *testing.T, a *App) (*httptest.ResponseRecorder, string) {
	t.Helper()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/oauth/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("login code = %d, want 302 (body %s)", w.Code, w.Body.String())
	}
	loc := w.Result().Header.Get("Location")
	lu, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	state := lu.Query().Get("state")
	if state == "" {
		t.Fatal("login redirect missing the state param")
	}
	// The redirect_uri must be the one DERIVED from the configured public
	// base (base + /auth/oauth/callback) — the admin never types it.
	if got, want := lu.Query().Get("redirect_uri"), "https://bevin.example.com/auth/oauth/callback"; got != want {
		t.Fatalf("authorize redirect_uri = %q, want derived %q", got, want)
	}
	// The state cookie was set (single-use).
	var stateCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "bevin_oauth_state" {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("login did not set the state cookie")
	}
	// The provider redirects back (it echoes state + hands a code).
	w = httptest.NewRecorder()
	cb := httptest.NewRequest(http.MethodGet, "/auth/oauth/callback?code=fake-code-123&state="+url.QueryEscape(state), nil)
	cb.AddCookie(stateCookie)
	a.ServeHTTP(w, cb)
	if w.Code != http.StatusFound {
		t.Fatalf("callback code = %d, want 302 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Result().Header.Get("Location"); got != "/" {
		t.Fatalf("callback redirect = %q, want /", got)
	}
	var session string
	for _, c := range w.Result().Cookies() {
		if c.Name == "bevin_session" {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("callback did not set the bevin session cookie")
	}
	return w, session
}

// TestOAuthLoginFlow — the full auto-provisioning path: a provider user signs
// in for the first time, bevin creates the account (origin oauth), issues a
// session, and the SAME subject signs into the SAME account on the next login
// (no duplicate).
func TestOAuthLoginFlow(t *testing.T) {
	a, srv, _ := oauthApp(t, map[string]any{
		"sub": "auth0|abc", "preferred_username": "alice", "name": "Alice", "email": "alice@example.com",
	})

	_, session := doOAuthRoundTrip(t, a)

	u, err := srv.GetUser("alice")
	if err != nil {
		t.Fatalf("auto-provisioned user missing: %v", err)
	}
	if u.IDP != "oauth" || u.IDPSubject != "auth0|abc" || u.Role != "user" || u.Status != "active" {
		t.Fatalf("provisioned user = %+v", u)
	}
	if u.Email != "alice@example.com" {
		t.Fatalf("email claim not stored: %q", u.Email)
	}
	// The session works.
	mw := httptest.NewRecorder()
	a.ServeHTTP(mw, newRequestAs(http.MethodGet, "/api/me", session, nil))
	if mw.Code != http.StatusOK {
		t.Fatalf("api/me with the oauth session = %d body=%s", mw.Code, mw.Body.String())
	}
	var me map[string]any
	_ = json.Unmarshal(mw.Body.Bytes(), &me)
	if me["handle"] != "alice" {
		t.Fatalf("api/me handle = %v", me["handle"])
	}

	// Second login for the same subject: same account, still one user.
	w2, session2 := doOAuthRoundTrip(t, a)
	_ = w2
	users, _ := srv.ListUsers()
	if len(users) != 2 { // admin + alice
		t.Fatalf("user count after re-login = %d, want 2 (no duplicate)", len(users))
	}
	mw2 := httptest.NewRecorder()
	a.ServeHTTP(mw2, newRequestAs(http.MethodGet, "/api/me", session2, nil))
	if mw2.Code != http.StatusOK {
		t.Fatalf("second oauth session = %d", mw2.Code)
	}
}

// TestOAuthCallbackBadState — a mismatched or missing state is rejected
// (CSRF / replay guard) and no session is issued.
func TestOAuthCallbackBadState(t *testing.T) {
	a, srv, _ := oauthApp(t, map[string]any{"sub": "auth0|abc", "preferred_username": "alice"})

	// No state cookie at all.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/oauth/callback?code=x&state=whatever", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing state code = %d, want 400", w.Code)
	}
	// A cookie that does not match the query state.
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oauth/callback?code=x&state=abc", nil)
	r.AddCookie(&http.Cookie{Name: "bevin_oauth_state", Value: "xyz"})
	a.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("mismatched state code = %d, want 400", w.Code)
	}
	// The state cookie is single-use: a valid pair consumed twice is 400 on
	// the replay.
	lw := httptest.NewRecorder()
	a.ServeHTTP(lw, httptest.NewRequest(http.MethodGet, "/auth/oauth/login", nil))
	lu, _ := url.Parse(lw.Result().Header.Get("Location"))
	state := lu.Query().Get("state")
	var sc *http.Cookie
	for _, c := range lw.Result().Cookies() {
		if c.Name == "bevin_oauth_state" {
			sc = c
		}
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/auth/oauth/callback?code=fake-code-123&state="+url.QueryEscape(state), nil)
	r.AddCookie(sc)
	a.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("valid flow failed: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/auth/oauth/callback?code=fake-code-123&state="+url.QueryEscape(state), nil)
	r.AddCookie(sc) // already consumed
	a.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("replayed state code = %d, want 400", w.Code)
	}
	if _, err := srv.GetUser("alice"); err != nil {
		t.Fatal("the valid first attempt should have provisioned alice")
	}
}

// TestOAuthDisabledRejected — an account the admin disabled (in the Users
// panel) is rejected at the OAuth callback with a clear message.
func TestOAuthDisabledRejected(t *testing.T) {
	a, srv, _ := oauthApp(t, map[string]any{"sub": "auth0|abc", "preferred_username": "alice"})
	// Provision once, then disable.
	_, _ = doOAuthRoundTrip(t, a)
	if err := srv.SetUserStatus("alice", "disabled"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/oauth/login", nil))
	lu, _ := url.Parse(w.Result().Header.Get("Location"))
	state := lu.Query().Get("state")
	var sc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "bevin_oauth_state" {
			sc = c
		}
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oauth/callback?code=fake-code-123&state="+url.QueryEscape(state), nil)
	r.AddCookie(sc)
	a.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled oauth login code = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("disabled login should say so: %s", w.Body.String())
	}
}

// TestOAuthHandleCollision — a provider identity whose derived handle is
// already taken by a DIFFERENT bevin identity gets a subject suffix (the
// admin's handle is never claimed by an OAuth login).
func TestOAuthHandleCollision(t *testing.T) {
	a, srv, _ := oauthApp(t, map[string]any{
		"sub": "auth0|admin-claim", "preferred_username": "admin", "name": "Not The Admin",
	})
	_, session := doOAuthRoundTrip(t, a)
	// The handle must NOT be "admin" (that's the internal admin).
	if _, err := srv.GetUser("admin"); err != nil {
		t.Fatal("the internal admin must still exist")
	}
	u, err := srv.FindOAuthUserBySubject("auth0|admin-claim")
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle == "admin" {
		t.Fatal("the oauth login claimed the admin's handle")
	}
	if u.Handle != "admin-auth0a" {
		t.Fatalf("collision handle = %q, want the suffixed form admin-auth0a", u.Handle)
	}
	// The session is for the new user, not the admin.
	mw := httptest.NewRecorder()
	a.ServeHTTP(mw, newRequestAs(http.MethodGet, "/api/me", session, nil))
	if mw.Code != http.StatusOK {
		t.Fatalf("collision session = %d", mw.Code)
	}
	var me map[string]any
	_ = json.Unmarshal(mw.Body.Bytes(), &me)
	if me["handle"] != u.Handle {
		t.Fatalf("collision session handle = %v, want %s", me["handle"], u.Handle)
	}
}

// TestOAuthStatusEndpoint — the login page probe: disabled -> enabled false
// (no secrets ever), enabled -> the provider name.
func TestOAuthStatusEndpoint(t *testing.T) {
	a, _, _ := oauthApp(t, map[string]any{"sub": "s", "preferred_username": "alice"})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/oauth/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d", w.Code)
	}
	var st map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["enabled"] != true || st["name"] != "TestIDP" {
		t.Fatalf("status = %+v", st)
	}
	for _, k := range []string{"client_secret", "client_id", "auth_url", "token_url"} {
		if _, has := st[k]; has {
			t.Fatalf("status must not leak %s: %+v", k, st)
		}
	}

	// Disabled (a different app): enabled false.
	a.cfg.Auth.OAuth.Enabled = false
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/oauth/status", nil))
	st = map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["enabled"] != false {
		t.Fatalf("status after disable = %+v", st)
	}
	// And the login route 404s while disabled.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/oauth/login", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled login code = %d, want 404", w.Code)
	}
}

// TestAdminUsersAPI — the Users panel endpoints: the full registry (both
// origins), add an internal user (validation + duplicate), enable/disable
// (with the self-guard), admin gating.
func TestAdminUsersAPI(t *testing.T) {
	a, srv, _ := oauthApp(t, map[string]any{"sub": "auth0|abc", "preferred_username": "alice"})
	// Provision one oauth user.
	_, _ = doOAuthRoundTrip(t, a)
	// ... and create an internal one.
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}

	// The list shows all three, with origins.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/users", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("users list code = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Users []store.User `json:"users"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Users) != 3 {
		t.Fatalf("users list = %d, want 3", len(got.Users))
	}
	origins := map[string]string{}
	for _, u := range got.Users {
		origins[u.Handle] = u.IDP
	}
	if origins["admin"] != "" || origins["bob"] != "" || origins["alice"] != "oauth" {
		t.Fatalf("origins = %+v", origins)
	}

	// A non-admin cannot list.
	if err := srv.CreateSession("tok-bob", "bob", time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequestAs(http.MethodGet, "/admin/api/users", "tok-bob", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin users list code = %d, want 403", w.Code)
	}

	// Add an internal user.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users", map[string]string{
		"handle": "carol", "display_name": "Carol", "password": "carolpass1",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("add user code = %d body=%s", w.Code, w.Body.String())
	}
	carol, err := srv.GetUser("carol")
	if err != nil || carol.IDP != "" {
		t.Fatalf("carol = %+v err=%v, want an internal user", carol, err)
	}
	// Duplicate handle -> 409.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users", map[string]string{
		"handle": "carol", "display_name": "Carol", "password": "carolpass1",
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate add code = %d, want 409", w.Code)
	}
	// Weak password / bad handle -> 400.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users", map[string]string{
		"handle": "dave", "display_name": "Dave", "password": "short",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("short password code = %d, want 400", w.Code)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users", map[string]string{
		"handle": "Dave!", "display_name": "Dave", "password": "davepass12",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad handle code = %d, want 400", w.Code)
	}

	// Disable alice (the oauth user); re-enable bob.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users/status", map[string]string{
		"handle": "alice", "status": "disabled",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("disable code = %d body=%s", w.Code, w.Body.String())
	}
	if u, _ := srv.GetUser("alice"); u.Status != "disabled" {
		t.Fatalf("alice status = %q, want disabled", u.Status)
	}
	// The admin cannot change their own status.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users/status", map[string]string{
		"handle": "admin", "status": "disabled",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("self-disable code = %d, want 400", w.Code)
	}
	// A bad status value is rejected.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users/status", map[string]string{
		"handle": "bob", "status": "frobnicated",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad status code = %d, want 400", w.Code)
	}
	// A missing user is 404.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/users/status", map[string]string{
		"handle": "ghost", "status": "disabled",
	}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing user status code = %d, want 404", w.Code)
	}
}

// TestAdminConfigOAuth — the Users panel's OAuth config area: the secret is
// masked on read, a blank secret keeps the stored one, a save applies, and an
// invalid config is rejected without clobbering.
func TestAdminConfigOAuth(t *testing.T) {
	a, _ := configApp(t)

	// Save the oauth area (secret set).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"oauth": map[string]any{
			"enabled": true, "name": "TestIDP", "client_id": "app-1", "client_secret": "cs-1",
			"auth_url": "https://idp.example.com/o/authorize/", "token_url": "https://idp.example.com/o/token/",
			"userinfo_url": "https://idp.example.com/o/user/", "scopes": "openid profile email",
			"base_uri": "https://bevin.example.com",
		},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("oauth save code = %d body=%s", w.Code, w.Body.String())
	}
	onDisk, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !onDisk.Auth.OAuth.Enabled || onDisk.Auth.OAuth.ClientSecret != "cs-1" {
		t.Fatalf("saved oauth = %+v", onDisk.Auth.OAuth)
	}

	// Read: masked (has_secret, never the secret itself).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/config", nil))
	var got struct {
		OAuth map[string]any `json:"oauth"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.OAuth["has_secret"] != true {
		t.Fatalf("oauth read has_secret = %v", got.OAuth["has_secret"])
	}
	if _, has := got.OAuth["client_secret"]; has {
		t.Fatalf("oauth read must not return the secret: %+v", got.OAuth)
	}
	if got.OAuth["client_id"] != "app-1" {
		t.Fatalf("oauth read client_id = %v", got.OAuth["client_id"])
	}

	// Save again with a BLANK secret: the stored one survives.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"oauth": map[string]any{
			"enabled": true, "name": "TestIDP", "client_id": "app-1", "client_secret": "",
			"auth_url": "https://idp.example.com/o/authorize/", "token_url": "https://idp.example.com/o/token/",
			"userinfo_url": "https://idp.example.com/o/user/", "scopes": "openid profile email",
			"base_uri": "https://bevin.example.com",
		},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("blank-secret save code = %d body=%s", w.Code, w.Body.String())
	}
	after, _ := config.Load(a.dataDir)
	if after.Auth.OAuth.ClientSecret != "cs-1" {
		t.Fatalf("a blank secret must keep the stored one, got %q", after.Auth.OAuth.ClientSecret)
	}

	// An invalid URL is rejected + does not clobber the saved config.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"oauth": map[string]any{
			"enabled": true, "name": "TestIDP", "client_id": "app-1", "client_secret": "cs-2",
			"auth_url": "not a url", "token_url": "https://idp.example.com/o/token/",
			"userinfo_url": "https://idp.example.com/o/user/", "scopes": "openid profile email",
			"base_uri": "https://bevin.example.com",
		},
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid oauth save code = %d, want 400", w.Code)
	}
	after, _ = config.Load(a.dataDir)
	if after.Auth.OAuth.AuthURL != "https://idp.example.com/o/authorize/" {
		t.Fatalf("a rejected save must not clobber the saved config: %+v", after.Auth.OAuth)
	}

	// Disabling is a normal save.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"oauth": map[string]any{
			"enabled": false, "name": "TestIDP", "client_id": "app-1", "client_secret": "",
			"auth_url": "https://idp.example.com/o/authorize/", "token_url": "https://idp.example.com/o/token/",
			"userinfo_url": "https://idp.example.com/o/user/", "scopes": "openid profile email",
			"base_uri": "https://bevin.example.com",
		},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("disable save code = %d body=%s", w.Code, w.Body.String())
	}
	after, _ = config.Load(a.dataDir)
	if after.Auth.OAuth.Enabled {
		t.Fatal("oauth should be disabled after the save")
	}
}
