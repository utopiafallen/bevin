package setup

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Release artifacts — Qdrant + llama.cpp are resolved at setup time by querying
// the GitHub releases API for the pinned version's assets + matching by OS/arch
// + format (robust to an asset rename; the download already needs the network,
// so this one extra call is not a new failure mode). The versions are pinned
// (qdrant v1.20.0, llama.cpp build b12230) so the download is reproducible. The
// embedder (embeddinggemma-2 GGUF) comes from HuggingFace (not a GitHub release).
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

	// Default loopback ports — the starting point when picking a free port if a
	// sidecar is auto-provisioned (the server is free to move off the default
	// when it is already taken).
	defaultQdrantPort = 6333
	defaultLlamaPort  = 8081
)

// ---- download progress reporting ----

// sidecarReport, when non-nil, receives live progress for a sidecar while it is
// being provisioned: status is one of pending|downloading|starting|ready|error,
// message is a human-readable detail (which file, byte %, etc.), and percent is
// a 0-100 download progress (0 when the total size is unknown). The setup
// Manager sets it for the duration of a provisioning run so the wizard can
// stream it. The real install functions call report() (phase transitions +
// download bytes); an injected fake install (tests) simply doesn't.
var sidecarReport func(kind, status, message string, percent int)

// report is a nil-safe helper the install functions use to report a phase.
func report(kind, status, message string, percent int) {
	if sidecarReport != nil {
		sidecarReport(kind, status, message, percent)
	}
}

// downloadProgress is a thin io.Writer that counts bytes and reports download
// progress (throttled to a few updates per second) via report(). got = bytes in
// the final file so far (resume offset + bytes written this pass); total = the
// expected final size (0 when unknown → the wizard shows an indeterminate bar).
type downloadProgress struct {
	w       io.Writer
	kind    string
	file    string
	total   int64
	got0    int64 // bytes already in the file before this pass (resume offset)
	written int64
	last    time.Time
}

func (p *downloadProgress) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	now := time.Now()
	if p.last.IsZero() || now.Sub(p.last) >= 250*time.Millisecond {
		p.last = now
		got := p.got0 + p.written
		pct := 0
		if p.total > 0 {
			pct = int(got * 100 / p.total)
			if pct > 100 {
				pct = 100
			}
		}
		msg := "downloading " + p.file
		if p.total > 0 {
			msg += fmt.Sprintf(" (%d%%, %s of %s)", pct, humanBytes(got), humanBytes(p.total))
		} else {
			msg += " (" + humanBytes(got) + ")"
		}
		report(p.kind, "downloading", msg, pct)
	}
	return n, err
}

// humanBytes renders a byte count in a compact form (e.g. "12.3 MB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---- download (resumable, atomic, cached) ----

// downloadResumable fetches url to dest, resuming a partial <dest>.part via
// HTTP Range. The file is written to <dest>.part then atomically renamed to
// dest, so a partial download is never a broken final file. It returns the
// number of bytes in the final file. kind + fileLabel are used for progress
// reporting (fileLabel is the human-readable name shown in the wizard).
func downloadResumable(ctx context.Context, url, dest string, kind, fileLabel string) (int64, error) {
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
	// Expected final size: Content-Length for a fresh download, or the "/total"
	// of Content-Range for a resume (0 when the server doesn't say →
	// indeterminate progress).
	total := int64(0)
	if cl, perr := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); perr == nil {
		if resp.StatusCode == http.StatusOK {
			total = cl + start
		}
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.IndexByte(cr, '/'); i >= 0 {
			if t, perr := strconv.ParseInt(cr[i+1:], 10, 64); perr == nil {
				total = t
			}
		}
	}
	pw := &downloadProgress{w: f, kind: kind, file: fileLabel, total: total, got0: start}
	written, err := io.Copy(pw, resp.Body)
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
func installQdrant(ctx context.Context, dataDir string, preferredPort int) (string, error) {
	dir := SidecarDir(dataDir, KindQdrant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	bin := findBinary(dir, "qdrant")
	if bin == "" {
		url, err := qdrantDownloadURL(ctx)
		if err != nil {
			return "", fmt.Errorf("qdrant release lookup: %w", err)
		}
		if url == "" {
			return "", fmt.Errorf("no qdrant release known for %s (pass an endpoint in the wizard)", osArch())
		}
		artifact := filepath.Join(dir, "qdrant-artifact"+filepath.Ext(url))
		if _, err := downloadResumable(ctx, url, artifact, KindQdrant, "Qdrant"); err != nil {
			return "", err
		}
		if err := extractArtifact(artifact, dir); err != nil {
			return "", err
		}
		bin = findBinary(dir, "qdrant")
		if bin == "" {
			return "", fmt.Errorf("qdrant binary not found after extract in %s", dir)
		}
	}
	// Pick free loopback ports (REST + gRPC), preferring the sidecar's existing
	// port (so a re-launch stays on its ports when they're still free) or the
	// defaults when freshly auto-provisioned.
	httpPort := preferredPort
	if httpPort <= 0 {
		httpPort = defaultQdrantPort
	}
	httpPort, err := pickFreePort(httpPort)
	if err != nil {
		return "", err
	}
	grpcPort, err := pickFreePort(httpPort + 1)
	if err != nil {
		return "", err
	}
	endpoint := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	// Loopback config (verified schema: service.host binds REST + gRPC).
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "service:\n" +
		"  host: 127.0.0.1\n" +
		"  http_port: " + strconv.Itoa(httpPort) + "\n" +
		"  grpc_port: " + strconv.Itoa(grpcPort) + "\n" +
		"  enable_cors: true\n" +
		"storage:\n" +
		"  storage_path: " + filepath.Join(dir, "storage") + "\n" +
		"telemetry:\n" +
		"  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		return "", err
	}
	report(KindQdrant, "starting", "Starting Qdrant…", 100)
	if err := launchDaemon(ctx, bin, dataDir, KindQdrant, "--config-path", cfgPath); err != nil {
		return "", err
	}
	return endpoint, waitForHealthy(ctx, endpoint, "/collections")
}

// installLlamaServer downloads the llama-server binary (with its bundled libs)
// + the embeddinggemma-2 GGUF + mmproj, and launches it in embedding mode on
// loopback, CPU-only. Verified: /v1/models reports multimodal input_modalities
// and /v1/embeddings returns a 512-dim vector.
func installLlamaServer(ctx context.Context, dataDir string, preferredPort int) (string, error) {
	dir := SidecarDir(dataDir, KindLlamaServer)
	models := filepath.Join(dir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		return "", err
	}
	// Binary: launch in place so its $ORIGIN rpath resolves the bundled .so libs.
	bin := findBinary(dir, "llama-server")
	if bin == "" {
		url, err := llamaDownloadURL(ctx)
		if err != nil {
			return "", fmt.Errorf("llama-server release lookup: %w", err)
		}
		if url != "" {
			artifact := filepath.Join(dir, "llama-artifact"+filepath.Ext(url))
			if _, err := downloadResumable(ctx, url, artifact, KindLlamaServer, "llama-server"); err != nil {
				return "", err
			}
			if err := extractArtifact(artifact, dir); err != nil {
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
		if _, err := downloadResumable(ctx, embedDownloadURL(embedGGUFName), gguf, KindLlamaServer, "embedding model"); err != nil {
			return "", err
		}
	}
	mmproj := filepath.Join(models, embedMMProjName)
	if _, err := os.Stat(mmproj); err != nil {
		if _, err := downloadResumable(ctx, embedDownloadURL(embedMMProjName), mmproj, KindLlamaServer, "multimodal projector"); err != nil {
			return "", err
		}
	}
	// Pick a free loopback port, preferring the sidecar's existing port (so a
	// re-launch stays on its port when it's still free) or the default when
	// freshly auto-provisioned.
	port := preferredPort
	if port <= 0 {
		port = defaultLlamaPort
	}
	port, err := pickFreePort(port)
	if err != nil {
		return "", err
	}
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port)
	// CPU-only, loopback, embedding mode. Video input uses the system ffmpeg
	// (already detected by installFFmpeg; llama-server finds it on PATH).
	args := []string{
		"-m", gguf, "--mmproj", mmproj, "--embedding",
		"--host", "127.0.0.1", "--port", strconv.Itoa(port), "-ngl", "0",
	}
	report(KindLlamaServer, "starting", "Starting llama-server (the model load can take a couple minutes)…", 100)
	if err := launchDaemon(ctx, bin, dataDir, KindLlamaServer, args...); err != nil {
		return "", err
	}
	return endpoint, waitForHealthy(ctx, endpoint, "/v1/models")
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
			artifact := filepath.Join(dir, "ffmpeg-artifact"+filepath.Ext(url))
			if _, err := downloadResumable(ctx, url, artifact, KindFFmpeg, "ffmpeg"); err != nil {
				return "", err
			}
			if err := extractArtifact(artifact, dir); err != nil {
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

// pickFreePort returns a free loopback TCP port, preferring `start` (when free)
// and scanning upward otherwise. It opens a real listener to test availability.
// The window between closing the listener and the sidecar's own bind is tiny; if
// the port is snatched in that window, the sidecar's bind fails and the health
// check catches it (the supervisor retries next tick).
func pickFreePort(start int) (int, error) {
	for p := start; p < start+100; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			_ = l.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free loopback port in %d..%d", start, start+99)
}

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

// ---- release artifact resolution (GitHub releases API, robust to a rename) ----

// githubReleaseAsset queries the GitHub releases API for a release's assets and
// returns the browser_download_url of the first asset matching `match`. It makes
// the sidecar download robust to an asset rename (we match by OS/arch + format,
// not a hardcoded name). The download already needs the network, so this one
// extra call is not a new failure mode.
func githubReleaseAsset(ctx context.Context, repo, tag string, match func(string) bool) (string, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases API %s: status %d", apiURL, resp.StatusCode)
	}
	var rel struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("decode github releases API %s: %w", apiURL, err)
	}
	for _, a := range rel.Assets {
		if match(a.Name) {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("no release asset matching for %s %s", repo, tag)
}

// qdrantDownloadURL resolves the Qdrant release artifact for this OS/arch by
// querying the GitHub releases API + matching the asset (x86_64 + OS + format).
func qdrantDownloadURL(ctx context.Context) (string, error) {
	var match func(string) bool
	switch runtime.GOOS {
	case "linux":
		match = func(n string) bool {
			return strings.Contains(n, "x86_64") && strings.Contains(n, "linux") && strings.HasSuffix(n, ".tar.gz")
		}
	case "windows":
		match = func(n string) bool {
			return strings.Contains(n, "x86_64") && strings.Contains(n, "windows") && strings.HasSuffix(n, ".zip")
		}
	default:
		return "", nil
	}
	return githubReleaseAsset(ctx, "qdrant/qdrant", "v"+qdrantVersion, match)
}

// llamaDownloadURL resolves the llama.cpp release artifact for this OS/arch by
// querying the GitHub releases API + matching the asset (x64 + OS + format).
func llamaDownloadURL(ctx context.Context) (string, error) {
	var match func(string) bool
	switch runtime.GOOS {
	case "linux":
		match = func(n string) bool {
			return strings.Contains(n, "ubuntu-x64") && strings.HasSuffix(n, ".tar.gz")
		}
	case "windows":
		match = func(n string) bool {
			return strings.Contains(n, "win-cpu-x64") && strings.HasSuffix(n, ".zip")
		}
	default:
		return "", nil
	}
	return githubReleaseAsset(ctx, "ggml-org/llama.cpp", llamaBuild, match)
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
