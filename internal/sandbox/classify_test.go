package sandbox

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsNotFoundHCS — the live-instance regression: the user's container
// was stuck "absent" forever because HCS reports a missing compute system
// as "A virtual machine or container with the specified identifier does
// not exist." (wrapped: "hcs::OpenComputeSystem bevin-xin: ...") — a
// phrasing the old isNotFound didn't recognize. Ensure's reattach lookup
// therefore reported "looking up the container failed" instead of falling
// through to the create path, so the container could never be created.
func TestIsNotFoundHCS(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			"live HCS miss (the reported failure)",
			errors.New("hcs::OpenComputeSystem bevin-xin: A virtual machine or container with the specified identifier does not exist."),
			true,
		},
		{"HCS miss, bare", errors.New("A virtual machine or container with the specified identifier does not exist."), true},
		{"hcsshim generic not found", errors.New("hcsshim: container not found"), true},
		{"win32 file not found", errors.New("The system cannot find the file specified."), true},
		{"win32 process not found", errors.New("cannot find the process"), true},
		{"access denied is NOT a miss", errors.New("hcs::CreateComputeSystem bevin-xin: access is denied"), false},
		{"vmcompute unreachable is NOT a miss", fmt.Errorf("open service vmcompute: %w", errors.New("the service does not exist")), false},
		{"nil is not a miss", nil, false},
	}
	for _, tt := range tests {
		if got := isNotFound(tt.err); got != tt.want {
			t.Fatalf("%s: isNotFound(%v) = %v, want %v", tt.name, tt.err, got, tt.want)
		}
	}
}

// TestIsAlreadyRunningStopped — the no-op-success classifiers (Start on a
// running system / Shutdown on a stopped one are success, not failure).
func TestIsAlreadyRunningStopped(t *testing.T) {
	if !isAlreadyRunning(nil) {
		t.Fatal("isAlreadyRunning(nil) = false, want true (Start on a running system is a no-op success)")
	}
	if isAlreadyRunning(errors.New("access is denied")) {
		t.Fatal("isAlreadyRunning(access denied) = true, want false (a real failure must surface)")
	}
	if !isAlreadyRunning(errors.New("the compute system is already running")) {
		t.Fatal("isAlreadyRunning(already running) = false, want true (no-op success)")
	}
	if !isAlreadyStopped(nil) {
		t.Fatal("isAlreadyStopped(nil) = false, want true")
	}
	if isAlreadyStopped(errors.New("shutdown timed out")) {
		t.Fatal("isAlreadyStopped(timeout) = true, want false")
	}
}
