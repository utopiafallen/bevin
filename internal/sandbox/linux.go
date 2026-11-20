//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The linux backend drives the OS's native container stack: systemd
// (init + the service that runs each container) + systemd-nspawn (the
// container runtime) + machinectl/systemd-run (exec + status via D-Bus).
//
// OS-level wiring installed per container (all regenerated on every
// ensure, derived from the data-dir state — never edited by hand):
//
//   - /var/lib/machines/<machine> → <data>/sandboxes/<handle>/rootfs
//     (the standard image search path; the template unit finds the
//     rootfs by machine name)
//   - /etc/systemd/system/systemd-nspawn@<machine>.service.d/10-bevin.conf
//     (a unit drop-in that pins the EXACT nspawn invocation — the distro
//     template's defaults are not relied on; notably it clears the
//     default --private-users, which would map the container's root to a
//     foreign UID and break ownership of the bind-mounted data dir)
const (
	systemdMarker      = "/run/systemd/system" // systemd is PID 1 when this exists
	defaultMachinesDir = "/var/lib/machines"   // standard container image search path
	defaultUnitBaseDir = "/etc/systemd/system" // unit drop-ins
)

// networkdConfig is written into each veth-mode container's rootfs: the
// in-container systemd-networkd config for the container side of the veth
// (nspawn names it "host0").
const networkdConfig = `[Match]
Name=host0

[Network]
Address=10.42.%d.2/24
Gateway=10.42.%d.1
DNS=1.1.1.1
DNS=8.8.8.8
`

// linuxState is the per-user bookkeeping kept in state.json.
type linuxState struct {
	Subnet    int    `json:"subnet"` // veth subnet index: 10.42.<N>.0/24
	CreatedAt string `json:"created_at"`
	// BaseBytes is the container-storage size captured at first creation —
	// the baseline for the disk cap's growth-over-baseline (the base OS
	// image does not count against the user). 0 = not captured (pre-feature;
	// the disk cap then counts the whole storage — a safe over-estimate).
	BaseBytes int64 `json:"base_bytes"`
}

type linuxBackend struct {
	dataDir string
	cfg     Config
	run     commandFunc
	// machinesDir/unitBaseDir are the OS-level wiring locations
	// (overridden in tests — they are absolute system paths by default).
	machinesDir string
	unitBaseDir string
}

func newLinux(dataDir string, cfg Config) *linuxBackend {
	return &linuxBackend{dataDir: dataDir, cfg: cfg, run: osRun, machinesDir: defaultMachinesDir, unitBaseDir: defaultUnitBaseDir}
}

// Runtime reports whether the linux container runtime is ready (root,
// systemd as init, systemd-nspawn available). The base container image
// is a SEPARATE gate (the setup's base provisioning + its Finalize
// backstop — setup/sandbox.go): folding it into Runtime().OK deadlocks
// setup, because the base auto-download only starts once the runtime
// reports ready, and the runtime was never ready until the download had
// finished. Must be cheap (no container is created or started by it).
func (b *linuxBackend) Runtime(_ context.Context) Runtime {
	if os.Geteuid() != 0 {
		return Runtime{false, "bevin must run as root: managing systemd-nspawn containers (and their networking) requires it"}
	}
	if _, err := os.Stat(systemdMarker); err != nil {
		return Runtime{false, "systemd is not running as the init system — the Linux container runtime is systemd-nspawn (systemd must be PID 1)"}
	}
	if _, err := exec.LookPath("systemd-nspawn"); err != nil {
		return Runtime{false, "systemd-nspawn is not installed (Debian/Ubuntu: apt install systemd-container)"}
	}
	return Runtime{true, "systemd + systemd-nspawn ready"}
}

func (b *linuxBackend) machineName(handle string) string { return MachineName(handle) }
func (b *linuxBackend) rootfsDir(handle string) string {
	return filepath.Join(UserSandboxDir(b.dataDir, handle), "rootfs")
}
func (b *linuxBackend) dropInPath(handle string) string {
	return filepath.Join(b.unitBaseDir, "systemd-nspawn@"+b.machineName(handle)+".service.d", "10-bevin.conf")
}
func (b *linuxBackend) machineSymlink(handle string) string {
	return filepath.Join(b.machinesDir, b.machineName(handle))
}
func (b *linuxBackend) statePath(handle string) string {
	return filepath.Join(UserSandboxDir(b.dataDir, handle), "state.json")
}

func (b *linuxBackend) loadState(handle string) linuxState {
	var st linuxState
	if data, err := os.ReadFile(b.statePath(handle)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func (b *linuxBackend) saveState(handle string, st linuxState) error {
	if st.CreatedAt == "" {
		st.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(filepath.Dir(b.statePath(handle)), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(b.statePath(handle), data, 0o644)
}

// nextSubnet allocates a /24 index (1..254) not already used by another
// user's container (stable: the allocation is persisted in state.json).
func (b *linuxBackend) nextSubnet() int {
	used := map[int]bool{}
	entries, _ := os.ReadDir(SandboxRoot(b.dataDir))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var st linuxState
		if data, err := os.ReadFile(filepath.Join(SandboxRoot(b.dataDir), e.Name(), "state.json")); err == nil {
			_ = json.Unmarshal(data, &st)
		}
		if st.Subnet != 0 {
			used[st.Subnet] = true
		}
	}
	for n := 1; n <= 254; n++ {
		if !used[n] {
			return n
		}
	}
	return 0
}

func (b *linuxBackend) networkMode() string {
	if b.cfg.Network == "private" {
		return "private"
	}
	return "veth"
}

// Ensure creates (when needed) and starts the user's container.
func (b *linuxBackend) Ensure(ctx context.Context, handle string) (Info, error) {
	if ok, err := b.running(ctx, handle); err != nil {
		return Info{}, err
	} else if ok {
		return Info{handle, StateRunning}, nil
	}

	m := b.machineName(handle)
	dir := UserSandboxDir(b.dataDir, handle)
	rootfs := b.rootfsDir(handle)
	base := BaseRoot(b.dataDir)

	// 1. The persistent rootfs, from the base template (a CoW snapshot
	//    when the filesystem supports it, a plain copy otherwise).
	if _, err := os.Stat(filepath.Join(rootfs, "etc", "os-release")); err != nil {
		if _, err := os.Stat(filepath.Join(base, "etc", "os-release")); err != nil {
			return Info{}, fmt.Errorf("base container image missing — run the setup wizard's sandbox step (it downloads a minimal OS)")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Info{}, err
		}
		if isBtrfsSubvolume(ctx, b.run, base) {
			_, _, code, _ := b.run(ctx, "", "btrfs", "subvolume", "snapshot", base, rootfs)
			if code != 0 {
				os.RemoveAll(rootfs) // partial snapshot artifact
			}
		}
		if _, err := os.Stat(filepath.Join(rootfs, "etc", "os-release")); err != nil {
			if _, errb, code, err := b.run(ctx, "", "cp", "-a", base, rootfs); err != nil || code != 0 {
				return Info{}, fmt.Errorf("copying the base image into the container storage failed: %v (%s)", err, tail(errb))
			}
		}
		// A fresh container must not keep the template's machine-id
		// (systemd inside wants a machine-id of its own; the convention
		// is a single newline = "generate on first boot").
		_ = os.WriteFile(filepath.Join(rootfs, "etc", "machine-id"), []byte("\n"), 0o644)
		// Capture the container-storage baseline (the disk cap counts GROWTH
		// over this — the base OS image does not count against the user).
		// Best-effort: a failure leaves the baseline 0 (the disk cap then
		// counts the whole storage — a safe over-estimate).
		b.captureBaseline(handle)
	}

	// 2. The user's data directory (mapped in below) must exist.
	if err := os.MkdirAll(UserDataDir(b.dataDir, handle), 0o755); err != nil {
		return Info{}, err
	}

	// 3. Networking: a stable per-container /24 (veth mode) + the
	//    in-container systemd-networkd config.
	mode := b.networkMode()
	st := b.loadState(handle)
	if mode == "veth" && st.Subnet == 0 {
		n := b.nextSubnet()
		if n == 0 {
			return Info{}, errors.New("no free container subnet (10.42.1.0/24 .. 10.42.254.0/24 exhausted)")
		}
		st.Subnet = n
		netDir := filepath.Join(rootfs, "etc", "systemd", "network")
		if err := os.MkdirAll(netDir, 0o755); err != nil {
			return Info{}, err
		}
		if err := os.WriteFile(filepath.Join(netDir, "25-bevin.network"), []byte(fmt.Sprintf(networkdConfig, n, n)), 0o644); err != nil {
			return Info{}, err
		}
	}
	if st.Subnet != 0 || st.CreatedAt != "" {
		if err := b.saveState(handle, st); err != nil {
			return Info{}, err
		}
	}

	// 4. OS-level service wiring (regenerated every ensure): the image
	//    symlink + the unit drop-in pinning the exact invocation, then a
	//    daemon-reload so systemd sees the drop-in.
	if err := os.MkdirAll(b.machinesDir, 0o755); err != nil {
		return Info{}, err
	}
	if err := b.ensureSymlink(rootfs, b.machineSymlink(handle)); err != nil {
		return Info{}, err
	}
	dropIn := dropInContent(b.dataDir, handle, mode, b.cfg.MemoryMB, b.cfg.Cores)
	if err := os.MkdirAll(filepath.Dir(b.dropInPath(handle)), 0o755); err != nil {
		return Info{}, err
	}
	if err := os.WriteFile(b.dropInPath(handle), []byte(dropIn), 0o644); err != nil {
		return Info{}, err
	}
	if _, errb, code, err := b.run(ctx, "", "systemctl", "daemon-reload"); err != nil || code != 0 {
		return Info{}, fmt.Errorf("systemctl daemon-reload failed: %v (%s)", err, tail(errb))
	}

	// 5. Start the container as a systemd service instance and wait for
	//    it to come up.
	if out, errb, code, err := b.run(ctx, "", "systemctl", "start", "systemd-nspawn@"+m); err != nil || code != 0 {
		return Info{}, fmt.Errorf("starting the container service failed: %v (%s)", err, tail(append(append([]byte{}, out...), errb...)))
	}
	if err := b.waitActive(ctx, handle, 90*time.Second); err != nil {
		out, _ := b.runOut(ctx, "journalctl", "-u", "systemd-nspawn@"+m, "-n", "15", "--no-pager")
		return Info{}, fmt.Errorf("the container did not come up: %v — %s", err, tail([]byte(out)))
	}

	// 6. Host side of the veth (it exists only while the container runs):
	//    static address + NATed outbound (idempotent).
	if mode == "veth" {
		if err := b.setupHostNAT(ctx, m, st.Subnet); err != nil {
			return Info{}, fmt.Errorf("container is up but host networking setup failed: %w", err)
		}
	}
	return Info{handle, StateRunning}, nil
}

func (b *linuxBackend) ensureSymlink(target, link string) error {
	if err := os.Symlink(target, link); err == nil {
		return nil
	}
	// Replace a stale entry (the data dir moved, or a previous removal
	// left something behind).
	if err := os.Remove(link); err != nil {
		return fmt.Errorf("replacing the container image link failed: %w", err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("creating the container image link failed: %w", err)
	}
	return nil
}

// dropInContent is the per-instance unit drop-in. The empty ExecStart=
// line CLEARS the distro template's command line (whose defaults — e.g.
// --private-users — are not relied on); the following line pins the exact
// invocation. --private-users=no keeps the container's root as the host's
// root so the bind-mounted data dir keeps its real ownership. The [Service]
// resource directives (MemoryHigh / CPUQuota) bound the service's cgroup,
// of which the nspawn container's cgroup is a descendant, so they bound the
// container (cgroup v2 limits are hierarchical).
func dropInContent(dataDir, handle, mode string, memMB, cores int) string {
	var netFlag string
	if mode == "private" {
		netFlag = "--private-network"
	} else {
		netFlag = "--network-veth"
	}
	var limits string
	// RAM = a SOFT limit (the container must not be killed on breach):
	// MemoryHigh throttles + reclaims above the threshold with NO OOM-kill.
	// MemoryMax is deliberately NOT set — a hard cap would OOM-kill the
	// container on breach. (memMB 0 = platform default = no directive.)
	if memMB > 0 {
		limits += "MemoryHigh=" + fmt.Sprintf("%dM", memMB) + "\n"
	}
	// CPU = at-most-N-cores-of-time (a quota). Windows' equivalent Cores
	// knob is a fixed vCPU count — close, not identical. (cores 0 = platform
	// default = no directive.)
	if cores > 0 {
		limits += "CPUQuota=" + fmt.Sprintf("%d%%", cores*100) + "\n"
	}
	return "[Service]\n" +
		"ExecStart=\n" +
		"ExecStart=/usr/bin/systemd-nspawn %i --settings=trusted --boot --private-users=no --bind=" +
		quoteArg(UserDataDir(dataDir, handle)) + ":/data " + netFlag + "\n" +
		limits
}

// quoteArg quotes a systemd unit-file command-line argument when it
// contains whitespace (the unit parser is whitespace-splitting).
func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// setupHostNAT gives the host side of the veth (nspawn names it
// "ve-"+machine) its static address and NATs the container's outbound
// traffic (idempotent — safe to run on every ensure/restart).
func (b *linuxBackend) setupHostNAT(ctx context.Context, m string, subnet int) error {
	iface := "ve-" + m
	ip := fmt.Sprintf("10.42.%d.1/24", subnet)
	if _, errb, code, err := b.run(ctx, "", "ip", "addr", "replace", ip, "dev", iface); err != nil || code != 0 {
		return fmt.Errorf("ip addr replace %s dev %s: %v (%s)", ip, iface, err, tail(errb))
	}
	if _, errb, code, err := b.run(ctx, "", "ip", "link", "set", "up", iface); err != nil || code != 0 {
		return fmt.Errorf("ip link set up %s: %v (%s)", iface, err, tail(errb))
	}
	// The NAT rule is global (one for the whole 10.42/16 range) — check
	// before adding.
	if _, _, code, err := b.run(ctx, "", "iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", "10.42.0.0/16", "!", "-o", "10.42.0.0/16", "-j", "MASQUERADE"); err == nil && code == 0 {
		return nil
	}
	if _, errb, code, err := b.run(ctx, "", "iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", "10.42.0.0/16", "!", "-o", "10.42.0.0/16", "-j", "MASQUERADE"); err != nil || code != 0 {
		return fmt.Errorf("adding the NAT rule failed: %v (%s)", err, tail(errb))
	}
	return nil
}

// running reports whether the container's service instance is active.
func (b *linuxBackend) running(ctx context.Context, handle string) (bool, error) {
	_, _, code, err := b.run(ctx, "", "systemctl", "is-active", "systemd-nspawn@"+b.machineName(handle))
	if err != nil {
		return false, err
	}
	return code == 0, nil // is-active exits 0 iff "active"
}

func (b *linuxBackend) waitActive(ctx context.Context, handle string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if ok, err := b.running(ctx, handle); err != nil {
			return err
		} else if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the container to start")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (b *linuxBackend) runOut(ctx context.Context, name string, argv ...string) (string, error) {
	out, errb, code, err := b.run(ctx, "", name, argv...)
	if err != nil {
		return string(out), err
	}
	if code != 0 {
		return string(out), fmt.Errorf("%s exited %d: %s", name, code, strings.TrimSpace(string(errb)))
	}
	return string(out), nil
}

// Status reports the current state of the user's container.
func (b *linuxBackend) Status(ctx context.Context, handle string) (Info, error) {
	if ok, err := b.running(ctx, handle); err != nil {
		return Info{}, err
	} else if ok {
		return Info{handle, StateRunning}, nil
	}
	return Info{handle, b.currentState(handle)}, nil
}

func (b *linuxBackend) currentState(handle string) State {
	if _, err := os.Stat(filepath.Join(b.rootfsDir(handle), "etc", "os-release")); err == nil {
		return StateStopped
	}
	return StateAbsent
}

// Stats reports the container's current usage (CPU-seconds + resident
// memory) from the service's cgroup (systemd tracks both for the
// systemd-nspawn@ instance). Only a RUNNING container has live usage —
// a stopped/absent one reports zero (the dashboard shows it as not running).
func (b *linuxBackend) Stats(ctx context.Context, handle string) (Stats, error) {
	ok, err := b.running(ctx, handle)
	if err != nil {
		return Stats{}, err
	}
	if !ok {
		return Stats{}, nil // stopped or absent: no live usage
	}
	unit := "systemd-nspawn@" + b.machineName(handle)
	out, err := b.runOut(ctx, "systemctl", "show", unit, "-p", "CPUUsageNSec", "-p", "MemoryCurrent")
	if err != nil {
		return Stats{Running: true}, nil // running, but the usage is unavailable
	}
	st := Stats{Running: true}
	for _, line := range strings.Split(out, "\n") {
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch k {
		case "CPUUsageNSec":
			if ns, perr := strconv.ParseUint(v, 10, 64); perr == nil {
				st.CPUSec = float64(ns) / 1e9
			}
		case "MemoryCurrent":
			if mb, perr := strconv.ParseUint(v, 10, 64); perr == nil {
				st.MemoryBytes = mb
			}
		}
	}
	return st, nil
}

// ContainerDataDir is where the user's data dir is mapped in (the
// nspawn Bind= target).
func (b *linuxBackend) ContainerDataDir() string { return "/data" }

// Exec runs a command string in the user's container, with the working
// directory at the mapped user data dir.
func (b *linuxBackend) Exec(ctx context.Context, handle, command string, timeout time.Duration) (Result, error) {
	return b.ExecIn(ctx, handle, command, "", timeout)
}

// ExecIn runs a command string in the user's container with an explicit
// working directory (an empty cwd = the data dir). The command is
// interpreted by the container's /bin/sh (arbitrary code is the point —
// bevin does not sanitize it).
//
// Mechanism: `systemd-run -M <machine> --wait` runs the command as a
// transient unit on the machine's systemd (via D-Bus — the OS's native
// exec path) and propagates the exit status. The wrapper subshell
// captures the output into /run/ files, which are fetched with
// `machinectl copy-from`.
func (b *linuxBackend) ExecIn(ctx context.Context, handle, command, cwd string, timeout time.Duration) (Result, error) {
	if strings.TrimSpace(command) == "" {
		return Result{}, errors.New("empty command")
	}
	if cwd != "" && !filepath.IsAbs(cwd) {
		return Result{}, errors.New("working directory must be an absolute container path")
	}
	if ok, err := b.running(ctx, handle); err != nil {
		return Result{}, err
	} else if !ok {
		return Result{}, ErrNotRunning
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 5*time.Minute {
		timeout = 5 * time.Minute
	}
	m := b.machineName(handle)
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	suffix, err := randomHex(8)
	if err != nil {
		return Result{}, err
	}
	unit := "bevin-exec-" + suffix + ".service"
	outPath := "/run/bevin-exec-" + suffix + ".out"
	errPath := "/run/bevin-exec-" + suffix + ".err"
	wrapper := fmt.Sprintf("( %s ) > %s 2> %s", command, outPath, errPath)

	args := []string{"-M", m, "--wait", "--unit", unit}
	if cwd != "" {
		args = append(args, "--working-directory", cwd)
	}
	args = append(args, "/bin/sh", "-c", wrapper)
	_, errb, code, err := b.run(execCtx, "", "systemd-run", args...)
	if err != nil {
		b.cleanupExec(ctx, m, unit, outPath, errPath)
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return Result{}, fmt.Errorf("exec timed out after %s", timeout)
		}
		return Result{}, fmt.Errorf("exec failed: %v (%s)", err, tail(errb))
	}
	res := b.collectOutput(ctx, m, outPath, errPath)
	res.ExitCode = code
	b.cleanupExec(ctx, m, unit, outPath, errPath)
	return res, nil
}

// collectOutput fetches the captured output files from the container.
func (b *linuxBackend) collectOutput(ctx context.Context, m, outPath, errPath string) Result {
	var res Result
	fetch := func(containerPath, tmp string) string {
		if _, _, code, err := b.run(ctx, "", "machinectl", "copy-from", m, containerPath, tmp); err == nil && code == 0 {
			if data, rerr := os.ReadFile(tmp); rerr == nil {
				return string(data)
			}
		}
		return ""
	}
	tmpDir := filepath.Join(os.TempDir(), "bevin-sandbox")
	tmpOut := filepath.Join(tmpDir, strings.TrimPrefix(outPath, "/"))
	tmpErr := filepath.Join(tmpDir, strings.TrimPrefix(errPath, "/"))
	if err := os.MkdirAll(filepath.Dir(tmpOut), 0o700); err != nil {
		return res
	}
	res.Stdout = fetch(outPath, tmpOut)
	res.Stderr = fetch(errPath, tmpErr)
	os.Remove(tmpOut)
	os.Remove(tmpErr)
	return res
}

// cleanupExec removes the captured files + stops the transient unit if it
// is still running (the timeout path). Best-effort: the machine's own
// cleanup catches the rest.
func (b *linuxBackend) cleanupExec(ctx context.Context, m, unit, outPath, errPath string) {
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, _, _, _ = b.run(ctx2, "", "systemd-run", "-M", m, "sh", "-c",
		fmt.Sprintf("rm -f %s %s; systemctl stop %s 2>/dev/null", outPath, errPath, unit))
}

// Stop stops the user's container (the storage is preserved).
func (b *linuxBackend) Stop(ctx context.Context, handle string) (Info, error) {
	if _, errb, code, err := b.run(ctx, "", "systemctl", "stop", "systemd-nspawn@"+b.machineName(handle)); err != nil || code != 0 {
		return Info{handle, b.currentState(handle)}, fmt.Errorf("stopping the container failed: %v (%s)", err, tail(errb))
	}
	return Info{handle, b.currentState(handle)}, nil
}

// Restart stops + starts the container so it picks up the backend's current
// config limits (the CPU/RAM caps live in the unit drop-in, which Ensure
// rewrites before the start). The persistent rootfs + the user data dir are
// untouched; in-flight processes are killed (an accepted consequence of a
// limit change).
func (b *linuxBackend) Restart(ctx context.Context, handle string) (Info, error) {
	if _, err := b.Stop(ctx, handle); err != nil {
		return Info{}, err
	}
	return b.Ensure(ctx, handle)
}

// Remove stops the user's container and deletes its persistent storage
// (the user's data directory is untouched).
func (b *linuxBackend) Remove(ctx context.Context, handle string) error {
	m := b.machineName(handle)
	if ok, err := b.running(ctx, handle); err != nil {
		return err
	} else if ok {
		if _, errb, code, err := b.run(ctx, "", "systemctl", "stop", "systemd-nspawn@"+m); err != nil || code != 0 {
			return fmt.Errorf("stopping the container before removal failed: %v (%s)", err, tail(errb))
		}
	}
	if err := os.RemoveAll(UserSandboxDir(b.dataDir, handle)); err != nil {
		return fmt.Errorf("removing the container storage failed: %w", err)
	}
	// Best-effort OS wiring removal (stale files here only affect the
	// next ensure, which regenerates them).
	os.Remove(b.dropInPath(handle))
	os.Remove(b.machineSymlink(handle))
	return nil
}

// isBtrfsSubvolume reports whether path is a btrfs subvolume (a CoW
// snapshot is then possible and instant).
func isBtrfsSubvolume(ctx context.Context, run commandFunc, path string) bool {
	_, _, code, err := run(ctx, "", "btrfs", "subvolume", "is-subvolume", path)
	return err == nil && code == 0
}

// captureBaseline records the container-storage size (the per-user sandbox
// dir: the rootfs + the small state.json) in the per-user state, once, at
// first creation — the baseline for the disk cap's growth-over-baseline.
// Idempotent (a non-zero baseline is left untouched). A 0 size leaves the
// baseline 0 (the disk cap then counts the whole storage — a safe
// over-estimate, never an under-count that lets a user over the cap).
func (b *linuxBackend) captureBaseline(handle string) {
	st := b.loadState(handle)
	if st.BaseBytes != 0 {
		return
	}
	if n := DirSize(UserSandboxDir(b.dataDir, handle)); n > 0 {
		st.BaseBytes = n
		_ = b.saveState(handle, st)
	}
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// tail returns the last ≤512 bytes of b as a printable string.
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 512 {
		s = s[len(s)-512:]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}
