package setup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sandboxProgressWriter counts bytes and reports throttled progress
// (250 ms — the same cadence as the sidecar downloads).
type sandboxProgressWriter struct {
	w        io.Writer
	total    int64
	written  int64
	last     time.Time
	progress func(frac float64, detail string)
}

func (p *sandboxProgressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	now := time.Now()
	if p.last.IsZero() || now.Sub(p.last) >= 250*time.Millisecond {
		p.last = now
		frac := 0.0
		if p.total > 0 {
			frac = float64(p.written) / float64(p.total)
			if frac > 1 {
				frac = 1
			}
		}
		if p.progress != nil {
			detail := "downloading (" + humanBytes(p.written)
			if p.total > 0 {
				detail += " of " + humanBytes(p.total)
			}
			detail += ")"
			p.progress(frac, detail)
		}
	}
	return n, err
}

// isHTMLBody reports whether a response body that was expected to be
// JSON is actually an HTML page. A 200 HTML body is the signature of a
// network filter / proxy / captive portal serving its own page (a
// challenge or block page) for the registry's URL — without the check
// the failure surfaces deep in the code as
// "invalid character '<' looking for beginning of value" (json.Unmarshal
// on a "<!doctype html>..." body), which points at nothing.
func isHTMLBody(body []byte, contentType string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/html") {
		return true
	}
	b := bytes.TrimLeft(body, " \t\r\n\xef\xbb\xbf")
	if len(b) == 0 {
		return false
	}
	head := b
	if len(head) > 64 {
		head = head[:64]
	}
	return strings.HasPrefix(strings.ToLower(string(head)), "<")
}

// extractTarLayer extracts a registry image layer into destDir. A layer
// is a tar, optionally gzip-compressed — detected by the gzip magic
// bytes (0x1f 0x8b): MCR historically served windows layers UNCOMPRESSED
// (tar), and now serves them as .tar.gzip (verified against the live
// ltsc2025 image 2026-11-18), so both must work — the mediaType in the
// manifest agrees, but the magic check is the robust signal.
func extractTarLayer(archive, destDir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := writeRootfsTarEntry(destDir, hdr, tr); err != nil {
			return err
		}
	}
	return nil
}

// writeRootfsTarEntry extracts one tar entry, for a real OS image
// (shared by both platforms' base provisioners): it handles symlinks +
// hard links (a rootfs has them — /bin/sh → dash, /lib → /usr/lib; the
// Windows layers have them too) and preserves the entries' modes (setuid
// bits, exec bits). The sidecar installer's writeTarEntry is not reused
// here on purpose (its blanket 0755 + no-symlink behavior is right for
// release artifacts, wrong for an OS).
func writeRootfsTarEntry(destDir string, hdr *tar.Header, r io.Reader) error {
	target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
	if target != filepath.Clean(destDir) && !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe tar path: %s", hdr.Name) // path traversal guard
	}
	mode := os.FileMode(hdr.Mode).Perm()
	switch hdr.Typeflag {
	case tar.TypeDir:
		if mode == 0 {
			mode = 0o755
		}
		return os.MkdirAll(target, mode)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, r)
		return err
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink: // hard link
		linkTarget := filepath.Join(destDir, filepath.FromSlash(hdr.Linkname))
		if !strings.HasPrefix(linkTarget, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe hardlink target: %s", hdr.Linkname)
		}
		os.Remove(target)
		return os.Link(linkTarget, target)
	}
	return nil
}

// sandboxDownload streams url to dest (a partial <dest>.part is renamed
// into place only when the full body arrived — a canceled download never
// leaves a broken final file). progress is nil-safe.
func sandboxDownload(ctx context.Context, url, dest string, progress func(frac float64, detail string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	part := dest + ".part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	pw := &sandboxProgressWriter{w: out, total: resp.ContentLength, progress: progress}
	written, err := io.Copy(pw, resp.Body)
	cerr := out.Close()
	if err != nil {
		os.Remove(part)
		return err
	}
	if cerr != nil {
		os.Remove(part)
		return cerr
	}
	if resp.ContentLength > 0 && written != resp.ContentLength {
		os.Remove(part)
		return fmt.Errorf("incomplete download (%d of %d bytes)", written, resp.ContentLength)
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return err
	}
	return nil
}
