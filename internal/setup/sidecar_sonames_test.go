package setup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestLinkSonames — the llama.cpp release tarball ships only full-version .so
// files (libllama-common.so.0.9.0) while the binaries link against the
// unversioned sonames (libllama-common.so.0); without the links the binary
// dies at exec with "error while loading shared libraries".
func TestLinkSonames(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only (ELF sonames)")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "llama-b1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"libllama.so.0.9.0", "libllama-common.so.0.9.0", "libggml.so.0.29.0", "libunversioned.so", "libsingle.so.1", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(sub, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	linkSonames(dir)

	for _, want := range []string{"libllama.so.0", "libllama-common.so.0", "libggml.so.0"} {
		fi, err := os.Lstat(filepath.Join(sub, want))
		if err != nil {
			t.Fatalf("missing symlink %s: %v", want, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s exists but is not a symlink", want)
		}
		// the link must resolve to the versioned file
		if st, err := os.Stat(filepath.Join(sub, want)); err != nil || st.Size() != 1 {
			t.Fatalf("symlink %s does not resolve: %v", want, err)
		}
	}
	// An already-unversioned lib must NOT be clobbered or linked to itself.
	if fi, err := os.Lstat(filepath.Join(sub, "libunversioned.so")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("libunversioned.so was clobbered: %v %v", fi, err)
	}
	// A single-component version is already its own soname: no link.
	if _, err := os.Lstat(filepath.Join(sub, "libsingle.so")); err == nil {
		t.Fatal("libsingle.so.1 gained a spurious libsingle.so link")
	}
	// Idempotency: a second run must not fail or double-link.
	linkSonames(dir)
	if _, err := os.Lstat(filepath.Join(sub, "libllama.so.0")); err != nil {
		t.Fatal(err)
	}
}
