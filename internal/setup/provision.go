package setup

// Sidecar provisioning streamed by the display-only sidecars step.
//
// The server auto-configures the sidecars — the wizard's sidecars step takes no
// input. When the admin reaches the step, StartProvision kicks off the
// (idempotent, forward-only) provisioning in the background; the step streams
// per-sidecar progress from ProvisionStatus until everything is ready, then
// shows a confirmation. Finalize waits for the run (idempotent) and uses the
// recorded endpoints, so a sidecar provisioned on the step is NOT re-provisioned
// (no port drift). Normal-mode edit reuses the same step: it probes the
// already-running sidecars (fast, no download) and shows them ready.

import (
	"context"

	"github.com/utopiafallen/bevin/internal/config"
)

// SidecarProgress is the per-sidecar provisioning status the wizard streams.
type SidecarProgress struct {
	Kind     string `json:"kind"`
	Status   string `json:"status"` // pending|downloading|starting|ready|error
	Message  string `json:"message"`
	Percent  int    `json:"percent"` // 0-100 download progress (0 when unknown)
	Endpoint string `json:"endpoint"`
	Error    string `json:"error"`
}

// provisionState tracks an in-flight (or completed) provisioning run.
type provisionState struct {
	started  bool
	running  bool
	done     bool
	err      error
	status   map[string]*SidecarProgress
	sidecars config.Sidecars // recorded endpoints (once ready)
	ctx      context.Context
	cancel   context.CancelFunc
	doneCh   chan struct{} // closed when the run finishes
}

// provisionOrder is the provisioning order (fast sidecars first, the slow
// llama-server — a CPU model load — last).
var provisionOrder = []string{KindQdrant, KindFFmpeg, KindLlamaServer}

// SidecarMeta is a sidecar's display name + a short description (for the
// wizard's progress view).
var SidecarMeta = map[string]struct {
	Name string
	Desc string
}{
	KindQdrant:      {"Qdrant", "Vector database — workspace file search"},
	KindFFmpeg:      {"ffmpeg", "Media decoding — workspace ingest"},
	KindLlamaServer: {"llama-server", "Local embedder (embeddinggemma-2)"},
}

// StartProvision starts the provisioning run (idempotent: a second call while
// it is running, or after it finished, is a no-op). It provisions in the
// background + records the resulting loopback endpoints. Returns immediately;
// the caller polls ProvisionStatus for progress.
func (m *Manager) StartProvision() {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	if m.provState != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &provisionState{
		started: true,
		running: true,
		status:  map[string]*SidecarProgress{},
		ctx:     ctx,
		cancel:  cancel,
		doneCh:  make(chan struct{}),
	}
	for _, kind := range provisionOrder {
		st.status[kind] = &SidecarProgress{Kind: kind, Status: "pending", Message: "Waiting…"}
	}
	m.provState = st
	go m.runProvision(st)
}

// ProvisionStatus snapshots the current provisioning state (for the wizard to
// stream). Kinds are returned in provisionOrder.
func (m *Manager) ProvisionStatus() (done, running bool, statuses []SidecarProgress, sidecars config.Sidecars) {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	st := m.provState
	if st == nil {
		for _, kind := range provisionOrder {
			statuses = append(statuses, SidecarProgress{Kind: kind, Status: "pending", Message: "Waiting…"})
		}
		return false, false, statuses, config.Sidecars{}
	}
	for _, kind := range provisionOrder {
		if p, ok := st.status[kind]; ok {
			cp := *p
			statuses = append(statuses, cp)
		}
	}
	return st.done, st.running, statuses, st.sidecars
}

// WaitForProvision blocks until the run (if any) is done, or ctx is cancelled.
// If provisioning was never started it returns immediately. Finalize calls this
// so a sidecar provisioned on the step is not re-provisioned (no port drift)
// and its endpoint is recorded before server.toml is written.
func (m *Manager) WaitForProvision(ctx context.Context) error {
	m.provMu.Lock()
	st := m.provState
	m.provMu.Unlock()
	if st == nil {
		return nil
	}
	select {
	case <-st.doneCh:
		return st.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recordedSidecars returns the endpoints the provisioning run recorded (once
// ready); when no run has finished (or none happened) it falls back to the
// wizard answers' sidecars. This is what Finalize/FinalizeEdit commit.
func (m *Manager) recordedSidecars() config.Sidecars {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recordedSidecarsLocked()
}

// recordedSidecarsLocked is recordedSidecars for callers that already hold
// m.mu (FinalizeEdit). It only takes provMu (the m.mu → m.provMu order keeps
// the lock graph acyclic).
func (m *Manager) recordedSidecarsLocked() config.Sidecars {
	m.provMu.Lock()
	st := m.provState
	var rec config.Sidecars
	if st != nil && st.done {
		rec = st.sidecars
	}
	m.provMu.Unlock()
	// Fall back to the answers for any endpoint the run didn't record.
	if rec.Qdrant == "" {
		rec.Qdrant = m.state.Answers.Sidecars.Qdrant
	}
	if rec.LlamaServer == "" {
		rec.LlamaServer = m.state.Answers.Sidecars.LlamaServer
	}
	if rec.FFmpeg == "" {
		rec.FFmpeg = m.state.Answers.Sidecars.FFmpeg
	}
	return rec
}

// runProvision provisions the sidecars sequentially (in provisionOrder),
// streaming the real install functions' phase reports (downloading / starting)
// into this run's status map via the sidecarReport hook. It records each
// sidecar's endpoint and finishes when all are done (or the first error).
func (m *Manager) runProvision(st *provisionState) {
	defer st.cancel()
	defer func() {
		m.provMu.Lock()
		st.running = false
		st.done = true
		m.provMu.Unlock()
		close(st.doneCh)
	}()
	// Stream the real install functions' phase reports into this run's status.
	sidecarReport = func(kind, status, message string, percent int) {
		m.setProvision(kind, status, message, percent)
	}
	defer func() { sidecarReport = nil }()

	for _, kind := range provisionOrder {
		if st.ctx.Err() != nil {
			return
		}
		m.setProvision(kind, "downloading", "Checking for an existing install…", 0)
		existing := m.existingEndpoint(kind)
		ep, err := m.sidecars.Ensure(st.ctx, m.dataDir, kind, existing)
		if err != nil {
			st.err = err
			m.setProvision(kind, "error", err.Error(), 0)
			return
		}
		m.recordEndpoint(kind, ep)
		m.setProvision(kind, "ready", "Ready", 100)
	}
}

// setProvision updates one sidecar's status + message + percent.
func (m *Manager) setProvision(kind, status, message string, percent int) {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	if st := m.provState; st != nil {
		if p, ok := st.status[kind]; ok {
			p.Status = status
			p.Message = message
			p.Percent = percent
		}
	}
}

// recordEndpoint records a sidecar's endpoint (so Finalize uses it without
// re-provisioning) + its status endpoint field.
func (m *Manager) recordEndpoint(kind, endpoint string) {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	if st := m.provState; st != nil {
		switch kind {
		case KindQdrant:
			st.sidecars.Qdrant = endpoint
		case KindLlamaServer:
			st.sidecars.LlamaServer = endpoint
		case KindFFmpeg:
			st.sidecars.FFmpeg = endpoint
		}
		if p, ok := st.status[kind]; ok {
			p.Endpoint = endpoint
		}
	}
}

// existingEndpoint returns the sidecar's current endpoint from the wizard
// answers (blank in setup mode → auto-provision; the recorded endpoint in
// normal-mode edit → probe it, no download).
func (m *Manager) existingEndpoint(kind string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch kind {
	case KindQdrant:
		return m.state.Answers.Sidecars.Qdrant
	case KindLlamaServer:
		return m.state.Answers.Sidecars.LlamaServer
	case KindFFmpeg:
		return m.state.Answers.Sidecars.FFmpeg
	}
	return ""
}
