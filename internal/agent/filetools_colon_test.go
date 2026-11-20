package agent

import (
	"runtime"
	"testing"
)

// TestFileSafeRelPathColon pins the NTFS ADS gate (security 2026-11-20):
// on Windows a colon in a path targets an alternate data stream (content
// invisible to listings, retrievable by name) — rejected. On other OSes a
// colon is a legal filename character — allowed. The existing gates (..,
// absolute, NUL) hold either way.
func TestFileSafeRelPathColon(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := fileSafeRelPath("file.txt:evil"); err == nil {
			t.Fatal("a colon must be rejected on Windows (NTFS ADS)")
		}
		if _, err := fileSafeRelPath("C:\\Users\\x"); err == nil {
			t.Fatal("an absolute Windows path must be rejected (its drive colon is an ADS)")
		}
	} else {
		if rel, err := fileSafeRelPath("notes: draft.txt"); err != nil {
			t.Fatalf("a colon must be legal on non-Windows: %v", err)
		} else if rel == "" {
			t.Fatal("empty rel")
		}
	}
	if _, err := fileSafeRelPath("../x"); err == nil {
		t.Fatal(".. must be rejected")
	}
	if _, err := fileSafeRelPath("/abs"); err == nil {
		t.Fatal("an absolute path must be rejected")
	}
}
