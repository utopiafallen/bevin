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

// buildSandbox (re)builds the per-user container backend from the config:
// nil when sandboxing is disabled in the config or unsupported on this
// platform. Called at boot and on every config save (the [sandbox] area
// can flip at runtime — an admin enabling it on an existing server).
func (a *App) buildSandbox(cfg *config.ServerConfig) {
	var bk sandbox.Backend
	if cfg.Sandbox.Enabled {
		if b, err := sandbox.New(a.dataDir, sandboxConfigFrom(cfg.Sandbox)); err == nil {
			bk = b
		} else if !errors.Is(err, sandbox.ErrDisabled) {
			log.Printf("bevin: sandbox backend unavailable: %v", err)
		}
	}
	a.cfgMu.Lock()
	a.sandboxBk = bk
	a.cfgMu.Unlock()
}

func sandboxConfigFrom(c config.Sandbox) sandbox.Config {
	return sandbox.Config{Enabled: c.Enabled, Network: c.Network, Cores: c.Cores, MemoryMB: c.MemoryMB}
}

// sandboxBackend returns the live backend (nil = disabled/unsupported).
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
// live backend when there is one, from a direct check otherwise (so a
// disabled server can still tell the user what the runtime looks like).
func (a *App) sandboxRuntime(ctx context.Context) sandbox.Runtime {
	if bk := a.sandboxBackend(); bk != nil {
		return bk.Runtime(ctx)
	}
	return sandbox.Check(ctx, a.dataDir, a.sandboxConfig())
}

// sandboxGate writes the 503 when sandboxing is off and returns the
// request's backend + user otherwise (the handle ALWAYS comes from the
// session — never from the request body).
func (a *App) sandboxGate(w http.ResponseWriter, r *http.Request) (sandbox.Backend, *store.User, bool) {
	u := auth.UserFromContext(r.Context())
	bk := a.sandboxBackend()
	if bk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "sandboxing is not enabled on this server (server.toml [sandbox])",
			"runtime": a.sandboxRuntime(r.Context()),
		})
		return nil, nil, false
	}
	return bk, u, true
}

// GET /api/sandbox — the OS runtime readiness + the user's container
// state.
func (a *App) sandboxStatus(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	info, err := bk.Status(r.Context(), u.Handle)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":   err.Error(),
			"runtime": a.sandboxRuntime(r.Context()),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime":   a.sandboxRuntime(r.Context()),
		"container": info,
	})
}

// POST /api/sandbox/ensure — create (when needed) and start the user's
// container. Idempotent. Can take a while on first run (rootfs copy from
// the base image + container boot) — the client should allow minutes.
func (a *App) sandboxEnsure(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	// The runtime must be ready NOW (the setup check gates this at install
	// time; this guards a runtime lost mid-flight, e.g. the service
	// stopped). 412 = the precondition (the enabled runtime) is not met.
	if rt := a.sandboxRuntime(r.Context()); !rt.OK {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error":   "the container runtime is not ready",
			"runtime": rt,
		})
		return
	}
	info, err := bk.Ensure(r.Context(), u.Handle)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// POST /api/sandbox/exec — run a command string in the user's container
// (the container's shell interprets it — arbitrary code is the point).
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
		code := http.StatusBadGateway
		if errors.Is(err, sandbox.ErrNotRunning) {
			code = http.StatusPreconditionFailed
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/sandbox/stop — stop the user's container (storage preserved).
func (a *App) sandboxStop(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	info, err := bk.Stop(r.Context(), u.Handle)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// DELETE /api/sandbox — stop the user's container and delete its
// persistent storage (the user's data directory is untouched).
func (a *App) sandboxRemove(w http.ResponseWriter, r *http.Request) {
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	if err := bk.Remove(r.Context(), u.Handle); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}
