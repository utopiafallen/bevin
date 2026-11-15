//go:build !windows && !linux

package sandbox

func newOS(_ string, _ Config) Backend {
	return &unsupportedBackend{
		reason: "sandboxing is not supported on this platform (bevin containers target linux and windows only)",
	}
}
