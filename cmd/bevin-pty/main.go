//go:build linux

// bevin-pty — the in-container pty runner (the design log's WS2 section).
// Linux-only (the pty + /proc wait-chain protocol; the windows backend's
// sessions run in the host process over hcsshim pipes — no helper).
//
// The host launches it as a transient systemd unit inside the user's
// container (the helper binary is copied to /run/bevin-pty per start — the
// container's tmpfs: no rootfs surgery, no disk-cap impact, always the
// current version):
//
//	systemd-run -M <m> --unit bevin-pty-<id>.service /run/bevin-pty \
//	  --state-file /data/.bevin-pty/<id>.state [--cwd P] [--pipe] \
//	  [--cols N --rows N] -- <command> [args...]
//
// It runs the command on a pseudo-terminal (pty mode; --pipe = plain
// pipes, for the exec-semantics diagnostic) and does three things:
//
//  1. Output bridge: everything the command writes is kept in an in-memory
//     ring (the last 64 KB) + a total-byte counter.
//  2. Input bridge: the host appends the model's input to the session's
//     input file (<state-file>.in); this process polls it (~100 ms) and
//     writes the bytes to the pty master (or the stdin pipe in pipe mode).
//  3. Wait-chain classification: a blocking read on fd 0 = waiting_input
//     (only a process reading stdin does that); a blocking read/write on
//     any other fd = waiting (a child, the network); anything else =
//     running. The classification + ring + counters are written ATOMICALLY
//     (tmp + rename) to the state file in the bind-mounted data dir.
//
// ALL I/O flows over the bind-mounted data dir — there is no side-channel
// protocol: the host never touches this process's stdio (the unit's stdio
// is journal/null). SIGTERM (the unit stop on PtyClose) kills the command's
// process group, writes the final state, and exits.
//
// No cgo: the pty comes from /dev/ptmx + ioctl via golang.org/x/sys/unix
// (pure Go), so the binary is static and runs anywhere linux.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ringBytes   = 64 << 10 // the output ring (last 64 KB)
	pollInputMs = 100      // input-file poll interval
	pollStateMs = 200      // wait-chain classification interval
	livenessSec = 1        // minimum state-file rewrite cadence (a liveness marker)
	promptMax   = 200      // the prompt excerpt cap (last non-empty output line)
)

// ptyStateFile is the state-file format (mirrors internal/sandbox.ptyStateFile).
type ptyStateFile struct {
	State      string `json:"state"`
	ExitCode   int    `json:"exit_code"`
	TotalBytes int64  `json:"total_bytes"`
	Prompt     string `json:"prompt"`
	Ring       string `json:"ring"` // base64
	UpdatedAt  int64  `json:"updated_at"`
}

func main() { os.Exit(run(os.Args[1:])) }

// run is the helper's whole program (a function so the tests can drive it
// in-process; main is the thin os.Exit wrapper).
func run(args []string) int {
	fs := flag.NewFlagSet("bevin-pty", flag.ContinueOnError)
	stateFile := fs.String("state-file", "", "the state-file path (in the bind-mounted data dir)")
	cwd := fs.String("cwd", "", "working directory (container-absolute)")
	pipe := fs.Bool("pipe", false, "plain pipes instead of a pty (the exec-semantics diagnostic)")
	cols := fs.Int("cols", 80, "the pty width")
	rows := fs.Int("rows", 24, "the pty height")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cmdArgs := fs.Args()
	if *stateFile == "" || len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: bevin-pty --state-file PATH [--cwd P] [--pipe] [--cols N --rows N] -- COMMAND [ARGS...]")
		return 2
	}

	h := &helper{
		statePath: *stateFile,
		inPath:    strings.TrimSuffix(*stateFile, ".state") + ".in",
		cwd:       *cwd,
		pipe:      *pipe,
		cols:      uint16(*cols),
		rows:      uint16(*rows),
	}
	if err := h.start(cmdArgs); err != nil {
		// A launch failure is still recorded (the host's wait loop sees
		// an exited session with the reason in the ring).
		h.appendRing([]byte(err.Error() + "\n"))
		h.writeState("exited", 127)
		return 1
	}
	defer h.killChild()

	// SIGTERM (the unit stop): kill the command's group, record the exit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		h.killChild()
	}()

	// The initial state file (the host's wait loop sees the session from
	// the first poll).
	h.writeState(h.classify(), 0)
	h.loop()
	h.killChild()
	return 0
}

// helper is one live session's state (the ring appends are mutex-guarded —
// the output readers run in goroutines; everything else runs on the loop).
type helper struct {
	statePath  string
	inPath     string
	cwd        string
	pipe       bool
	cols, rows uint16

	cmd *exec.Cmd
	// readers: the output-bridge goroutines — the exit path waits for
	// them (the kernel pipe/pty buffer must drain BEFORE the final state
	// write, or the tail of the output is lost).
	readers sync.WaitGroup

	mu         sync.Mutex
	ring       []byte
	totalBytes int64
	prompt     string
	line       strings.Builder // the in-progress output line (the prompt source)

	exitMu     sync.Mutex
	exited     bool
	exitStatus int
	inOffset   int64 // how much of the input file has been consumed
}

// start launches the command (pty or pipes) + its I/O goroutines.
func (h *helper) start(cmdArgs []string) error {
	h.cmd = exec.Command(cmdArgs[0], cmdArgs[1:]...)
	if h.cwd != "" {
		h.cmd.Dir = h.cwd
	}
	if h.pipe {
		// The pipes, wired for the CHILD (it reads stdin / writes
		// stdout+stderr); the parent holds the other ends.
		stdinR, stdinW, errIn := os.Pipe()
		stdoutR, stdoutW, errOut := os.Pipe()
		stderrR, stderrW, errErr := os.Pipe()
		if errIn != nil || errOut != nil || errErr != nil {
			return fmt.Errorf("creating the pipes failed: %v", errIn)
		}
		h.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		h.cmd.Stdin, h.cmd.Stdout, h.cmd.Stderr = stdinR, stdoutW, stderrW
		if err := h.cmd.Start(); err != nil {
			return err
		}
		h.readers.Add(2)
		go h.readToRing(stdoutR)
		go h.readToRing(stderrR)
		go h.inputPump(stdinW)
		return nil
	}
	// The pty (no x/sys/unix.Openpty on linux: /dev/ptmx + TIOCGPTN is
	// the kernel's pure-pty path, all plain syscalls — no cgo).
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening /dev/ptmx failed: %w", err)
	}
	var n int32
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(master), uintptr(unix.TIOCGPTN), uintptr(unsafe.Pointer(&n)))
	if errno != 0 {
		unix.Close(master)
		return fmt.Errorf("the pty's slave name lookup failed: %w", errno)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", int(n))
	// grantpt + unlockpt, by hand (fchown to the opener + chmod 0600 +
	// clear the lock flag — exactly what the libc pair does on linux;
	// the slave open EIOs without the unlock).
	_ = unix.Fchown(master, os.Getuid(), os.Getgid())
	_ = unix.Fchmod(master, 0o600)
	var lck int
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(master), uintptr(unix.TIOCSPTLCK), uintptr(unsafe.Pointer(&lck))); errno != 0 {
		unix.Close(master)
		return fmt.Errorf("unlocking the pty failed: %w", errno)
	}
	slave, err := unix.Open(slavePath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(master)
		return fmt.Errorf("opening the pty slave %s failed: %w", slavePath, err)
	}
	if err := unix.IoctlSetWinsize(master, unix.TIOCSWINSZ, &unix.Winsize{Row: h.rows, Col: h.cols}); err != nil {
		unix.Close(master)
		unix.Close(slave)
		return fmt.Errorf("setting the pty size failed: %w", err)
	}
	// Ctty is the CHILD's fd of the controlling tty (the fd table is
	// rearranged before the TIOCSCTTY ioctl runs: the child's stdin IS
	// the slave, so 0 — a parent fd number here would fail the validity
	// check).
	h.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	slaveFile := os.NewFile(uintptr(slave), "pty-slave")
	h.cmd.Stdin, h.cmd.Stdout, h.cmd.Stderr = slaveFile, slaveFile, slaveFile
	if err := h.cmd.Start(); err != nil {
		unix.Close(master)
		slaveFile.Close()
		return err
	}
	slaveFile.Close() // the host-side copy (the child holds its dup)
	h.readers.Add(1)
	go h.readToRing(os.NewFile(uintptr(master), "pty-master"))
	go h.inputPump(os.NewFile(uintptr(master), "pty-master"))
	return nil
}

// appendRing appends output to the ring + updates the prompt (the last
// non-empty line, ≤promptMax chars).
func (h *helper) appendRing(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = append(h.ring, b...)
	if len(h.ring) > ringBytes {
		h.ring = h.ring[len(h.ring)-ringBytes:]
	}
	h.totalBytes += int64(len(b))
	for _, c := range b {
		switch c {
		case '\n':
			line := strings.TrimRight(h.line.String(), " \r")
			if line != "" {
				if len(line) > promptMax {
					line = line[len(line)-promptMax:]
				}
				h.prompt = line
			}
			h.line.Reset()
		default:
			h.line.WriteRune(rune(c))
		}
	}
}

// readToRing drains r into the ring (the output bridge).
func (h *helper) readToRing(r *os.File) {
	defer h.readers.Done()
	defer r.Close()
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.appendRing(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// inputPump polls the input file and writes its new bytes to w (the pty
// master / the stdin pipe) until the child exits.
func (h *helper) inputPump(w *os.File) {
	defer w.Close()
	t := time.NewTicker(pollInputMs * time.Millisecond)
	defer t.Stop()
	buf := make([]byte, 8192)
	for range t.C {
		if h.isExited() {
			return
		}
		f, err := os.Open(h.inPath)
		if err != nil {
			continue // the host has not written any input yet
		}
		for {
			if h.isExited() {
				break
			}
			f.Seek(h.inOffset, 0)
			n, rerr := f.Read(buf)
			if n > 0 {
				h.inOffset += int64(n)
				if _, werr := w.Write(buf[:n]); werr != nil {
					break // the child is gone: stop the pump
				}
			}
			if rerr != nil || n < len(buf) {
				break // EOF
			}
		}
		f.Close()
	}
}

// classify is the wait-chain classification (Refinement A of the design
// log, the linux side): the child's /proc/<pid>/syscall shows the syscall
// it is blocked in (and its first argument = the fd). A blocking read on
// fd 0 = waiting_input; a blocking read/write anywhere else = waiting;
// everything else = running. (On a box where /proc/<pid>/syscall is not
// readable — e.g. WSL2 for unprivileged readers — it degrades to running:
// the model then judges from the output alone, which the design already
// gives it. Deploy targets run as root, where the file is readable.)
func (h *helper) classify() string {
	if h.isExited() {
		return "exited"
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/syscall", h.cmd.Process.Pid))
	if err != nil {
		return "running" // gone from under us (or unreadable): next iteration sees the exit
	}
	return classifySyscallFile(data)
}

// classifySyscallFile parses one /proc/<pid>/syscall file (the pure part
// of classify — unit-testable without /proc access):
// "<syscall-number> <arg0> <arg1> ..." — the syscall number is decimal,
// the arguments are 0x-prefixed hex; for read(2)/write(2) arg0 is the fd.
// A blocking read on fd 0 = waiting_input (only a process reading stdin
// does that); a blocking read/write anywhere else = waiting; not in a
// syscall (e.g. "-1" for a kernel sleep) = running.
func classifySyscallFile(data []byte) string {
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 2 {
		return "running"
	}
	// Base 0 (the Go-literal convention): decimal without a prefix, hex
	// with 0x — the file's exact format.
	sysno, err1 := strconv.ParseUint(fields[0], 0, 64)
	fd, err2 := strconv.ParseUint(fields[1], 0, 64)
	if err1 != nil || err2 != nil {
		return "running" // not in a syscall (e.g. 0xffffffffffffffff)
	}
	switch sysno {
	case 0: // read
		if fd == 0 {
			return "waiting_input"
		}
		return "waiting"
	case 1: // write (blocked writing output — a slow consumer)
		return "waiting"
	default:
		return "running"
	}
}

func (h *helper) isExited() bool {
	h.exitMu.Lock()
	defer h.exitMu.Unlock()
	return h.exited
}

// code returns the recorded exit status (0 while the child is running).
func (h *helper) code() int {
	h.exitMu.Lock()
	defer h.exitMu.Unlock()
	return h.exitStatus
}

// markExited records the child's exit (code 128+signal for a signalled
// death — the shell convention the model recognizes).
func (h *helper) markExited(werr error) {
	h.exitMu.Lock()
	defer h.exitMu.Unlock()
	if h.exited {
		return
	}
	h.exited = true
	if h.cmd != nil && h.cmd.ProcessState != nil {
		if ws, ok := h.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			switch {
			case ws.Signaled():
				h.exitStatus = 128 + int(ws.Signal())
			case ws.Exited():
				h.exitStatus = ws.ExitStatus()
			}
		}
		if h.exitStatus == 0 && werr != nil {
			h.exitStatus = 1
		}
	} else {
		h.exitStatus = 1
	}
}

// killChild kills the command's process group (the child is its own group
// leader: Setsid in pty mode, Setpgid in pipe mode). Idempotent.
func (h *helper) killChild() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	if h.isExited() {
		return
	}
	_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
	_ = h.cmd.Process.Kill()
	done := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		h.markExited(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// writeState writes the state file atomically (tmp + rename: the host
// never reads a half-written file).
func (h *helper) writeState(state string, code int) {
	h.mu.Lock()
	st := ptyStateFile{
		State:      state,
		ExitCode:   code,
		TotalBytes: h.totalBytes,
		Prompt:     h.prompt,
		Ring:       base64.StdEncoding.EncodeToString(h.ring),
		UpdatedAt:  time.Now().Unix(),
	}
	h.mu.Unlock()
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := h.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, h.statePath)
}

// loop is the main loop: reap the child when it exits, then rewrite the
// state file whenever the state/output moves (and at least once a second —
// the liveness marker for the host's poll).
func (h *helper) loop() {
	// The child's wait (a goroutine: killChild may reap it first —
	// markExited is idempotent).
	waitDone := make(chan struct{})
	go func() {
		werr := h.cmd.Wait()
		h.markExited(werr)
		close(waitDone)
	}()
	t := time.NewTicker(pollStateMs * time.Millisecond)
	defer t.Stop()
	var lastState string
	var lastTotal int64
	lastWrite := time.Now()
	for {
		if h.isExited() {
			// Drain the output buffers BEFORE the final state write: the
			// child's last bytes are still in the kernel pipe/pty buffer
			// (the readers return on EOF, which the child's exit causes).
			done := make(chan struct{})
			go func() { h.readers.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
			h.writeState("exited", h.code())
			return
		}
		state := h.classify()
		h.mu.Lock()
		total := h.totalBytes
		h.mu.Unlock()
		if state != lastState || total != lastTotal || time.Since(lastWrite) > livenessSec*time.Second {
			h.writeState(state, 0)
			lastState, lastTotal, lastWrite = state, total, time.Now()
			continue
		}
		select {
		case <-waitDone:
		case <-t.C:
		}
	}
}
