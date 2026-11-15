//go:build windows && amd64

package sandbox

//go:noescape
func cpuHypervisorPresent() bool
