package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/sandbox"
	"github.com/utopiafallen/bevin/internal/store"
)

// The user's per-user execution environment is a container under the
// hood, but containerization is a HIDDEN detail from the user (user
// directive): the user-facing surface says "your environment" and never
// leaks the runtime internals (nspawn/HCS, service names, remediation
// text). The full detail (the runtime check's diagnosis + the raw
// backend error) is still returned — to the ADMIN only, for server
// maintenance; the admin is the one who enables the OS runtime.

// buildSandbox (re)builds the per-user container backend from the config.
// Per-user containers are mandatory (there is no config off switch — the
// config layer canonicalizes [sandbox] enabled to true), so the backend
// is built whenever the platform has a container API; it is nil on
// unsupported platforms (the API then answers 503). Called at boot and on
// every config save.
func (a *App) buildSandbox(cfg *config.ServerConfig) {
	var bk sandbox.Backend
	if b, err := sandbox.New(a.dataDir, sandboxConfigFrom(cfg.Sandbox)); err == nil {
		bk = b
	} else if !errors.Is(err, sandbox.ErrDisabled) {
		log.Printf("bevin: sandbox backend unavailable: %v", err)
	}
	a.cfgMu.Lock()
	a.sandboxBk = bk
	a.cfgMu.Unlock()
}

func sandboxConfigFrom(c config.Sandbox) sandbox.Config {
	return sandbox.Config{Enabled: c.Enabled, Network: c.Network, Cores: c.Cores, MemoryMB: c.MemoryMB, DiskMB: c.DiskMB}
}

// sandboxBackend returns the live backend (nil = unsupported platform).
func (a *App) sandboxBackend() sandbox.Backend {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.sandboxBk
}

// sandboxConfig is the live [sandbox] section as a sandbox.Config.
func (a *App) sandboxConfig() sandbox.Config {
	return sandboxConfigFrom(a.liveCfg().Sandbox)
}

// sandboxRuntime reports the OS container runtime's readiness — from the
// live backend when there is one, from a direct check otherwise.
func (a *App) sandboxRuntime(ctx context.Context) sandbox.Runtime {
	if bk := a.sandboxBackend(); bk != nil {
		return bk.Runtime(ctx)
	}
	return sandbox.Check(ctx, a.dataDir, a.sandboxConfig())
}

func (a *App) sandboxRuntimePtr(ctx context.Context) *sandbox.Runtime {
	rt := a.sandboxRuntime(ctx)
	return &rt
}

// isAdmin reports whether the session user is an admin (the admin sees
// the runtime internals; the regular user sees the neutral wording).
func isAdmin(u *store.User) bool { return u != nil && u.Role == "admin" }

// envError writes the user-facing error for the environment state: the
// neutral wording for regular users (the container internals are hidden
// from them), the runtime detail + the raw error for the admin (who is
// the one who enables the OS runtime).
func envError(w http.ResponseWriter, code int, u *store.User, neutral string, rt *sandbox.Runtime, raw error) {
	resp := map[string]any{"error": neutral}
	if isAdmin(u) {
		if rt != nil {
			resp["runtime"] = *rt
		}
		if raw != nil {
			resp["detail"] = raw.Error()
		}
	}
	writeJSON(w, code, resp)
}

// envGate authenticates the request and admits ANY role (the sandbox API
// is user-facing — a user's OWN environment — but the admin may also use
// it: it is the admin who must diagnose a lost runtime, and only the
// admin sees the runtime detail in the responses). The handle ALWAYS
// comes from the session — never from the request body.
func (a *App) envGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, err := auth.SessionToken(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		u, err := a.prov.UserFromToken(r.Context(), tok)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		next(w, r.WithContext(auth.ContextWithUser(r.Context(), u)))
	}
}

// sandboxGate writes the 503 when the platform has no container backend
// and returns the request's backend + user otherwise (the handle ALWAYS
// comes from the session — never from the request body).
func (a *App) sandboxGate(w http.ResponseWriter, r *http.Request) (sandbox.Backend, *store.User, bool) {
	u := auth.UserFromContext(r.Context())
	bk := a.sandboxBackend()
	if bk == nil {
		envError(w, http.StatusServiceUnavailable, u,
			"your environment is not available on this server", a.sandboxRuntimePtr(r.Context()), nil)
		return nil, nil, false
	}
	return bk, u, true
}

// GET /api/sandbox — the user's environment state (the runtime detail
// is admin-only — the container internals are hidden from the user).
// A missing runtime is a STATE, not a gateway failure: 412 (nothing can
// be running without it), the same precondition as ensure.
func (a *App) sandboxStatus(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	if rt := a.sandboxRuntime(r.Context()); !rt.OK {
		envError(w, http.StatusPreconditionFailed, u,
			"your environment is not ready yet — try again in a moment", &rt, nil)
		return
	}
	info, err := bk.Status(r.Context(), u.Handle)
	if err != nil {
		envError(w, http.StatusBadGateway, u,
			"your environment could not be reached", a.sandboxRuntimePtr(r.Context()), err)
		return
	}
	resp := map[string]any{"environment": info}
	if isAdmin(u) {
		resp["runtime"] = a.sandboxRuntime(r.Context())
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/sandbox/ensure — create (when needed) and start the user's
// environment. Idempotent. Can take a while on first run (rootfs copy
// from the base image + boot) — the client should allow minutes.
func (a *App) sandboxEnsure(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	// The runtime must be ready NOW (the setup check gates this at
	// install time; this guards a runtime lost mid-flight, e.g. the
	// service stopped). 412 = the precondition (the ready runtime) is
	// not met.
	if rt := a.sandboxRuntime(r.Context()); !rt.OK {
		envError(w, http.StatusPreconditionFailed, u,
			"your environment is not ready yet — try again in a moment", &rt, nil)
		return
	}
	info, err := bk.Ensure(r.Context(), u.Handle)
	if err != nil {
		envError(w, http.StatusBadGateway, u,
			"your environment could not be started — try again", a.sandboxRuntimePtr(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// POST /api/sandbox/exec — run a command string in the user's
// environment (its shell interprets it — arbitrary code is the point).
// Body: {"command": "...", "timeout_ms": 120000} (timeout optional,
// default 60 s, capped at 5 min).
func (a *App) sandboxExec(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	var req struct {
		Command   string `json:"command"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "command is required"})
		return
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	res, err := bk.Exec(r.Context(), u.Handle, req.Command, timeout)
	if err != nil {
		// A non-zero command EXIT is a Result, not an error — everything
		// that errors here is infrastructure (the environment not
		// running, the exec plumbing), so it gets the neutral wording
		// for the user.
		code := http.StatusBadGateway
		if errors.Is(err, sandbox.ErrNotRunning) {
			code = http.StatusPreconditionFailed
		}
		envError(w, code, u, "your environment could not run the command", a.sandboxRuntimePtr(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/sandbox/stop — stop the user's environment (storage
// preserved).
func (a *App) sandboxStop(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	info, err := bk.Stop(r.Context(), u.Handle)
	if err != nil {
		envError(w, http.StatusBadGateway, u,
			"your environment could not be stopped", a.sandboxRuntimePtr(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// DELETE /api/sandbox — stop the user's environment and delete its
// persistent storage (the user's data directory is untouched).
func (a *App) sandboxRemove(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	if err := bk.Remove(r.Context(), u.Handle); err != nil {
		envError(w, http.StatusBadGateway, u,
			"your environment could not be removed", a.sandboxRuntimePtr(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}
