// Package sandbox provides per-user execution environments on top of the
// OS's NATIVE container APIs.
//
// Windows: each user's environment is a Hyper-V isolated container managed
// through the Host Compute Service (HCS) via hcsshim — Microsoft's Go
// interface to HCS (the same library Moby and containerd use).
//
// Linux: each user's environment is a systemd-nspawn container run as a
// systemd service instance (systemd-nspawn@<name>.service) and controlled
// through the OS's own tools (systemctl, machinectl, systemd-run) — the
// closest native equivalent to HCS.
//
// Both backends map the user's data directory into the container and keep
// persistent per-user storage under <data>/sandboxes/<handle>/, so a user
// can install applications and execute arbitrary code in their own
// environment and it survives container + bevin restarts.
//
// The backends ASSUME the necessary OS services are enabled (the HCS
// service + hypervisor on Windows; systemd as init on Linux). Runtime()
// reports readiness so the first-time setup can prompt the user to enable
// the runtime for their OS before proceeding.
package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// ErrDisabled is returned when sandboxing is disabled in the server config.
var ErrDisabled = errors.New("sandbox: disabled in the server config")

// ErrNotRunning is returned by Exec when the user's container is not up.
var ErrNotRunning = errors.New("sandbox: the user's container is not running")

// State is the lifecycle state of a user's container.
type State string

const (
	StateAbsent  State = "absent"  // no container storage on disk
	StateStopped State = "stopped" // storage exists; the container is not running
	StateRunning State = "running" // the container is up
)

// Runtime is the readiness of the OS container runtime. Detail is
// human-readable (the setup wizard shows it as-is).
type Runtime struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Info describes a user's container.
type Info struct {
	Handle string `json:"handle"`
	State  State  `json:"state"`
}

// Result is the output of an Exec call.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Config is the server-level sandbox configuration (server.toml
// [sandbox]). Zero values mean the platform default.
type Config struct {
	// Enabled is the master switch. When false the sandbox API answers
	// 503 and the setup check is skipped (an explicit admin opt-out for
	// machines that cannot run containers).
	Enabled bool `toml:"enabled" json:"enabled"`
	// Network is the linux container networking mode: "veth" (default —
	// a private per-container /24 with NATed outbound) or "private"
	// (no network at all). Ignored on Windows (Hyper-V isolated
	// containers get the host's NAT by default).
	Network string `toml:"network" json:"network"`
	// Cores is the per-container CPU count (0 = platform default).
	Cores int `toml:"cores" json:"cores"`
	// MemoryMB is the per-container memory cap in MiB (0 = platform
	// default).
	MemoryMB int `toml:"memory_mb" json:"memory_mb"`
}

// Backend is a per-OS container runtime.
type Backend interface {
	// Runtime reports whether the OS container runtime is available
	// (the OS service is enabled and the prerequisites are met) and a
	// human-readable detail.
	Runtime(ctx context.Context) Runtime

	// Ensure creates (when needed) and starts the user's container. It
	// is idempotent: an existing container is reattached and started
	// when stopped; its persistent storage is never clobbered.
	Ensure(ctx context.Context, handle string) (Info, error)

	// Exec runs a command string in the user's container (the
	// container's shell interprets it — arbitrary code is the point).
	// The container must be running.
	Exec(ctx context.Context, handle, command string, timeout time.Duration) (Result, error)

	// Status reports the current state of the user's container.
	Status(ctx context.Context, handle string) (Info, error)

	// Stop stops the user's container (the storage is preserved).
	Stop(ctx context.Context, handle string) (Info, error)

	// Remove stops the user's container and deletes its persistent
	// storage (the user's data directory is untouched).
	Remove(ctx context.Context, handle string) error
}

// Storage layout under the data directory (all container state lives here
// except the OS-level service wiring the linux backend installs):
//
//	<data>/sandboxes/
//	  base/
//	    root/         (linux) the base rootfs template
//	    <layer>/      (windows) the extracted base image layers
//	    base.json     (windows) the base layer list for the HCS config
//	  <handle>/
//	    state.json    per-user bookkeeping
//	    rootfs/       (linux) the container's rootfs (persistent)
//	    rwlayer/      (windows) the container's read-write layer
func SandboxRoot(dataDir string) string {
	return filepath.Join(dataDir, "sandboxes")
}

// BaseDir is where the base image lives (provisioned at setup).
func BaseDir(dataDir string) string {
	return filepath.Join(SandboxRoot(dataDir), "base")
}

// BaseRoot is the linux base rootfs.
func BaseRoot(dataDir string) string {
	return filepath.Join(BaseDir(dataDir), "root")
}

// UserSandboxDir is the per-user container storage.
func UserSandboxDir(dataDir, handle string) string {
	return filepath.Join(SandboxRoot(dataDir), handle)
}

// UserDataDir is the user's data directory (their folder on the server) —
// the directory that gets mapped into the container.
func UserDataDir(dataDir, handle string) string {
	return filepath.Join(dataDir, "users", handle)
}

// MachineName is the OS-level name of the user's container (the systemd
// machine name / HCS container ID). It must be a valid hostname AND, on
// the linux veth networking mode, short enough for nspawn's veth
// interface name ("ve-"+name, 15-char limit): longer than 12 chars it
// falls back to a stable hash of the handle.
func MachineName(handle string) string {
	if name := "bevin-" + handle; len(name) <= 12 {
		return name
	}
	sum := sha256.Sum256([]byte("bevin-" + handle))
	return fmt.Sprintf("b%x", sum[:5])
}
