//go:build linux

package sandbox

func newOS(dataDir string, cfg Config) Backend { return newLinux(dataDir, cfg) }
