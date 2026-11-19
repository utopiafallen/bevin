//go:build windows

package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsExecutableWindows pins the Windows semantics: there is no execute
// bit (Go reports 0666 for every regular file on native Windows), so
// existence is the test. A regression to the unix-style Mode()&0o111
// check made the standard-path detection find NOTHING on a Windows host
// with Edge installed — every probe went through this one predicate
// (2026-11-19, the live box reported "no supported desktop browser" while
// msedge.exe sat at the standard path).
func TestIsExecutableWindows(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !isExecutable(exe) {
		t.Fatalf("isExecutable(%q) = false — an existing regular file must count (Windows has no execute bit)", exe)
	}
	if isExecutable(filepath.Join(`C:\`, `no-such-browser-xyz-12345.exe`)) {
		t.Fatal("isExecutable on a missing path = true")
	}
}
