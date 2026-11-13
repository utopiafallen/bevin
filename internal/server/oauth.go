package server

// The external-IdP (OAuth) tier: the authorization-code flow alongside the
// server's own IdP. The server ALWAYS accepts its own logins; when the OAuth
// config is enabled, users enabled in the provider are auto-provisioned on
// first login (no explicit admin add) and sign in through the provider.
//
// Flow: /auth/oauth/login mints a one-time state (cookie) + redirects to the
// provider; the provider redirects back to /auth/oauth/callback with a code;
// we exchange the code for a token, fetch the user-info claims, map the
// subject to a bevin user (creating it on first sight), and issue a normal
// bevin session. The internal account and the OAuth mapping never collide:
// the admin is always self-IdP, and an OAuth login only ever matches users
// by the provider's stable subject claim.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/setup"
	"github.com/utopiafallen/bevin/internal/store"
)

const (
	oauthStateCookie = "bevin_oauth_state"
	oauthStateTTL    = 10 * time.Minute
)

// issueOAuthState mints a fresh 256-bit state, tracks it server-side for
// single-use (10 min TTL) and returns it. The tracking map is what makes the
// state single-use: deleting the client cookie alone would not stop a
// replayed (cookie, state) pair.
func (a *App) issueOAuthState() string {
	a.oauthMu.Lock()
	defer a.oauthMu.Unlock()
	if a.oauthStates == nil {
		a.oauthStates = make(map[string]time.Time)
	}
	now := time.Now()
	for k, exp := range a.oauthStates {
		if now.After(exp) {
			delete(a.oauthStates, k)
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand read failures are not recoverable
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	a.oauthStates[s] = now.Add(oauthStateTTL)
	return s
}

// consumeOAuthState removes a state from the registry (single use) and
// reports whether it was live (known + unexpired).
func (a *App) consumeOAuthState(state string) bool {
	a.oauthMu.Lock()
	defer a.oauthMu.Unlock()
	exp, ok := a.oauthStates[state]
	if !ok {
		return false
	}
	delete(a.oauthStates, state)
	return time.Now().Before(exp)
}

// apiOAuthStatus (public, pre-login) tells the login page whether external
// sign-in is enabled and what to call the provider. No secrets.
func (a *App) apiOAuthStatus(w http.ResponseWriter, r *http.Request) {
	o := a.liveCfg().Auth.OAuth
	writeJSON(w, http.StatusOK, map[string]any{"enabled": o.Enabled, "name": o.Name})
}

// oauthLogin starts the flow: mint the one-time state (HttpOnly cookie +
// tracked server-side for single-use, 10 min) and redirect to the provider's
// authorization endpoint.
func (a *App) oauthLogin(w http.ResponseWriter, r *http.Request) {
	o := a.liveCfg().Auth.OAuth
	if !o.Enabled {
		http.Error(w, "external sign-in is not enabled", http.StatusNotFound)
		return
	}
	if a.liveCfg().OAuthRedirectURI() == "" {
		http.Error(w, "external sign-in is misconfigured — set the public base URI on the Server page", http.StatusMisdirectedRequest)
		return
	}
	state := a.issueOAuthState()
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthStateTTL.Seconds()),
	})
	u, err := url.Parse(o.AuthURL)
	if err != nil {
		http.Error(w, "misconfigured authorization URL", http.StatusMisdirectedRequest)
		return
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", o.ClientID)
	q.Set("redirect_uri", a.liveCfg().OAuthRedirectURI())
	if o.Scopes != "" {
		q.Set("scope", o.Scopes)
	}
	q.Set("state", state)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// oauthCallback completes the flow: verify the one-time state, exchange the
// code, fetch the claims, resolve (or auto-provision) the user, issue the
// session cookie, and redirect to the chat UI.
func (a *App) oauthCallback(w http.ResponseWriter, r *http.Request) {
	o := a.liveCfg().Auth.OAuth
	if !o.Enabled {
		http.Error(w, "external sign-in is not enabled", http.StatusNotFound)
		return
	}
	// The state is SINGLE-USE: the cookie must carry the query state AND the
	// state must still be live in the server-side registry (deleted on first
	// use), so a replayed (cookie, state) pair can never sign in twice.
	var storedState string
	if c, err := r.Cookie(oauthStateCookie); err == nil {
		storedState = c.Value
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})
	state := r.URL.Query().Get("state")
	if storedState == "" || state == "" || subtle.ConstantTimeCompare([]byte(storedState), []byte(state)) != 1 || !a.consumeOAuthState(state) {
		http.Error(w, "state mismatch — start the sign-in again", http.StatusBadRequest)
		return
	}
	if perr := r.URL.Query().Get("error"); perr != "" {
		http.Error(w, "the provider denied the sign-in: "+perr, http.StatusForbidden)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}
	view := cfgOAuthView{
		clientID:     o.ClientID,
		clientSecret: o.ClientSecret,
		tokenURL:     o.TokenURL,
		userinfoURL:  o.UserinfoURL,
		redirectURI:  a.liveCfg().OAuthRedirectURI(),
	}
	accessToken, err := a.oauthExchangeToken(r.Context(), view, code)
	if err != nil {
		log.Printf("bevin: oauth: token exchange: %v", err)
		http.Error(w, "sign-in failed at the token exchange — check the OAuth settings", http.StatusBadGateway)
		return
	}
	claims, err := a.oauthUserinfo(r.Context(), view, accessToken)
	if err != nil {
		log.Printf("bevin: oauth: user-info: %v", err)
		http.Error(w, "sign-in failed while fetching your identity — check the OAuth settings", http.StatusBadGateway)
		return
	}
	subject, _ := claims["sub"].(string)
	if strings.TrimSpace(subject) == "" {
		http.Error(w, "the provider returned no subject (sub claim) — the app needs the openid scope", http.StatusBadGateway)
		return
	}

	// Resolve the provider user to a bevin account: by stable subject. A
	// first login auto-provisions (the provider is the allowlist — the user
	// is enabled THERE, not here); a disabled account is rejected.
	u, err := a.srv.FindOAuthUserBySubject(subject)
	if errors.Is(err, store.ErrUserNotFound) {
		handle, displayName, perr := a.oauthHandle(claims, subject)
		if perr != nil {
			log.Printf("bevin: oauth: provisioning: %v", perr)
			http.Error(w, perr.Error(), http.StatusConflict)
			return
		}
		u, err = a.srv.CreateOAuthUser(handle, displayName, subject)
		if err != nil {
			log.Printf("bevin: oauth: create user: %v", err)
			http.Error(w, "could not create the account — contact your administrator", http.StatusInternalServerError)
			return
		}
		log.Printf("bevin: oauth: auto-provisioned %s (subject %s)", handle, subject)
	} else if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if u.Status != "active" {
		http.Error(w, "this account has been disabled — contact your administrator", http.StatusForbidden)
		return
	}
	// Keep the email current when the provider supplies one.
	if email, _ := claims["email"].(string); strings.TrimSpace(email) != "" && email != u.Email {
		if err := a.srv.SetUserEmail(u.Handle, strings.TrimSpace(email)); err != nil {
			log.Printf("bevin: oauth: set email for %s: %v", u.Handle, err)
		}
	}

	token, err := auth.NewToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := a.srv.CreateSession(token, u.Handle, a.ttl, r.UserAgent()); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, token, a.ttl)
	log.Printf("bevin: oauth: login %s (%s) from %s", u.Handle, clientIP(r), o.Name)
	http.Redirect(w, r, "/", http.StatusFound)
}

// oauthExchangeToken performs the authorization-code exchange at the
// provider's token endpoint (a confidential client: client secret in the
// body, as the common providers expect).
func (a *App) oauthExchangeToken(ctx context.Context, o cfgOAuthView, code string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", o.redirectURI)
	form.Set("client_id", o.clientID)
	if o.clientSecret != "" {
		form.Set("client_secret", o.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.oauthHTTP().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("token endpoint returned %s: %s", resp.Status, truncateBody(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("token response is not JSON: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("token endpoint returned no access_token")
	}
	return tok.AccessToken, nil
}

// oauthUserinfo fetches the claims from the provider's user-info endpoint
// with the access token (Bearer).
func (a *App) oauthUserinfo(ctx context.Context, o cfgOAuthView, accessToken string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.userinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := a.oauthHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("user-info endpoint returned %s: %s", resp.Status, truncateBody(body))
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("user-info response is not JSON: %w", err)
	}
	return claims, nil
}

// cfgOAuthView is the (private) OAuth config fields the HTTP calls need.
// Kept out of the wire types so the client secret never rides in a response.
type cfgOAuthView struct {
	clientID     string
	clientSecret string
	tokenURL     string
	userinfoURL  string
	redirectURI  string
}

func (a *App) oauthHTTP() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// oauthHandle derives the bevin handle + display name from the provider
// claims: preferred_username → email local part → subject. A handle already
// taken by a DIFFERENT identity gets a short subject suffix; if that is also
// taken (or the derivation yields nothing) the login is rejected with a
// clear message — an identity collision an admin resolves by renaming.
func (a *App) oauthHandle(claims map[string]any, subject string) (string, string, error) {
	display, _ := claims["name"].(string)
	display = strings.TrimSpace(display)
	if display == "" {
		display = "New User"
	}
	var base string
	if pu, _ := claims["preferred_username"].(string); strings.TrimSpace(pu) != "" {
		base = setup.SanitizeHandle(pu)
	} else if email, _ := claims["email"].(string); strings.Contains(email, "@") {
		base = setup.SanitizeHandle(strings.SplitN(strings.TrimSpace(email), "@", 2)[0])
	} else {
		base = setup.SanitizeHandle(subject)
	}
	if base == "" {
		base = setup.SanitizeHandle("oauth-") + setup.SanitizeHandle(subject)
	}
	if base == "" {
		return "", "", errors.New("the provider returned no usable identity (no preferred_username, email, or subject) — contact your administrator")
	}
	candidates := []string{base}
	suffix := setup.SanitizeHandle(subject)
	if suffix != "" {
		candidates = append(candidates, base+"-"+suffix[:min(6, len(suffix))])
	}
	for _, handle := range candidates {
		if _, err := a.srv.GetUser(handle); errors.Is(err, store.ErrUserNotFound) {
			return handle, display, nil
		} else if err != nil {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("the identity this provider returned is already mapped to another account in bevin (handle %q) — contact your administrator", base)
}

// truncateBody keeps an error snippet of a provider response body (the body
// is capped at 1 MiB by the callers; this keeps logs/JSON terse).
func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
