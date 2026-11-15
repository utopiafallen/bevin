//go:build windows

package sandbox

func newOS(dataDir string, cfg Config) Backend { return newWindows(dataDir, cfg) }
