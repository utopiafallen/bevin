//go:build linux

package setup

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

// linuxBaseURL is the OS the per-user containers are created from: the
// Ubuntu 26.04 LTS (resolute) cloud rootfs — the latest LTS server
// release (user directive: track the latest Ubuntu LTS; a new bevin
// release bumps this pin when Canonical cuts a new one) — a minimal full
// OS WITH systemd (systemd-nspawn --boot needs an init inside) — ~40 MB
// compressed. The "current" alias is pinned: a stable URL Canonical keeps
// updated within the resolute series (point releases).
const (
	linuxBaseURL = "https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64-root.tar.xz"
	// linuxBaseName / linuxBaseSHA256: the published checksums file
	// (same directory as the artifact). The digest lets a FRESH download
	// be verified, and an already-downloaded archive (a run that died
	// between download and extraction) be reused WITHOUT re-downloading.
	linuxBaseName   = "resolute-server-cloudimg-amd64-root.tar.xz"
	linuxBaseSHA256 = "https://cloud-images.ubuntu.com/resolute/current/SHA256SUMS"
)

// sandboxBaseReady reports whether the linux base rootfs is provisioned.
func sandboxBaseReady(dataDir string) bool {
	_, err := os.Stat(filepath.Join(sandbox.BaseRoot(dataDir), "etc", "os-release"))
	return err == nil
}

// provisionSandboxBase downloads + extracts the base rootfs into
// <data>/sandboxes/base/root/. It is atomic: the archive + staging dir
// live outside the final location, the result is verified, and only then
// renamed into place — a canceled/failed run never leaves a broken base.
// The VERIFIED archive is kept on disk after a successful run: the next
// re-provision (if the base root is ever deleted) reuses it — verified
// against the published digest — instead of re-downloading.
func provisionSandboxBase(ctx context.Context, dataDir string, progress func(frac float64, detail string)) error {
	baseRoot := sandbox.BaseRoot(dataDir)
	staging := baseRoot + ".staging"
	archive := filepath.Join(sandbox.SandboxRoot(dataDir), ".base.tar.xz")
	keepArchive := false
	_ = os.RemoveAll(staging)
	defer func() {
		os.RemoveAll(staging)
		if !keepArchive {
			os.Remove(archive)
		}
	}()

	// 1. The published digest (a few KB, same host as the artifact).
	want, err := publishedSHA256(ctx, linuxBaseSHA256, linuxBaseName)
	if err != nil {
		return fmt.Errorf("fetching the base image's published checksum: %w", err)
	}
	// 2. An already-downloaded archive that verifies is reused (skip the
	// download); one that doesn't is stale/corrupt — re-download.
	verified := false
	if fi, err := os.Stat(archive); err == nil && fi.Size() > 0 {
		if progress != nil {
			progress(0.05, "verifying existing "+humanBytes(fi.Size())+" archive…")
		}
		if got, err := fileSHA256Hex(archive); err == nil && got == want {
			verified = true
			if progress != nil {
				progress(0.6, "archive already downloaded and verified — skipping the download")
			}
		} else {
			os.Remove(archive)
		}
	}
	// 3. Download (0-60% of the progress budget) — fresh downloads are
	// verified against the published digest too.
	if !verified {
		if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
			return err
		}
		if err := sandboxDownload(ctx, linuxBaseURL, archive, func(frac float64, detail string) {
			if progress != nil {
				progress(frac*0.6, detail)
			}
		}); err != nil {
			return fmt.Errorf("downloading the base image: %w", err)
		}
		if got, err := fileSHA256Hex(archive); err != nil || got != want {
			return fmt.Errorf("the downloaded base image's checksum does not match Canonical's published one")
		}
	}
	// 4. Extract (60-95%).
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	if progress != nil {
		progress(0.6, "extracting the base image…")
	}
	if err := extractTarXz(archive, staging, func(frac float64) {
		if progress != nil {
			progress(0.6+frac*0.35, "extracting the base image…")
		}
	}); err != nil {
		return fmt.Errorf("extracting the base image: %w", err)
	}
	// 5. Verify.
	if _, err := os.Stat(filepath.Join(staging, "etc", "os-release")); err != nil {
		return fmt.Errorf("the downloaded base image is invalid (no /etc/os-release inside)")
	}
	// 6. Move into place (same filesystem → rename is atomic).
	if err := os.RemoveAll(baseRoot); err != nil {
		return err
	}
	if err := os.Rename(staging, baseRoot); err != nil {
		return err
	}
	keepArchive = true
	if progress != nil {
		progress(1, "base image ready")
	}
	return nil
}

// extractTarXz streams `xz -dc archive` (the native tool — compress/xz is
// not in the Go stdlib) into the tar reader and extracts into destDir.
func extractTarXz(archive, destDir string, progress func(frac float64)) error {
	cmd := exec.Command("xz", "-dc", archive)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errBuf errWriter
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("the xz tool is not available (install xz-utils): %w", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	tr := tar.NewReader(out)
	total := int64(0)
	got := int64(0)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if e := errBuf.String(); e != "" {
				return fmt.Errorf("xz: %s", e)
			}
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			total += hdr.Size
			got += hdr.Size
			if progress != nil && total > 0 && hdr.Size >= (1<<20) {
				progress(float64(got) / float64(total))
			}
		}
		if err := writeRootfsTarEntry(destDir, hdr, tr); err != nil {
			return err
		}
	}
	return nil
}

// errWriter collects a command's stderr for error reporting (bounded).
type errWriter struct{ b []byte }

func (w *errWriter) Write(p []byte) (int, error) {
	if len(w.b) < 4096 {
		w.b = append(w.b, p...)
	}
	return len(p), nil
}

func (w *errWriter) String() string { return string(w.b) }
