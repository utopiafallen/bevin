//go:build windows

package sandbox

import "testing"

// TestHCSServiceName — regression guard for the HCS detection false
// negative: the Hyper-V Host Compute Service is registered under the
// service NAME "vmcompute" (binary C:\Windows\System32\vmcompute.exe).
// The earlier guess "hcsshim" is the Go library's name, not a Windows
// service — querying it always failed with "service does not exist" even
// when the service was running, so the wizard reported the (running)
// runtime as not ready.
func TestHCSServiceName(t *testing.T) {
	if hcsServiceName != "vmcompute" {
		t.Fatalf("hcsServiceName = %q, want \"vmcompute\" (the registered service name of the Hyper-V Host Compute Service; \"hcsshim\" is the Go library, not a service)", hcsServiceName)
	}
}
