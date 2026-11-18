package setup

// First-time-setup support for the per-user sandbox (containers):
//
//   - the OS container runtime check (the HCS service + hypervisor on
//     Windows; systemd + systemd-nspawn on Linux) — the user's directive:
//     the check runs during first-time setup and the user is prompted to
//     enable the runtime for their OS BEFORE setup is allowed to proceed;
//   - the base container image provisioning (a minimal OS the per-user
//     containers are created from — linux: an Ubuntu rootfs; windows: a
//     servercore image), streamed by the display-only server step like
//     the sidecars step streams its sidecars.
//
// Finalize gates on the sandbox (server-side enforcement of the wizard
// prompt): per-user containers are MANDATORY (no opt-out — user
// directive), so a setup cannot complete until the runtime is ready and
// the base image is provisioned.

import (
	"context"
	"fmt"
	"log"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/sandbox"
)

// BaseStatus is the base container image's provisioning status (the
// wizard streams it).
type BaseStatus struct {
	Status   string  `json:"status"` // pending|provisioning|ready|error
	Progress float64 `json:"progress"`
	Detail   string  `json:"detail"`
}

// SandboxStatus is the wizard's sandbox state (runtime + base image).
type SandboxStatus struct {
	Runtime sandbox.Runtime `json:"runtime"`
	Base    BaseStatus      `json:"base"`
}

// sandboxBaseState tracks an in-flight base image provisioning run (the
// same pattern as the sidecar provisioning run).
type sandboxBaseState struct {
	running  bool
	done     bool
	err      error
	progress float64
	detail   string
	ctx      context.Context
	cancel   context.CancelFunc
	doneCh   chan struct{} // closed when the run finishes
}

// SandboxStatus runs the OS runtime check + reports the base image state.
// When the runtime is ready and the base is missing it auto-starts the
// provisioning (the server step polls this endpoint — display-only, the
// server drives the work like it does for the sidecars).
func (m *Manager) SandboxStatus(ctx context.Context) SandboxStatus {
	// What setup will save: the [sandbox] section of the existing config
	// when there is one (edit mode), else the fresh-setup default —
	// always enabled (per-user containers are mandatory).
	cfg := sandbox.Config{Enabled: true, Network: "veth"}
	if c, err := config.Load(m.dataDir); err == nil {
		cfg = sandbox.Config{Enabled: true, Network: c.Sandbox.Network, Cores: c.Sandbox.Cores, MemoryMB: c.Sandbox.MemoryMB}
	}
	rt := sandbox.Check(ctx, m.dataDir, cfg)
	base := m.baseState()
	if rt.OK && base.Status == "pending" {
		m.StartSandboxBaseProvision()
		base = m.baseState()
	}
	return SandboxStatus{Runtime: rt, Base: base}
}

// baseState snapshots the base image state from disk + the in-flight run.
func (m *Manager) baseState() BaseStatus {
	if sandboxBaseReady(m.dataDir) {
		return BaseStatus{Status: "ready", Progress: 1}
	}
	m.baseMu.Lock()
	defer m.baseMu.Unlock()
	st := m.baseStateRun
	if st == nil || !st.running {
		return BaseStatus{Status: "pending"}
	}
	if st.done {
		if st.err != nil {
			return BaseStatus{Status: "error", Detail: st.err.Error()}
		}
		return BaseStatus{Status: "ready", Progress: 1}
	}
	return BaseStatus{Status: "provisioning", Progress: st.progress, Detail: st.detail}
}

// StartSandboxBaseProvision starts the base image download + extraction in
// the background (idempotent: a second call while running, or when the
// base is already provisioned, is a no-op). Returns immediately; the
// caller polls SandboxStatus for progress.
func (m *Manager) StartSandboxBaseProvision() {
	if sandboxBaseReady(m.dataDir) {
		return
	}
	m.baseMu.Lock()
	defer m.baseMu.Unlock()
	if m.baseStateRun != nil && m.baseStateRun.running && !m.baseStateRun.done {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &sandboxBaseState{
		running: true,
		ctx:     ctx,
		cancel:  cancel,
		detail:  "starting…",
		doneCh:  make(chan struct{}),
	}
	m.baseStateRun = st
	go func() {
		defer close(st.doneCh)
		err := provisionSandboxBase(ctx, m.dataDir, func(frac float64, detail string) {
			m.baseMu.Lock()
			st.progress = frac
			st.detail = detail
			m.baseMu.Unlock()
		})
		m.baseMu.Lock()
		st.done = true
		st.err = err
		m.baseMu.Unlock()
		if err != nil {
			log.Printf("bevin: setup: sandbox base provisioning: %v", err)
		}
	}()
}

// WaitForSandboxBase blocks until the base image is provisioned (starting
// the run if needed) — Finalize uses it so the saved config never points
// at a missing base.
func (m *Manager) WaitForSandboxBase(ctx context.Context) error {
	m.StartSandboxBaseProvision()
	m.baseMu.Lock()
	st := m.baseStateRun
	m.baseMu.Unlock()
	if st == nil || st.done {
		if sandboxBaseReady(m.dataDir) {
			return nil
		}
		if st != nil && st.err != nil {
			return st.err
		}
		return fmt.Errorf("the base container image could not be provisioned")
	}
	// Race the run's completion against the caller's context.
	select {
	case <-st.doneCh:
		if sandboxBaseReady(m.dataDir) {
			return nil
		}
		m.baseMu.Lock()
		err := st.err
		m.baseMu.Unlock()
		if err != nil {
			return err
		}
		return fmt.Errorf("the base container image could not be provisioned")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SandboxGateError is the error the setup gates return when the OS
// container runtime is not ready — it carries the human-readable
// remediation the wizard shows (the user enables the runtime and
// re-submits). It is enforced at the SERVER step (advance) and again at
// Finalize (the backstop).
func SandboxGateError(rt sandbox.Runtime) error {
	return fmt.Errorf("the OS container runtime is not ready: %s — enable it (see the instructions on the server step) and try again", rt.Detail)
}
