package agent

import (
	"runtime"
	"testing"
)

// TestContainerPath pins the container-absolute path gates: a relative
// path is rejected everywhere; on Windows the path is drive-absolute and a
// colon after the drive letter is rejected (NTFS alternate data stream —
// content invisible to listings, retrievable by name); on other OSes a
// colon is a legal filename character.
func TestContainerPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := containerPath(`C:\data\file.txt:evil`); err == nil {
			t.Fatal("a colon after the drive letter must be rejected on Windows (NTFS ADS)")
		}
		if _, err := containerPath(`relative\path`); err == nil {
			t.Fatal("a relative path must be rejected")
		}
	} else {
		if _, err := containerPath("/data/notes: draft.txt"); err != nil {
			t.Fatalf("a colon must be legal on non-Windows: %v", err)
		}
		if _, err := containerPath("relative/path"); err == nil {
			t.Fatal("a relative path must be rejected")
		}
	}
	if _, err := containerPath(""); err == nil {
		t.Fatal("an empty path must be rejected")
	}
	if _, err := containerPath("/a\x00b"); err == nil {
		t.Fatal("a NUL byte must be rejected")
	}
}
