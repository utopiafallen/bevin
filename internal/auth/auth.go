// Package auth is the identity/auth seam. v1 is the internal provider
// (bevin acts as its own IdP — the "admin is always self-IdP" rule): users
// are admin-provisioned (no self-service registration), passwords are
// argon2id hashes, and sessions are opaque signed tokens in a cookie. A future
// OAuth (authentik) provider implements the same Provider interface.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/utopiafallen/bevin/internal/store"
)

// SessionCookie is the name of the bevin session cookie.
const SessionCookie = "bevin_session"

// ErrInvalidCredentials is the standard login failure.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ctxKey is an unexported context key type.
type ctxKey struct{}

// ContextWithUser stores the authenticated user in the request context.
func ContextWithUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFromContext returns the authenticated user, or nil.
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxKey{}).(*store.User)
	return u
}

// SetSessionCookie sets the (httpOnly, lax) session cookie. The binary speaks
// plain HTTP (TLS lives at the reverse proxy), so no Secure flag is set here.
func SetSessionCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie removes the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

// SessionToken reads the session cookie.
func SessionToken(r *http.Request) (string, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return "", err
	}
	return c.Value, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}