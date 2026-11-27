//go:build linux

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The helper's matrix (the design log's WS2 section): a real pty round-trip
// (output → ring, the wait-chain's waiting_input bucket, input file → pty,
// exit + the shell-convention exit code), pipe mode (the exec-semantics
// diagnostic path), the ring wrap (the 64 KB budget), and the SIGTERM kill
// (the unit-stop path — the real binary, so the signal is real).
//
// These run a real command on a real pty: the box must have /dev/ptmx
// (linux; WSL2 does). They are the closest thing to the container path
// without a container (the host↔container plumbing — the state file in the
// mapped dir, the unit stop — is verified on first deploy).

// startHelper runs the helper in-process (run is the program body) and
// returns the state-file path. mode is "" (pty) or "--pipe".
func startHelper(t *testing.T, mode string, command string, args ...string) (statePath string) {
	t.Helper()
	dir := t.TempDir()
	statePath = filepath.Join(dir, "s.state")
	full := []string{"--state-file", statePath}
	if mode != "" {
		full = append(full, mode)
	}
	full = append(full, command)
	full = append(full, args...)
	go func() {
		code := run(full)
		if code != 0 {
			t.Errorf("run(%v) = %d, want 0", full, code)
		}
	}()
	return statePath
}

// waitState polls the state file until the predicate holds or the deadline
// passes (it returns the last state seen).
func waitState(t *testing.T, statePath string, deadline time.Duration, want string) *ptyStateFile {
	t.Helper()
	var last *ptyStateFile
	by := time.Now().Add(deadline)
	for time.Now().Before(by) {
		data, err := os.ReadFile(statePath)
		if err == nil {
			var st ptyStateFile
			if json.Unmarshal(data, &st) == nil {
				last = &st
				if want == "" || st.State == want {
					return &st
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("no state file appeared at %s (want state %q)", statePath, want)
	}
	t.Fatalf("timed out waiting for state %q (last: %s, ring: %q)", want, last.State, decode(t, last.Ring))
	return last
}

func decode(t *testing.T, b64 string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("bad ring base64: %v", err)
	}
	return string(data)
}

// TestPtyRoundTrip: a command that waits for a prompt, echoes the answer,
// and exits 0. The full interactive loop: output appears, the input file
// is consumed, the echo appears, and the session exits cleanly.
//
// (The wait-chain bucket: where /proc/<pid>/syscall is readable — the
// deploy target, bevin as root — the state says waiting_input while the
// command blocks on its prompt; on a box that hides the file (WSL2,
// unprivileged) it degrades to running and the model judges from the
// output. Either way the I/O loop is what this test asserts.)
func TestPtyRoundTrip(t *testing.T) {
	statePath := startHelper(t, "", "sh", "-c", "read x; echo got:$x")
	inPath := strings.TrimSuffix(statePath, ".state") + ".in"

	// The prompt: the command is blocked reading its stdin (waiting_input
	// where classifiable, running where /proc/<pid>/syscall is hidden).
	st := waitState(t, statePath, 5*time.Second, "")
	if st.State != "waiting_input" && st.State != "running" {
		t.Fatalf("initial state = %q, want waiting_input or running", st.State)
	}
	t.Logf("initial state: %s", st.State)

	// The model's answer, via the input file (the host appends).
	if err := os.WriteFile(inPath, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st = waitState(t, statePath, 5*time.Second, "exited")
	if st.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (ring: %q)", st.ExitCode, decode(t, st.Ring))
	}
	ring := decode(t, st.Ring)
	if !strings.Contains(ring, "got:hi") {
		t.Fatalf("ring = %q, want it to contain the echo 'got:hi'", ring)
	}
	// The prompt excerpt: the last non-empty line of the output.
	if st.Prompt != "got:hi" {
		t.Fatalf("prompt = %q, want %q", st.Prompt, "got:hi")
	}
}

// TestPtyPipeMode: --pipe (no pty — the exec-semantics diagnostic path):
// stdout + stderr both reach the ring, the exit code is the shell's.
func TestPtyPipeMode(t *testing.T) {
	statePath := startHelper(t, "--pipe", "sh", "-c", "printf out; printf err 1>&2; exit 3")
	st := waitState(t, statePath, 5*time.Second, "exited")
	if st.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", st.ExitCode)
	}
	ring := decode(t, st.Ring)
	if !strings.Contains(ring, "out") || !strings.Contains(ring, "err") {
		t.Fatalf("ring = %q, want both stdout and stderr", ring)
	}
}

// TestPtyRingWrap: more output than the ring (80 KB) — the ring keeps the
// LAST 64 KB + the total counter counts everything.
func TestPtyRingWrap(t *testing.T) {
	statePath := startHelper(t, "--pipe", "sh", "-c", "head -c 81920 /dev/zero | tr '\\0' 'x'")
	st := waitState(t, statePath, 10*time.Second, "exited")
	if st.TotalBytes < 81920 {
		t.Fatalf("total bytes = %d, want >= 81920", st.TotalBytes)
	}
	ring := decode(t, st.Ring)
	if len(ring) != ringBytes {
		t.Fatalf("ring length = %d, want %d (the budget)", len(ring), ringBytes)
	}
	if !strings.HasSuffix(ring, strings.Repeat("x", 100)) {
		t.Fatal("the ring must keep the TAIL of the output")
	}
}

// TestPtyBadCwd: an unlaunchable command still records an exited session
// (with the reason in the ring) — the host's wait loop sees it.
func TestPtyBadCwd(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "s.state")
	go func() {
		_ = run([]string{"--state-file", statePath, "--cwd", "/no/such/dir", "--", "sh", "-c", "true"})
	}()
	st := waitState(t, statePath, 5*time.Second, "exited")
	if st.ExitCode == 0 {
		t.Fatalf("a bad cwd must not exit 0 (ring: %q)", decode(t, st.Ring))
	}
}

// TestPtyKillOnSignal: the unit-stop path — a REAL binary (the signal has
// to be a real SIGTERM): a long sleep is started, the helper gets SIGTERM,
// and it (a) exits, (b) the state file says exited, (c) the command's
// process group is dead (no orphaned sleep).
func TestPtyKillOnSignal(t *testing.T) {
	// Build the real binary (the in-process run() cannot receive a real
	// signal from outside).
	bin := filepath.Join(t.TempDir(), "bevin-pty")
	b := exec.Command("go", "build", "-o", bin, ".")
	if out, err := b.CombinedOutput(); err != nil {
		t.Skipf("building the helper for the signal test failed (no toolchain?): %v (%s)", err, out)
	}
	statePath := filepath.Join(t.TempDir(), "s.state")
	cmd := exec.Command(bin, "--state-file", statePath, "--", "sh", "-c", "sleep 300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	// Let it settle (the initial state file).
	waitState(t, statePath, 5*time.Second, "")
	st := waitState(t, statePath, 5*time.Second, "running")
	_ = st
	// The sleep is its own process group (the helper's child): kill the
	// helper and verify the sleep died with its group.
	_ = cmd.Process.Signal(os.Interrupt) // same handler as the unit stop's SIGTERM
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the helper did not exit after SIGTERM within 5 s")
	}
	st = waitState(t, statePath, 5*time.Second, "exited")
	if st.ExitCode == 0 {
		t.Fatalf("a killed session must not exit 0 (got %d)", st.ExitCode)
	}
	// The orphan check, by exact pid (pgrep -f would match its own
	// wrapper shell): the helper's direct child (sh -c "sleep 300" — the
	// shell execs sleep, so the child IS the sleep) must be gone.
	time.Sleep(200 * time.Millisecond)
	if child := childPID(t, cmd.Process.Pid); child > 0 && pidAlive(child) {
		t.Fatalf("the command's process group survived the helper's SIGTERM (child %d still alive)", child)
	}
}

// childPID finds parent's direct child pid by walking /proc (0 when none
// — a dead child is the point of the check, so absence is reported as
// success by the caller).
func childPID(t *testing.T, parent int) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("no /proc to walk on this box: %v", err)
	}
	for _, e := range entries {
		if !isDigits(e.Name()) {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// stat: "pid (comm) state ppid ..." — comm may contain spaces.
		open := strings.Index(string(data), "(")
		closeIdx := strings.LastIndex(string(data), ")")
		if open < 0 || closeIdx < 0 {
			continue
		}
		fields := strings.Fields(string(data)[closeIdx+1:])
		if len(fields) < 2 || fields[1] != fmt.Sprintf("%d", parent) {
			continue
		}
		pid, _ := strconv.Atoi(e.Name())
		return pid
	}
	return 0
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func pidAlive(pid int) bool {
	_, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	return err == nil
}

// TestClassifySyscallFile: the wait-chain parser on synthetic
// /proc/<pid>/syscall contents (the format: "<syscall-number> <arg0> ..."
// — for read/write, arg0 is the fd).
func TestClassifySyscallFile(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"read on fd 0", "0 0x0 0x7ffd0000 0x1000 0x7ffd0001 0x7ffd0002 0xffffffffffffffff", "waiting_input"},
		{"read on fd 3", "0 0x3 0x7ffd0000 0x1000 0x0 0x0 0xffffffffffffffff", "waiting"},
		{"write on fd 1", "1 0x1 0x55aa 0x40 0x0 0x0 0xffffffffffffffff", "waiting"},
		{"read on fd 255", "0 0xff 0x7ffd0000 0x80 0x0 0x0 0xffffffffffffffff", "waiting"},
		{"in another syscall (accept)", "43 0x3 0x7ffd0000 0 0x0 0x0 0xffffffffffffffff", "running"},
		{"not in a syscall", "ffffffffffffffff ffffffffffffffd6 0 0 0 0 0", "running"},
		{"empty", "", "running"},
	}
	for _, c := range cases {
		if got := classifySyscallFile([]byte(c.data)); got != c.want {
			t.Errorf("%s: classify(%q) = %q, want %q", c.name, c.data, got, c.want)
		}
	}
}
