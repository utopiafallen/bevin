//go:build linux

package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

// TestProvisionSandboxBaseLiveReuse exercises the REAL linux base
// provisioner end-to-end against cloud-images.ubuntu.com (~40 MB
// download — env-gated so it is skipped in normal runs):
//
//   - run 1: fresh — fetches the published digest, downloads the
//     archive, verifies it, extracts it, and KEEPS the verified archive.
//   - the base root is deleted (the archive is kept).
//   - run 2: must REUSE the verified archive — no re-download.
func TestProvisionSandboxBaseLiveReuse(t *testing.T) {
	if os.Getenv("BEVIN_E2E_BASE") == "" {
		t.Skip("set BEVIN_E2E_BASE=1 to run the live base download test (~40 MB)")
	}
	dataDir := t.TempDir()

	// Run 1: fresh download + extract.
	var d1 []string
	if err := provisionSandboxBase(context.Background(), dataDir, func(_ float64, detail string) {
		d1 = append(d1, detail)
	}); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if !sandboxBaseReady(dataDir) {
		t.Fatal("base not ready after the first provision")
	}
	archive := filepath.Join(sandbox.SandboxRoot(dataDir), ".base.tar.xz")
	if fi, err := os.Stat(archive); err != nil || fi.Size() == 0 {
		t.Fatalf("the verified archive was not kept after a successful run: %v", err)
	}

	// Delete the base root, keep the archive.
	if err := os.RemoveAll(sandbox.BaseRoot(dataDir)); err != nil {
		t.Fatal(err)
	}
	if sandboxBaseReady(dataDir) {
		t.Fatal("base still reports ready after deleting its root")
	}

	// Run 2: must reuse the archive (verified) — no re-download.
	var d2 []string
	if err := provisionSandboxBase(context.Background(), dataDir, func(_ float64, detail string) {
		d2 = append(d2, detail)
	}); err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if !sandboxBaseReady(dataDir) {
		t.Fatal("base not ready after the second provision")
	}
	sawSkip, sawDownload := false, false
	for _, d := range d2 {
		if strings.Contains(d, "skipping the download") {
			sawSkip = true
		}
		if strings.HasPrefix(d, "downloading (") {
			sawDownload = true
		}
	}
	if !sawSkip {
		t.Fatalf("the second run did not report reusing the archive: %q", d2)
	}
	if sawDownload {
		t.Fatalf("the second run re-downloaded despite a verified archive: %q", d2)
	}
}
