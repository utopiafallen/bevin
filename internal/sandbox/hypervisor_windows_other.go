//go:build windows && !amd64

package sandbox

// cpuHypervisorPresent on non-amd64 windows: there is no portable
// CPUID equivalent, so the check reports "not verifiable" as present.
// bevin's windows target is amd64; if the hypervisor is truly absent
// there, HCS will surface a hard (and legible) error at container
// creation time.
func cpuHypervisorPresent() bool { return true }
