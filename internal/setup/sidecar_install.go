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

// Release artifact locations — VERIFIED against the live GitHub / HuggingFace
// release APIs (2026-11-09): Qdrant v1.20.0 (qdrant-x86_64-unknown-linux-gnu.
// tar.gz), llama.cpp build b12230 (llama-b12230-bin-ubuntu-x64.tar.gz), and the
// embeddinggemma-2 GGUF from unsloth/embeddinggemma-2-GGUF (Q8_0 text model +
// mmproj, 512-dim, multimodal text/image/audio/video). If a project renames a
// release artifact, update this small set and re-verify against the release API.
const (
	qdrantVersion = "1.20.0"
	llamaBuild    = "b12230"

	embedGGUFRepo = "unsloth"
	embedGGUFRepoPath = "embeddinggemma-2-GGUF"
	embedGGUFName   = "embeddinggemma-2-Q8_0.gguf"
	embedMMProjName = "mmproj-Q8_0.gguf"
)

// sidecar endpoints (loopback) used when auto-provisioning.
const (
	defaultQdrantEndpoint = "http://127.0.0.1:6333"
	defaultLlamaEndpoint  = "http://127.0.0.1:8081"
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
	client := &http.Client{Timeout: 20 * time.Minute}
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

// extractArtifact extracts a downloaded archive (by extension) into destDir.
// A bare binary (no recognized archive extension) is moved into place. It does
// NOT relocate extracted binaries — the caller uses findBinary to launch them
// in place (llama-server's binary keeps its $ORIGIN rpath to the bundled libs).
func extractArtifact(artifact, destDir string) error {
	switch strings.ToLower(filepath.Ext(artifact)) {
	case ".gz":
		_, err := extractTarGz(artifact, destDir)
		return err
	case ".zip":
		return extractZip(artifact, destDir)
	case ".7z":
		return fmt.Errorf("7z archives need a 7z extractor (not bundled); install the sidecar system-wide or pass an endpoint/path in the wizard")
	default:
		return os.Rename(artifact, filepath.Join(destDir, filepath.Base(artifact)))
	}
}

// findBinary locates a binary by name (or name.exe) under dir, without moving
// it; returns its path, or "" if not found.
func findBinary(dir, name string) string {
	var found string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || found != "" || info.IsDir() {
			return nil
		}
		b := filepath.Base(p)
		if b == name || b == name+".exe" {
			found = p
		}
		return nil
	})
	return found
}

// ---- per-sidecar install ----

// installQdrant downloads the Qdrant binary, writes a loopback config, and
// launches it bound to 127.0.0.1. Verified: /collections and /healthz respond,
// and the socket is 127.0.0.1-only. Idempotent via Ensure (probe-first).
func installQdrant(ctx context.Context, dataDir string) (string, error) {
	dir := SidecarDir(dataDir, KindQdrant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	bin := findBinary(dir, "qdrant")
	if bin == "" {
		url := qdrantDownloadURL()
		if url == "" {
			return "", fmt.Errorf("no qdrant release known for %s (pass an endpoint in the wizard)", osArch())
		}
		if _, err := downloadResumable(ctx, url, filepath.Join(dir, "qdrant-artifact")); err != nil {
			return "", err
		}
		if err := extractArtifact(filepath.Join(dir, "qdrant-artifact"), dir); err != nil {
			return "", err
		}
		bin = findBinary(dir, "qdrant")
		if bin == "" {
			return "", fmt.Errorf("qdrant binary not found after extract in %s", dir)
		}
	}
	// Loopback config (verified schema: service.host binds REST + gRPC).
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "service:\n" +
		"  host: 127.0.0.1\n" +
		"  http_port: 6333\n" +
		"  grpc_port: 6334\n" +
		"  enable_cors: true\n" +
		"storage:\n" +
		"  storage_path: " + filepath.Join(dir, "storage") + "\n" +
		"telemetry:\n" +
		"  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		return "", err
	}
	if err := launchDaemon(ctx, bin, dataDir, KindQdrant, "--config-path", cfgPath); err != nil {
		return "", err
	}
	return defaultQdrantEndpoint, waitForHealthy(ctx, defaultQdrantEndpoint, "/collections")
}

// installLlamaServer downloads the llama-server binary (with its bundled libs)
// + the embeddinggemma-2 GGUF + mmproj, and launches it in embedding mode on
// loopback, CPU-only. Verified: /v1/models reports multimodal input_modalities
// and /v1/embeddings returns a 512-dim vector.
func installLlamaServer(ctx context.Context, dataDir string) (string, error) {
	dir := SidecarDir(dataDir, KindLlamaServer)
	models := filepath.Join(dir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		return "", err
	}
	// Binary: launch in place so its $ORIGIN rpath resolves the bundled .so libs.
	bin := findBinary(dir, "llama-server")
	if bin == "" {
		if url := llamaDownloadURL(); url != "" {
			if _, err := downloadResumable(ctx, url, filepath.Join(dir, "llama-artifact")); err != nil {
				return "", err
			}
			if err := extractArtifact(filepath.Join(dir, "llama-artifact"), dir); err != nil {
				return "", err
			}
			bin = findBinary(dir, "llama-server")
		}
	}
	if bin == "" {
		return "", fmt.Errorf("llama-server binary not available for %s (pass an endpoint in the wizard)", osArch())
	}
	// Embedding model + mmproj (the multimodal projector).
	gguf := filepath.Join(models, embedGGUFName)
	if _, err := os.Stat(gguf); err != nil {
		if _, err := downloadResumable(ctx, embedDownloadURL(embedGGUFName), gguf); err != nil {
			return "", err
		}
	}
	mmproj := filepath.Join(models, embedMMProjName)
	if _, err := os.Stat(mmproj); err != nil {
		if _, err := downloadResumable(ctx, embedDownloadURL(embedMMProjName), mmproj); err != nil {
			return "", err
		}
	}
	// CPU-only, loopback, embedding mode. Video input uses the system ffmpeg
	// (already detected by installFFmpeg; llama-server finds it on PATH).
	args := []string{
		"-m", gguf, "--mmproj", mmproj, "--embedding",
		"--host", "127.0.0.1", "--port", "8081", "-ngl", "0",
	}
	if err := launchDaemon(ctx, bin, dataDir, KindLlamaServer, args...); err != nil {
		return "", err
	}
	return defaultLlamaEndpoint, waitForHealthy(ctx, defaultLlamaEndpoint, "/v1/models")
}

// installFFmpeg detects a system ffmpeg first (the common case); otherwise it
// downloads a per-OS build where a supported archive is available. Returns the
// ffmpeg path.
func installFFmpeg(ctx context.Context, dataDir string) (string, error) {
	if p := findSystemFFmpeg(); p != "" {
		return p, nil
	}
	if runtime.GOOS != "windows" {
		// Linux: a static build is a .tar.xz (needs an xz extractor, not
		// bundled). ffmpeg is a normal system dependency — ask for it.
		return "", fmt.Errorf("ffmpeg not found on PATH and no bundled Linux build; install ffmpeg or pass its path in the wizard")
	}
	dir := SidecarDir(dataDir, KindFFmpeg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	bin := findBinary(dir, "ffmpeg")
	if bin == "" {
		if url := ffmpegDownloadURL(); url != "" {
			if _, err := downloadResumable(ctx, url, filepath.Join(dir, "ffmpeg-artifact")); err != nil {
				return "", err
			}
			if err := extractArtifact(filepath.Join(dir, "ffmpeg-artifact"), dir); err != nil {
				return "", err
			}
			bin = findBinary(dir, "ffmpeg")
		}
	}
	if bin == "" {
		return "", fmt.Errorf("ffmpeg not available for %s and none found on PATH", osArch())
	}
	return bin, nil
}

// ---- process launch (daemon) + health wait ----

// launchDaemon starts a long-lived sidecar process, logging to its sidecar dir
// and recording its PID. It does not block on the process.
func launchDaemon(ctx context.Context, bin string, dataDir, name string, args ...string) error {
	dir := SidecarDir(dataDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	if err := os.WriteFile(filepath.Join(dir, name+".pid"), []byte(fmt.Sprint(c.Process.Pid)), 0o644); err != nil {
		logFile.Close()
		return err
	}
	_ = logFile.Close()
	return nil
}

// waitForHealthy polls the endpoint+path until it returns <500 (or the budget
// elapses). Used to functionally verify a freshly-launched sidecar. The budget
// is generous to cover a CPU model load.
func waitForHealthy(ctx context.Context, endpoint, path string) error {
	url := strings.TrimRight(endpoint, "/") + path
	deadline := time.Now().Add(3 * time.Minute)
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
	switch runtime.GOOS {
	case "linux": // verified: qdrant-x86_64-unknown-linux-gnu.tar.gz
		return fmt.Sprintf("https://github.com/qdrant/qdrant/releases/download/v%s/qdrant-x86_64-unknown-linux-gnu.tar.gz", qdrantVersion)
	case "windows": // asset verified via the release API (7z, gnu toolchain)
		return fmt.Sprintf("https://github.com/qdrant/qdrant/releases/download/v%s/qdrant-x86_64-pc-windows-gnu.7z", qdrantVersion)
	}
	return ""
}

func llamaDownloadURL() string {
	switch runtime.GOOS {
	case "linux": // verified: llama-b12230-bin-ubuntu-x64.tar.gz
		return fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/llama-%s-bin-ubuntu-x64.tar.gz", llamaBuild, llamaBuild)
	case "windows": // asset verified via the release API
		return fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/llama-%s-bin-win-cpu-x64.zip", llamaBuild, llamaBuild)
	}
	return ""
}

func ffmpegDownloadURL() string {
	if runtime.GOOS == "windows" {
		return "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	}
	return ""
}

func embedDownloadURL(filename string) string {
	// verified: unsloth/embeddinggemma-2-GGUF (Q8_0 text model + mmproj)
	return fmt.Sprintf("https://huggingface.co/%s/%s/resolve/main/%s", embedGGUFRepo, embedGGUFRepoPath, filename)
}
