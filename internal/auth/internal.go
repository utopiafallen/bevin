package auth

import (
	"context"
	"time"

	"github.com/utopiafallen/bevin/internal/store"
)

// Provider is the authentication seam. v1 is the internal (self-IdP)
// implementation; a future OAuth (authentik) provider implements the same
// interface so the rest of the server never special-cases a backend.
type Provider interface {
	// Authenticate verifies handle/password and, on success, creates a session
	// and returns the user + an opaque session token.
	Authenticate(ctx context.Context, handle, password string) (*store.User, string, error)
	// UserFromToken resolves a live session token to its user.
	UserFromToken(ctx context.Context, token string) (*store.User, error)
	// Logout invalidates a session token.
	Logout(ctx context.Context, token string) error
	// SetPassword sets or replaces a user's password.
	SetPassword(ctx context.Context, handle, password string) error
	// CreateUserProvisioned creates a user with a role + password (admin and
	// initial setup only — there is no self-service registration).
	CreateUserProvisioned(ctx context.Context, handle, displayName, role, password string) (*store.User, error)
}

// InternalProvider is the v1 self-IdP implementation backed by a ServerStore.
type InternalProvider struct {
	srv *store.ServerStore
	ttl time.Duration
}

// NewInternalProvider builds the internal provider with a session TTL.
func NewInternalProvider(srv *store.ServerStore, ttl time.Duration) *InternalProvider {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return &InternalProvider{srv: srv, ttl: ttl}
}

// TTL returns the session TTL (used to size the cookie MaxAge).
func (p *InternalProvider) TTL() time.Duration { return p.ttl }

func (p *InternalProvider) Authenticate(ctx context.Context, handle, password string) (*store.User, string, error) {
	u, err := p.srv.GetUser(handle)
	if err != nil {
		return nil, "", ErrInvalidCredentials
	}
	if u.Status != "active" {
		return nil, "", ErrInvalidCredentials
	}
	hash, err := p.srv.PasswordHash(handle)
	if err != nil {
		return nil, "", err
	}
	if hash == "" || !verifyArgon2id(hash, password) {
		return nil, "", ErrInvalidCredentials
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}
	if err := p.srv.CreateSession(token, handle, p.ttl, ""); err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (p *InternalProvider) UserFromToken(ctx context.Context, token string) (*store.User, error) {
	if token == "" {
		return nil, store.ErrSessionNotFound
	}
	sess, err := p.srv.GetSession(token)
	if err != nil {
		return nil, err
	}
	u, err := p.srv.GetUser(sess.Handle)
	if err != nil {
		return nil, err
	}
	if u.Status != "active" {
		return nil, store.ErrSessionNotFound
	}
	return u, nil
}

func (p *InternalProvider) Logout(ctx context.Context, token string) error {
	return p.srv.DeleteSession(token)
}

func (p *InternalProvider) SetPassword(ctx context.Context, handle, password string) error {
	if _, err := p.srv.GetUser(handle); err != nil {
		return err
	}
	h, err := hashArgon2id(password)
	if err != nil {
		return err
	}
	return p.srv.SetCredential(handle, h)
}

func (p *InternalProvider) CreateUserProvisioned(ctx context.Context, handle, displayName, role, password string) (*store.User, error) {
	if role == "" {
		role = "user"
	}
	u, err := p.srv.CreateUser(handle, displayName, role)
	if err != nil {
		return nil, err
	}
	if err := p.SetPassword(ctx, handle, password); err != nil {
		return nil, err
	}
	return u, nil
}