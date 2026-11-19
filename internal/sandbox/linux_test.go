//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRun simulates the OS tools the linux backend drives (systemctl,
// machinectl, systemd-run, ip, iptables, cp, btrfs) — no real systemd
// needed. started tracks which service instances are active (is-active
// flips per unit); calls records every invocation for assertions.
type fakeRun struct {
	mu      sync.Mutex
	started map[string]bool
	calls   [][]string
	// copyFrom maps a container path to the content the fake "machine"
	// holds for it (the exec output files).
	copyFrom map[string]string
	// dataDir is what the fake needs to simulate `cp -a base rootfs`.
	dataDir string
}

func (f *fakeRun) call(ctx context.Context, dir, name string, argv ...string) ([]byte, []byte, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, argv...))
	f.mu.Unlock()
	switch name {
	case "btrfs":
		return nil, []byte("not a btrfs filesystem"), 1, nil
	case "cp":
		// Simulate `cp -a <src> <dst>` (the base rootfs copy).
		src, dst := argv[1], argv[2]
		if err := copyDir(src, dst); err != nil {
			return nil, []byte(err.Error()), 1, nil
		}
		return nil, nil, 0, nil
	case "systemctl":
		if len(argv) >= 1 && argv[0] == "daemon-reload" {
			return nil, nil, 0, nil
		}
		if len(argv) >= 2 && argv[0] == "start" {
			f.mu.Lock()
			f.started[argv[1]] = true
			f.mu.Unlock()
			return nil, nil, 0, nil
		}
		if len(argv) >= 2 && argv[0] == "stop" {
			f.mu.Lock()
			f.started[argv[1]] = false
			f.mu.Unlock()
			return nil, nil, 0, nil
		}
		if len(argv) >= 2 && argv[0] == "is-active" {
			f.mu.Lock()
			on := f.started[argv[1]]
			f.mu.Unlock()
			if on {
				return []byte("active\n"), nil, 0, nil
			}
			return []byte("inactive\n"), nil, 3, nil
		}
	case "ip", "iptables":
		return nil, nil, 0, nil
	case "systemd-run":
		// `systemd-run -M <m> --wait --unit <u> /bin/sh -c <wrapper>` —
		// the fake "machine" runs it: exit 0, and it writes the output
		// files the wrapper names (so copy-from can fetch them).
		for i, a := range argv {
			if a == "-c" && i+1 < len(argv) {
				f.writeExecFiles(argv[i+1])
			}
		}
		return nil, nil, 0, nil
	case "machinectl":
		// `machinectl copy-from <m> <containerPath> <hostPath>`
		if len(argv) >= 4 && argv[0] == "copy-from" {
			f.mu.Lock()
			content := f.copyFrom[argv[2]]
			f.mu.Unlock()
			if content == "" {
				return nil, []byte("no such file"), 1, nil
			}
			if err := os.WriteFile(argv[3], []byte(content), 0o644); err != nil {
				return nil, []byte(err.Error()), 1, nil
			}
			return nil, nil, 0, nil
		}
	case "journalctl":
		return []byte("fake journal\n"), nil, 0, nil
	}
	return nil, []byte("fake: unhandled " + name), 127, nil
}

// writeExecFiles parses the exec wrapper's redirections and records the
// fake output files in copyFrom (the wrapper: `( <cmd> ) > out 2> err` —
// the redirection operator is its OWN token, the path follows it).
func (f *fakeRun) writeExecFiles(wrapper string) {
	fields := strings.Fields(wrapper)
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case ">":
			if i+1 < len(fields) {
				i++
				f.copyFrom[fields[i]] = "stdout-line\n"
			}
		case "2>":
			if i+1 < len(fields) {
				i++
				f.copyFrom[fields[i]] = "stderr-line\n"
			}
		}
	}
}

func (f *fakeRun) hadCall(name string, sub ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c[0] != name {
			continue
		}
		ok := true
		for _, s := range sub {
			found := false
			for _, a := range c {
				if a == s {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// copyDir is a tiny recursive copy (the fake `cp -a`).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

// newTestBackend builds a linux backend fully in a temp data dir with the
// fake OS tools + a fake base rootfs.
func newTestBackend(t *testing.T) (*linuxBackend, *fakeRun, string) {
	t.Helper()
	dataDir := t.TempDir()
	// The base rootfs (what setup would have provisioned).
	baseRoot := BaseRoot(dataDir)
	if err := os.MkdirAll(filepath.Join(baseRoot, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "etc", "os-release"), []byte("NAME=TestOS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "etc", "machine-id"), []byte("fixed-template-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRun{copyFrom: map[string]string{}, started: map[string]bool{}, dataDir: dataDir}
	b := &linuxBackend{
		dataDir:     dataDir,
		cfg:         Config{Enabled: true, Network: "veth"},
		run:         fake.call,
		machinesDir: filepath.Join(dataDir, "machines"),
		unitBaseDir: filepath.Join(dataDir, "system"),
	}
	return b, fake, dataDir
}

func TestLinuxEnsureCreatesContainer(t *testing.T) {
	b, fake, dataDir := newTestBackend(t)
	ctx := context.Background()

	info, err := b.Ensure(ctx, "alice")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if info.State != StateRunning {
		t.Fatalf("state = %s, want running", info.State)
	}

	rootfs := b.rootfsDir("alice")
	// 1. The rootfs exists (copied from the base).
	if _, err := os.Stat(filepath.Join(rootfs, "etc", "os-release")); err != nil {
		t.Fatalf("rootfs missing after ensure: %v", err)
	}
	// 2. The template machine-id was reset to a single newline.
	id, err := os.ReadFile(filepath.Join(rootfs, "etc", "machine-id"))
	if err != nil || string(id) != "\n" {
		t.Fatalf("machine-id = %q (err %v), want a single newline", id, err)
	}
	// 3. The user data dir was created.
	if _, err := os.Stat(UserDataDir(dataDir, "alice")); err != nil {
		t.Fatalf("user data dir missing: %v", err)
	}
	// 4. A subnet was allocated + the in-container networkd config written.
	st := b.loadState("alice")
	if st.Subnet < 1 || st.Subnet > 254 {
		t.Fatalf("subnet = %d, want 1-254", st.Subnet)
	}
	net, err := os.ReadFile(filepath.Join(rootfs, "etc", "systemd", "network", "25-bevin.network"))
	if err != nil {
		t.Fatalf("networkd config missing: %v", err)
	}
	if want := fmt.Sprintf("Address=10.42.%d.2/24", st.Subnet); !strings.Contains(string(net), want) {
		t.Fatalf("networkd config = %s, want %q inside", net, want)
	}
	// 5. The unit drop-in pins the invocation (bind + veth + no private
	//    users) — the exact line is what the container runs with.
	dropIn, err := os.ReadFile(b.dropInPath("alice"))
	if err != nil {
		t.Fatalf("drop-in missing: %v", err)
	}
	di := string(dropIn)
	for _, want := range []string{
		"ExecStart=", // the clear line
		"--private-users=no",
		"--bind=" + UserDataDir(dataDir, "alice") + ":/data",
		"--network-veth",
		"--boot",
	} {
		if !strings.Contains(di, want) {
			t.Fatalf("drop-in =\n%s\nmissing %q", di, want)
		}
	}
	// 6. The image symlink points at the rootfs.
	if tgt, err := os.Readlink(b.machineSymlink("alice")); err != nil || tgt != rootfs {
		t.Fatalf("symlink = %q (err %v), want %s", tgt, err, rootfs)
	}
	// 7. The service was started + daemon-reload ran.
	if !fake.hadCall("systemctl", "start", "systemd-nspawn@bevin-alice") {
		t.Fatalf("systemctl start missing: %v", fake.calls)
	}
	if !fake.hadCall("systemctl", "daemon-reload") {
		t.Fatal("daemon-reload missing")
	}
	// 8. The host-side veth was configured (NATed).
	if !fake.hadCall("ip", "addr", "replace", fmt.Sprintf("10.42.%d.1/24", st.Subnet)) {
		t.Fatalf("ip addr replace missing: %v", fake.calls)
	}
	if !fake.hadCall("iptables", "MASQUERADE") {
		t.Fatal("iptables MASQUERADE missing")
	}

	// Idempotency: a second ensure on a running container is a no-op that
	// reports running (no re-copy, no re-start).
	nBefore := len(fake.calls)
	info, err = b.Ensure(ctx, "alice")
	if err != nil || info.State != StateRunning {
		t.Fatalf("second Ensure: %v / %s", err, info.State)
	}
	if len(fake.calls) != nBefore+1 { // only the is-active probe
		t.Fatalf("second Ensure made %d calls, want 1 (is-active): %v", len(fake.calls)-nBefore, fake.calls[nBefore:])
	}
}

func TestLinuxEnsurePrivateNetwork(t *testing.T) {
	b, fake, _ := newTestBackend(t)
	b.cfg.Network = "private"
	if _, err := b.Ensure(context.Background(), "bob"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dropIn, err := os.ReadFile(b.dropInPath("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dropIn), "--private-network") {
		t.Fatalf("drop-in missing --private-network:\n%s", dropIn)
	}
	if fake.hadCall("iptables") {
		t.Fatal("private mode must not touch iptables")
	}
	// No subnet allocation in private mode.
	if st := b.loadState("bob"); st.Subnet != 0 {
		t.Fatalf("private mode allocated subnet %d, want none", st.Subnet)
	}
}

func TestLinuxSubnetAllocationIsUnique(t *testing.T) {
	b, _, dataDir := newTestBackend(t)
	ctx := context.Background()
	// Allocate three users' containers; the subnets must be distinct.
	seen := map[int]bool{}
	for _, h := range []string{"alice", "bob", "carol"} {
		if _, err := b.Ensure(ctx, h); err != nil {
			t.Fatalf("Ensure(%s): %v", h, err)
		}
		n := b.loadState(h).Subnet
		if seen[n] {
			t.Fatalf("subnet %d allocated twice", n)
		}
		seen[n] = true
	}
	_ = dataDir
}

func TestLinuxExec(t *testing.T) {
	b, fake, _ := newTestBackend(t)
	ctx := context.Background()
	if _, err := b.Ensure(ctx, "alice"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	res, err := b.Exec(ctx, "alice", "echo hello", 10*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "stdout-line\n" || res.Stderr != "stderr-line\n" || res.ExitCode != 0 {
		t.Fatalf("result = %+v, want stdout/stderr lines + exit 0", res)
	}
	// The exec went through systemd-run -M (the native D-Bus path).
	if !fake.hadCall("systemd-run", "-M", "bevin-alice", "--wait") {
		t.Fatalf("systemd-run -M missing: %v", fake.calls)
	}
	// And the output was fetched with machinectl copy-from.
	if !fake.hadCall("machinectl", "copy-from") {
		t.Fatal("machinectl copy-from missing")
	}
}

func TestLinuxExecNotRunning(t *testing.T) {
	b, _, _ := newTestBackend(t)
	// No ensure: the container has never started.
	if _, err := b.Exec(context.Background(), "alice", "ls", 10*time.Second); err != ErrNotRunning {
		t.Fatalf("Exec = %v, want ErrNotRunning", err)
	}
}

func TestLinuxExecIn(t *testing.T) {
	b, fake, _ := newTestBackend(t)
	ctx := context.Background()
	if _, err := b.Ensure(ctx, "alice"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	const cwd = "/data/workspaces/abc123"
	res, err := b.ExecIn(ctx, "alice", "pwd", cwd, 10*time.Second)
	if err != nil {
		t.Fatalf("ExecIn: %v", err)
	}
	if res.Stdout != "stdout-line\n" || res.ExitCode != 0 {
		t.Fatalf("result = %+v, want the captured output", res)
	}
	// The systemd-run invocation carried --working-directory <cwd>.
	fake.mu.Lock()
	found := false
	for _, c := range fake.calls {
		if c[0] != "systemd-run" {
			continue
		}
		for i, a := range c {
			if a == "--working-directory" && i+1 < len(c) && c[i+1] == cwd {
				found = true
			}
		}
	}
	fake.mu.Unlock()
	if !found {
		t.Fatalf("no systemd-run call with --working-directory %s: %v", cwd, fake.calls)
	}
	// A relative working directory is rejected (container paths are
	// absolute).
	if _, err := b.ExecIn(ctx, "alice", "pwd", "relative/path", 10*time.Second); err == nil {
		t.Fatal("relative cwd accepted, want an error")
	}
	if got := b.ContainerDataDir(); got != "/data" {
		t.Fatalf("ContainerDataDir = %q, want /data", got)
	}
}

func TestLinuxStopAndRemove(t *testing.T) {
	b, fake, dataDir := newTestBackend(t)
	ctx := context.Background()
	if _, err := b.Ensure(ctx, "alice"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	info, err := b.Stop(ctx, "alice")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if info.State != StateStopped {
		t.Fatalf("after Stop = %s, want stopped", info.State)
	}
	if !fake.hadCall("systemctl", "stop", "systemd-nspawn@bevin-alice") {
		t.Fatal("systemctl stop missing")
	}
	// The storage + wiring survive a stop (persistent by design).
	if _, err := os.Stat(b.rootfsDir("alice")); err != nil {
		t.Fatal("storage vanished on stop")
	}

	if err := b.Remove(ctx, "alice"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(b.rootfsDir("alice")); !os.IsNotExist(err) {
		t.Fatal("storage survived Remove")
	}
	if _, err := os.Stat(b.dropInPath("alice")); !os.IsNotExist(err) {
		t.Fatal("drop-in survived Remove")
	}
	if _, err := os.Stat(b.machineSymlink("alice")); !os.IsNotExist(err) {
		t.Fatal("symlink survived Remove")
	}
	// The user's DATA dir is untouched by Remove.
	if _, err := os.Stat(UserDataDir(dataDir, "alice")); err != nil {
		t.Fatal("the user data dir must survive Remove")
	}
}

func TestLinuxStatus(t *testing.T) {
	b, _, _ := newTestBackend(t)
	ctx := context.Background()
	if info, err := b.Status(ctx, "alice"); err != nil || info.State != StateAbsent {
		t.Fatalf("Status(absent) = %s, %v", info.State, err)
	}
	if _, err := b.Ensure(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if info, err := b.Status(ctx, "alice"); err != nil || info.State != StateRunning {
		t.Fatalf("Status(running) = %s, %v", info.State, err)
	}
	if _, err := b.Stop(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if info, err := b.Status(ctx, "alice"); err != nil || info.State != StateStopped {
		t.Fatalf("Status(stopped) = %s, %v", info.State, err)
	}
}

func TestMachineName(t *testing.T) {
	// Short handles keep the readable name.
	if got := MachineName("alice"); got != "bevin-alice" {
		t.Fatalf("MachineName(alice) = %q", got)
	}
	// Long handles fall back to a stable hash — and the result must fit
	// the veth interface name limit (ve-<name> <= 15 chars).
	long := "a-very-long-handle-that-exceeds-twelve-chars"
	got := MachineName(long)
	if len("ve-"+got) > 15 {
		t.Fatalf("MachineName(%q) = %q: ve-+name = %d chars > 15", long, got, len("ve-"+got))
	}
	if again := MachineName(long); again != got {
		t.Fatal("MachineName is not deterministic")
	}
	// Distinct handles must not collide (the hash prefix has 40 bits).
	if MachineName("handle-x") == MachineName("handle-y") {
		t.Fatal("machine name collision")
	}
}

func TestLinuxRuntimeDetail(t *testing.T) {
	// The runtime detail must always be non-empty + actionable (whatever
	// fails first on this host).
	b, _, dataDir := newTestBackend(t)
	rt := b.Runtime(context.Background())
	if rt.Detail == "" {
		t.Fatal("empty detail")
	}
	// The base image is NOT a runtime gate (the setup's base provisioning
	// + Finalize own it — folding it into Runtime().OK deadlocked setup:
	// the base auto-download starts only when the runtime reports ready,
	// so a fresh machine could never get either ready). With no base
	// image, the runtime check must never cite it.
	os.RemoveAll(BaseRoot(dataDir))
	rt = b.Runtime(context.Background())
	if strings.Contains(rt.Detail, "base") {
		t.Fatalf("runtime check cites the base image (it is not a runtime gate): %q", rt.Detail)
	}
}
