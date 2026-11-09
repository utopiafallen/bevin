package setup

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
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
)

// Release artifact locations. These are the common stable URLs as of this
// implementation; if a project renames a release artifact, update the small
// set here (and re-verify). Qdrant per-OS artifacts follow its GitHub release
// naming; the embedding GGUF is pulled from HuggingFace.
const (
	qdrantVersion = "1.13.2"
	llamaBuild    = "b5906"

	// embeddinggemma-2 GGUF (full multimodal 740M + mmproj, Q8_0 ~865MB).
	// NOTE: confirm the exact HF repo/path at first real run (per setup plan §6).
	embedGGUFRepo  = "google"
	embedGGUFBasename = "embeddinggemma-2-Q8_0.gguf"
	embedMMProjName = "embeddinggemma-2-mmproj-Q8_0.gguf"
)

// sidecar endpoints (loopback) used when auto-provisioning.
const (
	defaultQdrantEndpoint   = "http://127.0.0.1:6333"
	defaultLlamaEndpoint    = "http://127.0.0.1:8081"
)

// ---- download (resumable, atomic, cached) ----

// downloadResumable fetches url to dest, resuming a partial <dest>.part via
// HTTP Range. The file is written to <dest>.part then atomically renamed to
// dest, so a partial download is never a broken final file. It returns the
// number of bytes in the final file.
func downloadResumable(ctx context.Context, url, dest string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	part := dest + ".part"

	start := int64(0)
	if fi, err := os.Stat(part); err == nil {
		start = fi.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK && start > 0 {
		// Server ignored Range → restart fresh.
		start = 0
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if written == 0 && start == 0 {
		return 0, fmt.Errorf("download %s: 0 bytes", url)
	}
	if err := os.Rename(part, dest); err != nil {
		return 0, err
	}
	fi, _ := os.Stat(dest)
	if fi == nil {
		return 0, fmt.Errorf("download %s: missing after rename", url)
	}
	return fi.Size(), nil
}

// ---- archive extraction ----

// extractTarGz extracts a .tar.gz into destDir and returns the top-level entry
// name (if a single root dir exists).
func extractTarGz(archive, destDir string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	roots := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		name := hdr.Name
		if i := strings.Index(name, "/"); i >= 0 {
			roots[name[:i]] = true
		}
		if err := writeTarEntry(destDir, hdr, tr); err != nil {
			return "", err
		}
	}
	if len(roots) == 1 {
		for r := range roots {
			return r, nil
		}
	}
	return "", nil
}

func writeTarEntry(destDir string, hdr *tar.Header, r io.Reader) error {
	target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
	if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe tar path: %s", hdr.Name) // path traversal guard
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o755)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, r)
		return err
	}
	return nil
}

func extractZip(archive, destDir string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe zip path: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- per-sidecar install ----

// installQdrant downloads a per-OS Qdrant build, launches it bound to loopback,
// and returns its endpoint. Idempotent: re-running finds the running instance
// via the probe (Ensure) and is a no-op.
func installQdrant(ctx context.Context, dataDir string) (string, error) {
	dir := SidecarDir(dataDir, KindQdrant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "qdrant")
	if _, err := os.Stat(bin); err != nil {
		url := qdrantDownloadURL()
		if url == "" {
			return "", fmt.Errorf("no qdrant release known for %s (pass an endpoint in the wizard)", osArch())
		}
		if _, err := downloadResumable(ctx, url, filepath.Join(dir, "qdrant-artifact")); err != nil {
			return "", err
		}
		if err := extractArtifact(filepath.Join(dir, "qdrant-artifact"), dir, "qdrant"); err != nil {
			return "", err
		}
	}
	if err := launchDaemon(ctx, bin, dataDir, "qdrant", "--port", "6333"); err != nil {
		return "", err
	}
	return defaultQdrantEndpoint, waitForHealthy(ctx, defaultQdrantEndpoint, "/collections")
}

// installLlamaServer downloads the llama-server binary + the embeddinggemma-2
// GGUF + mmproj and launches it in embedding mode on loopback.
func installLlamaServer(ctx context.Context, dataDir string) (string, error) {
	dir := SidecarDir(dataDir, KindLlamaServer)
	models := filepath.Join(dir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "llama-server")
	if _, err := os.Stat(bin); err != nil {
		if url := llamaDownloadURL(); url != "" {
			if _, err := downloadResumable(ctx, url, filepath.Join(dir, "llama-artifact")); err != nil {
				return "", err
			}
			if err := extractArtifact(filepath.Join(dir, "llama-artifact"), dir, "llama-server"); err != nil {
				return "", err
			}
		}
	}
	gguf := filepath.Join(models, embedGGUFBasename)
	if _, err := os.Stat(gguf); err != nil {
		if url := embedGGUFDownloadURL(); url != "" {
			if _, err := downloadResumable(ctx, url, gguf); err != nil {
				return "", err
			}
		}
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("llama-server binary not available for %s (pass an endpoint in the wizard)", osArch())
	}
	if _, err := os.Stat(gguf); err != nil {
		return "", fmt.Errorf("embedding GGUF not available (confirm the HF URL in sidecar_install.go)")
	}
	if err := launchDaemon(ctx, bin, dataDir, "llama-server",
		"-m", gguf, "--mmproj", filepath.Join(models, embedMMProjName),
		"--embedding", "--host", "127.0.0.1", "--port", "8081", "-ngl", "0",
		"--video-ffmpeg-dir", installFFmpegPathHint(dataDir)); err != nil {
		return "", err
	}
	return defaultLlamaEndpoint, waitForHealthy(ctx, defaultLlamaEndpoint, "/v1/models")
}

// installFFmpeg detects a system ffmpeg first; otherwise downloads a per-OS
// static build. Returns the ffmpeg path.
func installFFmpeg(ctx context.Context, dataDir string) (string, error) {
	if p := findSystemFFmpeg(); p != "" {
		return p, nil
	}
	dir := SidecarDir(dataDir, KindFFmpeg)
	bin := filepath.Join(dir, "ffmpeg")
	if _, err := os.Stat(bin); err != nil {
		if url := ffmpegDownloadURL(); url != "" {
			if _, err := downloadResumable(ctx, url, filepath.Join(dir, "ffmpeg-artifact")); err != nil {
				return "", err
			}
			if err := extractArtifact(filepath.Join(dir, "ffmpeg-artifact"), dir, "ffmpeg"); err != nil {
				return "", err
			}
		}
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("ffmpeg not available for %s and none found on PATH", osArch())
	}
	return bin, nil
}

// installFFmpegPathHint returns the local ffmpeg path for llama-server's
// --video-ffmpeg-dir (best effort; ffmpeg is on PATH otherwise).
func installFFmpegPathHint(dataDir string) string {
	return SidecarDir(dataDir, KindFFmpeg)
}

// extractArtifact extracts a downloaded archive (by extension) into destDir and
// ensures the named binary is at destDir/<name>.
func extractArtifact(artifact, destDir, name string) error {
	switch strings.ToLower(filepath.Ext(artifact)) {
	case ".gz":
		if _, err := extractTarGz(artifact, destDir); err != nil {
			return err
		}
	case ".zip":
		if err := extractZip(artifact, destDir); err != nil {
			return err
		}
	default:
		// A bare binary: rename into place.
		if err := os.Rename(artifact, filepath.Join(destDir, name)); err != nil {
			return err
		}
		return nil
	}
	// Find the binary anywhere under destDir and move it to destDir/<name>.
	found := ""
	_ = filepath.Walk(destDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || found != "" {
			return nil
		}
		base := filepath.Base(p)
		if base == name || base == name+".exe" || base == "ffmpeg" && name == "ffmpeg" {
			found = p
		}
		return nil
	})
	if found == "" {
		return fmt.Errorf("binary %q not found in %s", name, destDir)
	}
	if found != filepath.Join(destDir, name) {
		if err := os.Rename(found, filepath.Join(destDir, name)); err != nil {
			return err
		}
	}
	if runtime.GOOS == "windows" {
		// Ensure the .exe name for Windows.
		if _, err := os.Stat(filepath.Join(destDir, name+".exe")); err != nil {
			_ = os.Rename(filepath.Join(destDir, name), filepath.Join(destDir, name+".exe"))
			return nil
		}
	}
	return nil
}

// ---- process launch (daemon) + health wait ----

// launchDaemon starts a long-lived sidecar process, logging to <dataDir>/sidecars
// and recording its PID. It does not block on the process.
func launchDaemon(ctx context.Context, bin string, dataDir, name string, args ...string) error {
	if p, err := os.Executable(); err == nil {
		_ = p
	}
	logPath := filepath.Join(SidecarDir(dataDir, name), name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	c := exec.Command(bin, args...)
	c.Stdout = logFile
	c.Stderr = logFile
	if err := c.Start(); err != nil {
		logFile.Close()
		return err
	}
	if err := os.WriteFile(filepath.Join(SidecarDir(dataDir, name), name+".pid"), []byte(fmt.Sprint(c.Process.Pid)), 0o644); err != nil {
		logFile.Close()
		return err
	}
	return nil
}

// waitForHealthy polls the endpoint+path until it returns <500 (or the budget
// elapses). Used to functionally verify a freshly-launched sidecar.
func waitForHealthy(ctx context.Context, endpoint, path string) error {
	url := strings.TrimRight(endpoint, "/") + path
	deadline := time.Now().Add(90 * time.Second)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := client.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("sidecar at %s did not become healthy in time", endpoint)
}

// ---- download URL builders (the one place to fix if a release renames) ----

func qdrantDownloadURL() string {
	arch := runtime.GOARCH
	if runtime.GOARCH == "amd64" {
		arch = "x86_64"
	}
	switch runtime.GOOS {
	case "linux":
		return fmt.Sprintf("https://github.com/qdrant/qdrant/releases/download/v%s/qdrant-%s-unknown-linux-gnu.tar.gz", qdrantVersion, arch)
	case "windows":
		return fmt.Sprintf("https://github.com/qdrant/qdrant/releases/download/v%s/qdrant-%s-pc-windows-msvc.zip", qdrantVersion, arch)
	}
	return ""
}

func llamaDownloadURL() string {
	switch runtime.GOOS {
	case "linux":
		return fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/llama-%s-bin-ubuntu-x64.zip", llamaBuild, llamaBuild)
	case "windows":
		return fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/llama-%s-bin-win-x64.zip", llamaBuild, llamaBuild)
	}
	return ""
}

func ffmpegDownloadURL() string {
	switch runtime.GOOS {
	case "linux":
		// Static builds from johnvansickle.com (commonly mirrored).
		return "https://johnvansickle.com/ffmpeg/builds/ffmpeg-release-amd64-static.tar.xz"
	case "windows":
		return "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	}
	return ""
}

func embedGGUFDownloadURL() string {
	// Best-known HF path; confirm at first real run (per setup plan §6).
	return fmt.Sprintf("https://huggingface.co/%s/embeddinggemma-2/resolve/main/%s", embedGGUFRepo, embedGGUFBasename)
}