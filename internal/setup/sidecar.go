package setup

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// Sidecar kind names.
const (
	KindQdrant       = "qdrant"
	KindLlamaServer  = "llama-server"
	KindFFmpeg       = "ffmpeg"
	KindEmbeddingGGUF = "embedding-gguf"
)

// SidecarManager handles sidecar provisioning with the safety invariants:
//   - probe-first: if a sidecar is already installed + healthy, Ensure is a no-op.
//   - forward-only: never deletes; stepping back / canceling leaves sidecars in place.
//   - resumable: a partial download is not a broken install (HTTP Range resume).
//
// Probe and Install are injectable so the state machine + idempotency are testable
// without a network. The default impl performs real HTTP probes and real downloads.
type SidecarManager struct {
	Probe   func(ctx context.Context, dataDir, kind, endpoint string) (bool, error)
	Install func(ctx context.Context, dataDir, kind string, preferredPort int) (string, error)
}

// NewSidecarManager builds a manager; nil probe/install fall back to the real
// defaults.
func NewSidecarManager(probe func(ctx context.Context, dataDir, kind, endpoint string) (bool, error),
	install func(ctx context.Context, dataDir, kind string, preferredPort int) (string, error)) *SidecarManager {
	if probe == nil {
		probe = defaultProbe
	}
	if install == nil {
		install = defaultInstall
	}
	return &SidecarManager{Probe: probe, Install: install}
}

// SidecarDir returns the per-sidecar marker dir under <data dir>/sidecars/<kind>.
func SidecarDir(dataDir, kind string) string {
	return filepath.Join(dataDir, "sidecars", kind)
}

// Ensure is idempotent + forward-only: if the endpoint is already healthy, no-op;
// otherwise (re-)install. It never deletes. When (re-)installing it prefers the
// sidecar's existing loopback port (so a re-launch stays on its port when that
// port is still free), so the recorded endpoint doesn't drift.
func (sm *SidecarManager) Ensure(ctx context.Context, dataDir, kind, endpoint string) (string, error) {
	if endpoint != "" && sm.Probe != nil {
		if ok, err := sm.Probe(ctx, dataDir, kind, endpoint); err == nil && ok {
			return endpoint, nil
		}
	}
	return sm.Install(ctx, dataDir, kind, loopbackPort(endpoint))
}

// EnsureAll provisions the required sidecars for a config, idempotently, all
// bound to loopback. Each sidecar's (loopback) endpoint is recorded back into
// cfg so the running process points at it (a re-launch that lands on a
// different free port updates the endpoint accordingly).
func (sm *SidecarManager) EnsureAll(ctx context.Context, cfg *config.ServerConfig) error {
	ep, err := sm.Ensure(ctx, cfg.DataDir, KindQdrant, cfg.Sidecars.Qdrant)
	if err != nil {
		return fmt.Errorf("qdrant: %w", err)
	}
	cfg.Sidecars.Qdrant = ep
	ef, err := sm.Ensure(ctx, cfg.DataDir, KindFFmpeg, "")
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	cfg.Sidecars.FFmpeg = ef
	// The llama-server (embedder) is the slow one (a CPU model load can take
	// minutes) — ensure it last so the fast sidecars come up first.
	ep, err = sm.Ensure(ctx, cfg.DataDir, KindLlamaServer, cfg.Sidecars.LlamaServer)
	if err != nil {
		return fmt.Errorf("llama-server: %w", err)
	}
	cfg.Sidecars.LlamaServer = ep
	return nil
}

// superviseInterval is how often the sidecar supervisor re-checks the sidecars
// and respawns any that died mid-session.
var superviseInterval = 30 * time.Second

// Supervise keeps the sidecars alive for the life of the process. On start it
// ensures all of them are up (re-launching any that are down — e.g. after a
// bevin restart or a machine reboot, since the sidecars are separate processes
// bevin does not keep alive on its own), then re-checks every
// superviseInterval and respawns any that died mid-session (crash/OOM). It
// returns when ctx is done. A down sidecar is re-provisioned via the same
// idempotent Ensure (probe-first, forward-only), so an already-running sidecar
// is never disturbed. It updates cfg in place with the endpoints it lands on;
// it does not write server.toml (the setup wizard + admin edit own the on-disk
// config, which is re-ensured on the next boot).
func (sm *SidecarManager) Supervise(ctx context.Context, cfg *config.ServerConfig) {
	for {
		if err := sm.EnsureAll(ctx, cfg); err != nil {
			// A sidecar failed to (re-)provision. Log + retry next tick; a
			// transient failure (a busy port, a slow model load) is not fatal
			// to the server.
			log.Printf("bevin: sidecar: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(superviseInterval):
		}
	}
}

// loopbackPort returns the port of a loopback endpoint (127.0.0.1 / localhost /
// ::1), or 0 if the endpoint is not loopback or has no port. It is used to
// prefer a sidecar's existing port when re-launching (avoids port drift).
func loopbackPort(endpoint string) int {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return 0
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return 0
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return p
}

// markerPath is the per-sidecar version marker (a completed install writes it).
func markerPath(dataDir, kind string) string {
	return filepath.Join(SidecarDir(dataDir, kind), "MARKER")
}

func writeMarker(dataDir, kind, endpoint string) error {
	dir := SidecarDir(dataDir, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(markerPath(dataDir, kind), []byte(endpoint+"\n"), 0o644)
}

// ---- default probe (real) ----

// defaultProbe checks whether a sidecar is healthy at its endpoint. For ffmpeg
// the "endpoint" is a filesystem path (or empty → search PATH / marker).
func defaultProbe(ctx context.Context, dataDir, kind, endpoint string) (bool, error) {
	switch kind {
	case KindQdrant:
		return httpHealthy(ctx, endpoint, "/collections")
	case KindLlamaServer:
		return httpHealthy(ctx, endpoint, "/v1/models")
	case KindFFmpeg:
		p := endpoint
		if p == "" {
			p = findSystemFFmpeg()
		}
		if p == "" {
			return false, nil
		}
		_, err := exec.CommandContext(ctx, p, "-version").CombinedOutput()
		return err == nil, nil
	case KindEmbeddingGGUF:
		return fileExists(filepath.Join(SidecarDir(dataDir, "models"), "embeddinggemma-2-Q8_0.gguf")), nil
	}
	return false, fmt.Errorf("unknown sidecar %q", kind)
}

func httpHealthy(ctx context.Context, endpoint, path string) (bool, error) {
	url := strings.TrimRight(endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil // unreachable → not installed
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode < 500, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func findSystemFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// ---- default install (real, best-effort) ----

func defaultInstall(ctx context.Context, dataDir, kind string, preferredPort int) (string, error) {
	switch kind {
	case KindQdrant:
		return installQdrant(ctx, dataDir, preferredPort)
	case KindLlamaServer:
		return installLlamaServer(ctx, dataDir, preferredPort)
	case KindFFmpeg:
		return installFFmpeg(ctx, dataDir)
	}
	return "", fmt.Errorf("unknown sidecar %q", kind)
}

// osArch returns a platform token like "linux-x86_64" / "windows-x86_64".
func osArch() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return runtime.GOOS + "-" + arch
}