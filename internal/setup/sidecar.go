package setup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/utopiafallen/alcyona-agent/internal/config"
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
	Install func(ctx context.Context, dataDir, kind string) (string, error)
}

// NewSidecarManager builds a manager; nil probe/install fall back to the real
// defaults.
func NewSidecarManager(probe func(ctx context.Context, dataDir, kind, endpoint string) (bool, error),
	install func(ctx context.Context, dataDir, kind string) (string, error)) *SidecarManager {
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
// otherwise install. It never deletes.
func (sm *SidecarManager) Ensure(ctx context.Context, dataDir, kind, endpoint string) (string, error) {
	if endpoint != "" && sm.Probe != nil {
		if ok, err := sm.Probe(ctx, dataDir, kind, endpoint); err == nil && ok {
			return endpoint, nil
		}
	}
	return sm.Install(ctx, dataDir, kind)
}

// EnsureAll provisions the required sidecars for a config, idempotently, all
// bound to loopback.
func (sm *SidecarManager) EnsureAll(ctx context.Context, cfg *config.ServerConfig) error {
	if _, err := sm.Ensure(ctx, cfg.DataDir, KindQdrant, cfg.Sidecars.Qdrant); err != nil {
		return fmt.Errorf("qdrant: %w", err)
	}
	if _, err := sm.Ensure(ctx, cfg.DataDir, KindLlamaServer, cfg.Sidecars.LlamaServer); err != nil {
		return fmt.Errorf("llama-server: %w", err)
	}
	if _, err := sm.Ensure(ctx, cfg.DataDir, KindFFmpeg, ""); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
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

func defaultInstall(ctx context.Context, dataDir, kind string) (string, error) {
	switch kind {
	case KindQdrant:
		return installQdrant(ctx, dataDir)
	case KindLlamaServer:
		return installLlamaServer(ctx, dataDir)
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