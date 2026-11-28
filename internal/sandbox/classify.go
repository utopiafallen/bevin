package sandbox

import (
	"strings"
)

// The HCS error classifiers (shared, not windows-tagged, so the string
// matching is unit-testable on every platform — the windows.go backend is
// the only caller). They classify errors from the Host Compute Service
// WITHOUT depending on a specific win32 error text: HCS wraps its native
// errors in a few English phrasings, and the native ERROR_* strings vary
// by locale on the host, so the match is on the message shapes that have
// actually been observed.

// isNotFound reports whether an HCS error means "no such compute system"
// (the caller's reattach/destroy step must treat it as a clean miss, not
// a failure). The phrasings:
//
//   - "cannot find the process" / "The system cannot find the file" — the
//     native win32 ERROR_FILE_NOT_FOUND family (the hcsshim plumbing)
//   - "not found" — the generic hcsshim wrap
//   - "identifier does not exist" — HCS's OWN message for a missing
//     compute system: "A virtual machine or container with the specified
//     identifier does not exist." (observed live: hcs::OpenComputeSystem
//     bevin-<handle>). The match is on "identifier" (not a bare
//     "does not exist" — that would also swallow unrelated phrasings,
//     e.g. a service lookup failure). Missing this one (2026-11-28,
//     user's live instance) made Ensure's reattach lookup report
//     "looking up the container failed" for a container that simply was
//     never created — so the create path was unreachable and the
//     container stayed absent forever.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "cannot find the process") ||
		strings.Contains(s, "The system cannot find the file") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "identifier does not exist")
}

// isAlreadyRunning reports whether a Start failed because the system is
// already up (a no-op success — the caller proceeds).
func isAlreadyRunning(err error) bool {
	if err == nil {
		return true // Start on a running system is a no-op success
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already") || strings.Contains(s, "running")
}

// isAlreadyStopped reports whether a Shutdown/Terminate failed because the
// system was already down (a no-op success).
func isAlreadyStopped(err error) bool {
	if err == nil {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not running") || strings.Contains(s, "stopped") || strings.Contains(s, "terminated")
}
