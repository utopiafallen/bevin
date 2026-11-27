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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrDisabled is returned by New on platforms with no container backend
// (darwin, ...) — the sandbox API then answers 503.
var ErrDisabled = errors.New("sandbox: not supported on this platform")

// ErrNotRunning is returned by Exec when the user's container is not up.
var ErrNotRunning = errors.New("sandbox: the user's container is not running")

// IsExecTimeout reports whether err is an Exec/ExecIn timeout (the command
// was killed for exceeding its deadline — as opposed to an infrastructure
// failure). Both backends report the timeout with this wording; the agent's
// exec tool uses this as its single classifier for the timeout diagnostic.
func IsExecTimeout(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "exec timed out after")
}

// ErrFileNotFound is returned by ReadFile when the container has no such
// file (wrapped, with the path).
var ErrFileNotFound = errors.New("sandbox: no such file in the container")

// ErrIsDir is returned by ReadFile/WriteFileAtomic when the path is a
// directory, not a file (wrapped, with the path).
var ErrIsDir = errors.New("sandbox: the path is a directory, not a file")

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

// Stats is the current resource usage of a container, as best the platform
// can report it. CPUSec is cumulative CPU-seconds since the container
// started; MemoryBytes is the current resident memory. Disk is deliberately
// NOT here — the per-user disk footprint is the server's concern (the disk
// monitor, which counts growth over the container-storage baseline), not the
// container runtime's; the admin dashboard joins the two.
type Stats struct {
	Running     bool
	CPUSec      float64
	MemoryBytes uint64
}

// Config is the server-level sandbox configuration (server.toml
// [sandbox]). Zero values mean the platform default.
type Config struct {
	// Enabled is always true for a loaded server config (per-user
	// containers are not optional — the config layer canonicalizes it;
	// see config.Sandbox). The backend is nil on platforms with no
	// container API, where the API answers 503.
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
	// DiskMB is the per-user disk usage cap in MiB (0 = no cap). It is
	// enforced by the server's DiskMonitor + diskGate (host-side), not by
	// the backends themselves — see the design log's disk cap section.
	DiskMB int `toml:"disk_mb" json:"disk_mb"`
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
	// The container must be running. The working directory is the
	// mapped user data dir (ExecIn with an empty cwd).
	Exec(ctx context.Context, handle, command string, timeout time.Duration) (Result, error)

	// ExecIn is Exec with an explicit working directory INSIDE the
	// container (e.g. a workspace folder under the mapped data dir —
	// see ContainerDataDir). An empty cwd = the data dir.
	ExecIn(ctx context.Context, handle, command, cwd string, timeout time.Duration) (Result, error)

	// ReadFile reads a file from INSIDE the user's container. path is a
	// container-absolute path (the platform-native form: /etc/hosts on
	// linux, C:\Windows\... on windows). The container must be running
	// (ErrNotRunning); a missing path is ErrFileNotFound (wrapped), a
	// directory ErrIsDir (wrapped).
	ReadFile(ctx context.Context, handle, path string) ([]byte, error)

	// WriteFileAtomic atomically writes content to a container-absolute
	// path: it stages the new content next to the destination and renames
	// it into place, so a concurrent reader never observes a half-written
	// file. The parent directory is created when missing. The container
	// must be running (ErrNotRunning).
	WriteFileAtomic(ctx context.Context, handle, path string, content []byte) error

	// ContainerDataDir is where the user's data directory is mapped in
	// inside the container ("/data" on linux, C:\data on windows).
	// The server uses it to compute container-side paths for exec
	// working directories (e.g. a workspace's folder).
	ContainerDataDir() string

	// Status reports the current state of the user's container.
	Status(ctx context.Context, handle string) (Info, error)

	// Stats reports the container's current resource usage (CPU-seconds +
	// resident memory), as best the platform can gather. A container that
	// is absent (or that the runtime cannot query) reports Running=false +
	// zero usage rather than an error, so the admin dashboard can list
	// every user uniformly.
	Stats(ctx context.Context, handle string) (Stats, error)

	// Stop stops the user's container (the storage is preserved).
	Stop(ctx context.Context, handle string) (Info, error)

	// Restart stops + starts the user's container so it picks up the
	// backend's CURRENT config limits (the per-container CPU/RAM caps —
	// those are applied at (re)start, never to a running container).
	// In-flight processes are killed (an accepted consequence of applying
	// limit changes); the persistent container storage + the user data dir
	// are preserved.
	Restart(ctx context.Context, handle string) (Info, error)

	// Remove stops the user's container and deletes its persistent
	// storage (the user's data directory is untouched).
	Remove(ctx context.Context, handle string) error

	// --- The interactive (PTY) layer (the design log's WS2 section) ---

	// PtyStart starts an interactive session: the command runs in the
	// user's environment on a pseudo-terminal (windows: an HCS console).
	// The environment must be running (ErrNotRunning). It returns the
	// session id (the handle's sessions are addressable by it only).
	PtyStart(ctx context.Context, handle, command, cwd string, cols, rows int) (string, error)

	// PtyWait blocks up to timeout for new output / a state change / the
	// command exiting, and returns what happened: the output produced
	// since the previous wait (the 64 KB ring budget — see PtyWaitResult),
	// the current wait-chain state (running | waiting_input | waiting |
	// exited), and the exit code when exited. A timeout is NOT an error
	// (the model simply waits again).
	PtyWait(ctx context.Context, handle, sessionID string, timeout time.Duration) (PtyWaitResult, error)

	// PtyInput writes text to the session's stdin (the model's answer to
	// a prompt). A session that has already exited is an error (there is
	// no process to send it to).
	PtyInput(ctx context.Context, handle, sessionID, text string) error

	// PtyClose ends the session (killing the command's process group) and
	// returns its exit code (best effort: -1 when unknown). Idempotent.
	PtyClose(ctx context.Context, handle, sessionID string) (int, error)

	// PtyCloseAll ends every live session of the handle (the environment
	// Stop/Remove paths + the server shutdown call it — a stopped
	// environment takes its sessions with it; the host-side session files
	// and registry entries are cleaned up here).
	PtyCloseAll(ctx context.Context, handle string) error
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
//	<data>/users/<handle>/.bevin-pty/   (linux) the pty session state files
//	  (inside the MAPPED data dir — the container sees it at /data/.bevin-pty;
//	  the helper writes, the host reads directly)
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

// DirSize returns the total size in bytes of all regular files under dir
// (recursive). Unreadable entries are skipped (a permission hiccup must not
// fail the walk — it would poison a disk-cap baseline or a monitor pass).
// Shared (all platforms): the sandbox backends use it to capture the
// container-storage baseline, and the server's DiskMonitor uses it for the
// per-user disk-usage check.
func DirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entry: skip
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// ContainerBaseBytes returns the per-user container-storage baseline (bytes)
// captured at creation, 0 if unknown. It reads the per-user state file
// (written by the backend's Ensure, which records base_bytes on first
// creation). The DiskMonitor uses it for the growth-over-baseline disk cap.
// Platform-agnostic: both backends record base_bytes in state.json.
func ContainerBaseBytes(dataDir, handle string) int64 {
	data, err := os.ReadFile(filepath.Join(UserSandboxDir(dataDir, handle), "state.json"))
	if err != nil {
		return 0
	}
	var st struct {
		BaseBytes int64 `json:"base_bytes"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return 0
	}
	return st.BaseBytes
}

// randomHex returns n random bytes as 2n hex chars (a unique per-call
// suffix for capture/stage files).
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
