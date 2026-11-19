//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	hcsshim "github.com/Microsoft/hcsshim"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// The windows backend drives the Host Compute Service (HCS) — the OS's
// native container API — via hcsshim (Microsoft's Go interface, the same
// library Moby and containerd use). Each user's environment is a
// Hyper-V isolated container (a utility VM), created from:
//
//   - the read-only BASE image layers (provisioned at setup: a Windows
//     servercore image extracted under <data>/sandboxes/base/, recorded
//     in base.json)
//   - a per-user READ-WRITE layer (<data>/sandboxes/<handle>/rwlayer) —
//     apps the user installs land here and survive container + bevin
//     restarts
//   - a mapped directory: the user's data dir → C:\data in the container
const (
	// hcsServiceName is the Windows service NAME (not the localized display
	// name, not the hcsshim Go library) of the Hyper-V Host Compute
	// Service — the service that actually creates the isolated containers.
	// Its binary is C:\Windows\System32\vmcompute.exe; it is registered
	// under the name "vmcompute" on every locale (display name "Hyper-V
	// Host Compute Service" is localized, the service name is not).
	hcsServiceName = "vmcompute"
	hcsOwner       = "bevin"   // the Owner field used to find our containers
	dataMount      = `C:\data` // where the user's data dir is mapped in
)

// baseImage is base.json — the extracted base image layers (in order,
// read-only) for the HCS Layers list.
type baseImage struct {
	Layers []baseLayer `json:"layers"`
}

type baseLayer struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type windowsBackend struct {
	dataDir string
	cfg     Config
}

func newWindows(dataDir string, cfg Config) *windowsBackend {
	return &windowsBackend{dataDir: dataDir, cfg: cfg}
}

// Runtime reports whether the windows container runtime is ready: bevin
// elevated (HCS requires admin), the Host Compute Service running, the
// hypervisor active (Hyper-V isolation), and the base image provisioned.
func (b *windowsBackend) Runtime(_ context.Context) Runtime {
	if !isElevated() {
		return Runtime{false, "bevin must run as Administrator: the Host Compute Service (which creates the Hyper-V isolated containers) requires it"}
	}
	if ok, detail := hcsServiceState(); !ok {
		return Runtime{false, "the Windows Host Compute Service (vmcompute) is not " + detail + " — enable it (services.msc → “Hyper-V Host Compute Service” → Start, set to Automatic)"}
	}
	if !cpuHypervisorPresent() {
		return Runtime{false, "no hypervisor is active — enable the “Hyper-V Platform” Windows feature (Control Panel → Programs → Turn Windows features on or off) and restart the computer"}
	}
	if _, err := os.Stat(b.baseJSONPath()); err != nil {
		return Runtime{false, "the base container image is not provisioned — run the setup wizard's sandbox step (it downloads a Windows servercore image)"}
	}
	return Runtime{true, "Host Compute Service running; hypervisor active; base image present"}
}

// hcsServiceState queries the Host Compute Service through the service
// control manager (pure Go, no WMI/PowerShell).
func hcsServiceState() (bool, string) {
	m, err := mgr.Connect()
	if err != nil {
		return false, "reachable (cannot open the service control manager: " + err.Error() + ")"
	}
	defer m.Disconnect()
	s, err := m.OpenService(hcsServiceName)
	if err != nil {
		return false, "installed (cannot open the service: " + err.Error() + ")"
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return false, "queryable (cannot query the service: " + err.Error() + ")"
	}
	if st.State == svc.Running {
		return true, "running"
	}
	return false, serviceNameState(st.State)
}

func serviceNameState(st svc.State) string {
	switch st {
	case svc.Stopped:
		return "stopped — start it"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	default:
		return fmt.Sprintf("in state %d", int(st))
	}
}

// isElevated reports whether the bevin process holds an elevated
// (administrator) token — required for HCS.
func isElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer windows.CloseHandle(windows.Handle(token))
	return token.IsElevated()
}

func (b *windowsBackend) containerID(handle string) string { return "bevin-" + handle }
func (b *windowsBackend) rwLayerDir(handle string) string {
	return filepath.Join(UserSandboxDir(b.dataDir, handle), "rwlayer")
}
func (b *windowsBackend) baseJSONPath() string {
	return filepath.Join(BaseDir(b.dataDir), "base.json")
}

func (b *windowsBackend) loadBase() (*baseImage, error) {
	data, err := os.ReadFile(b.baseJSONPath())
	if err != nil {
		return nil, fmt.Errorf("base image not provisioned: %w", err)
	}
	var bi baseImage
	if err := json.Unmarshal(data, &bi); err != nil {
		return nil, fmt.Errorf("corrupt base image record: %w", err)
	}
	return &bi, nil
}

// diskState is the state when HCS reports no such compute system:
// storage on disk = stopped, otherwise absent.
func (b *windowsBackend) diskState(handle string) State {
	if _, err := os.Stat(b.rwLayerDir(handle)); err == nil {
		return StateStopped
	}
	return StateAbsent
}

// Status reports the current state of the user's container.
func (b *windowsBackend) Status(_ context.Context, handle string) (Info, error) {
	props, err := hcsshim.GetContainers(hcsshim.ComputeSystemQuery{Owners: []string{hcsOwner}})
	if err != nil {
		// HCS itself is unreachable — report the on-disk state rather
		// than failing the status call.
		return Info{handle, b.diskState(handle)}, nil
	}
	for _, p := range props {
		if p.ID == b.containerID(handle) {
			if p.State == "Running" && !p.Stopped {
				return Info{handle, StateRunning}, nil
			}
			return Info{handle, StateStopped}, nil
		}
	}
	return Info{handle, b.diskState(handle)}, nil
}

// Ensure creates (when needed) and starts the user's container. The
// compute system is reattached when it still exists (its state lives in
// the on-host layer folders, so a bevin restart does not lose anything).
func (b *windowsBackend) Ensure(ctx context.Context, handle string) (Info, error) {
	id := b.containerID(handle)

	// Reattach path: the compute system still exists (a bevin restart,
	// or the container was left running/stopped on the host).
	if c, err := hcsshim.OpenContainer(id); err == nil {
		started := false
		if err := c.Start(); err != nil && !isAlreadyRunning(err) {
			c.Close()
			return Info{}, fmt.Errorf("starting the existing container failed: %w", err)
		} else if err == nil {
			started = true
		}
		if started {
			if err := b.startKeepalive(ctx, c); err != nil {
				c.Close()
				return Info{}, err
			}
		}
		c.Close()
		return Info{handle, StateRunning}, nil
	} else if !isNotFound(err) {
		return Info{}, fmt.Errorf("looking up the container failed: %w", err)
	}

	// Create path: build the HCS config (base layers + RW layer + the
	// data dir mapping) and create + start.
	cc, err := b.buildConfig(handle)
	if err != nil {
		return Info{}, err
	}
	c, err := hcsshim.CreateContainer(id, cc)
	if err != nil {
		return Info{}, fmt.Errorf("creating the container failed: %w", err)
	}
	if err := c.Start(); err != nil {
		_ = c.Terminate()
		c.Close()
		return Info{}, fmt.Errorf("starting the new container failed: %w", err)
	}
	if err := b.startKeepalive(ctx, c); err != nil {
		c.Close()
		return Info{}, err
	}
	c.Close()
	return Info{handle, StateRunning}, nil
}

// startKeepalive launches the infinite-keepalive process that keeps the
// Hyper-V isolated container alive (an HV container with no guest
// processes has no reason to stay running).
func (b *windowsBackend) startKeepalive(ctx context.Context, c hcsshim.Container) error {
	pc := &hcsshim.ProcessConfig{
		CommandLine:      `C:\Windows\System32\cmd.exe /d /c ping 127.0.0.1 -t`,
		WorkingDirectory: dataMount,
	}
	p, err := c.CreateProcess(pc)
	if err != nil {
		return fmt.Errorf("starting the container keepalive failed: %w", err)
	}
	_ = p.Close()
	_ = ctx
	return nil
}

func (b *windowsBackend) buildConfig(handle string) (*hcsshim.ContainerConfig, error) {
	base, err := b.loadBase()
	if err != nil {
		return nil, err
	}
	rw := b.rwLayerDir(handle)
	if err := os.MkdirAll(rw, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(UserDataDir(b.dataDir, handle), 0o755); err != nil {
		return nil, err
	}
	layers := make([]hcsshim.Layer, 0, len(base.Layers)+1)
	for _, l := range base.Layers {
		layers = append(layers, hcsshim.Layer{ID: l.ID, Path: l.Path})
	}
	layers = append(layers, hcsshim.Layer{ID: b.containerID(handle), Path: rw})
	cc := &hcsshim.ContainerConfig{
		SystemType:  "Container",
		Name:        b.containerID(handle),
		Owner:       hcsOwner,
		HvPartition: true, // Hyper-V isolation: a utility VM per container
		HostName:    "bevin",
		Layers:      layers,
		MappedDirectories: []hcsshim.MappedDir{
			{HostPath: UserDataDir(b.dataDir, handle), ContainerPath: dataMount},
		},
	}
	if b.cfg.Cores > 0 {
		cc.ProcessorCount = uint32(b.cfg.Cores)
	}
	if b.cfg.MemoryMB > 0 {
		cc.MemoryMaximumInMB = int64(b.cfg.MemoryMB)
	}
	return cc, nil
}

// ContainerDataDir is where the user's data dir is mapped in.
func (b *windowsBackend) ContainerDataDir() string { return dataMount }

// Exec runs a command string in the user's container, with the working
// directory at the mapped user data dir.
func (b *windowsBackend) Exec(ctx context.Context, handle, command string, timeout time.Duration) (Result, error) {
	return b.ExecIn(ctx, handle, command, "", timeout)
}

// ExecIn runs a command string in the user's container with an explicit
// working directory (an empty cwd = the data dir). The command is
// interpreted by cmd.exe (arbitrary code is the point — bevin does not
// sanitize it).
func (b *windowsBackend) ExecIn(ctx context.Context, handle, command, cwd string, timeout time.Duration) (Result, error) {
	if strings.TrimSpace(command) == "" {
		return Result{}, errors.New("empty command")
	}
	if cwd == "" {
		cwd = dataMount
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 5*time.Minute {
		timeout = 5 * time.Minute
	}
	c, err := hcsshim.OpenContainer(b.containerID(handle))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer c.Close()

	// cmd /s /c "<command>" passes <command> verbatim to the shell
	// (with the outer pair of quotes stripped) — no quote mangling.
	pc := &hcsshim.ProcessConfig{
		CommandLine:      `C:\Windows\System32\cmd.exe /d /s /c "` + command + `"`,
		WorkingDirectory: cwd,
		CreateStdOutPipe: true,
		CreateStdErrPipe: true,
	}
	p, err := c.CreateProcess(pc)
	if err != nil {
		return Result{}, fmt.Errorf("launching the process in the container failed: %w", err)
	}
	defer p.Close()

	stdin, stdout, stderr, err := p.Stdio()
	if err != nil {
		return Result{}, fmt.Errorf("attaching to the process failed: %w", err)
	}
	_ = stdin
	var outBuf, errBuf bytes.Buffer
	copiesDone := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&outBuf, stdout); copiesDone <- struct{}{} }()
	go func() { _, _ = io.Copy(&errBuf, stderr); copiesDone <- struct{}{} }()

	type waitResult struct {
		err error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		waitCh <- waitResult{p.Wait()}
	}()

	select {
	case <-ctx.Done():
		_ = p.Kill()
		<-waitCh
		drainCopies(copiesDone, 2*time.Second)
		return Result{}, fmt.Errorf("exec cancelled: %w", ctx.Err())
	case <-time.After(timeout):
		_ = p.Kill()
		<-waitCh
		drainCopies(copiesDone, 2*time.Second)
		return Result{}, fmt.Errorf("exec timed out after %s", timeout)
	case wr := <-waitCh:
		// Wait returns an error for a non-zero exit code — the exit
		// code itself is the RESULT, not an error (unless the wait
		// itself failed, in which case ExitCode says the same).
		drainCopies(copiesDone, 5*time.Second)
		code, err := p.ExitCode()
		if err != nil && wr.err != nil {
			return Result{}, fmt.Errorf("the process could not be waited on: %w", wr.err)
		}
		return Result{Stdout: string(outBuf.Bytes()), Stderr: string(errBuf.Bytes()), ExitCode: code}, nil
	}
}

func drainCopies(done <-chan struct{}, timeout time.Duration) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-t.C:
			return
		}
	}
}

// Stop stops the user's container (the storage is preserved).
func (b *windowsBackend) Stop(_ context.Context, handle string) (Info, error) {
	c, err := hcsshim.OpenContainer(b.containerID(handle))
	if err != nil {
		// No compute system — only disk state to report.
		return Info{handle, b.diskState(handle)}, nil
	}
	if err := c.Shutdown(); err != nil && !isAlreadyStopped(err) {
		c.Close()
		return Info{}, fmt.Errorf("shutting down the container failed: %w", err)
	}
	// Bounded wait; a slow shutdown is not a failure (the storage is
	// durable either way).
	_ = c.WaitTimeout(60 * time.Second)
	c.Close()
	return Info{handle, b.diskState(handle)}, nil
}

// Remove stops the user's container and deletes its persistent storage
// (the user's data directory is untouched).
func (b *windowsBackend) Remove(ctx context.Context, handle string) error {
	c, err := hcsshim.OpenContainer(b.containerID(handle))
	if err == nil {
		if err := c.Terminate(); err != nil && !isAlreadyStopped(err) {
			c.Close()
			return fmt.Errorf("terminating the container failed: %w", err)
		}
		_ = c.WaitTimeout(60 * time.Second)
		c.Close()
	} else if !isNotFound(err) {
		return fmt.Errorf("looking up the container failed: %w", err)
	}
	if err := os.RemoveAll(UserSandboxDir(b.dataDir, handle)); err != nil {
		return fmt.Errorf("removing the container storage failed: %w", err)
	}
	return nil
}

// isNotFound / isAlreadyRunning / isAlreadyStopped classify HCS errors
// without depending on a specific win32 error text.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "cannot find the process") ||
		strings.Contains(s, "The system cannot find the file") ||
		strings.Contains(s, "not found")
}

func isAlreadyRunning(err error) bool {
	if err == nil {
		return true // Start on a running system is a no-op success
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already") || strings.Contains(s, "running")
}

func isAlreadyStopped(err error) bool {
	if err == nil {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not running") || strings.Contains(s, "stopped") || strings.Contains(s, "terminated")
}
