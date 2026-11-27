//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	hcsshim "github.com/Microsoft/hcsshim"
)

// The windows pty backend (the design log's WS2 section). A session is an
// HCS process with EmulateConsole (the same mechanism Docker uses for -t
// on Windows containers): stdio flows over the hcsshim process pipes
// directly into this process's in-memory ring — no state-file protocol
// (that is the linux path, where the runner is a separate process inside
// the container; here the runner IS the host process).
//
// The wait-chain state is running | exited in v1: the design's
// wait-reason refinement (NtQuerySystemInformation thread wait reasons) is
// a follow-up — the model judges "is it waiting for my input?" from the
// output alone (the prompt excerpt + a stalled total counter), which is
// the information the design gives it on both platforms.
//
// (Console encoding: the emulated console encodes guest output with the
// console code page — ASCII/UTF-8 command output is readable as-is;
// exotic code pages may arrive garbled. A follow-up can decode.)
type winPtySession struct {
	id     string
	handle string
	proc   hcsshim.Process
	stdin  io.WriteCloser

	mu         sync.Mutex
	ring       []byte
	totalBytes int64
	delivered  int64
	prompt     string
	line       strings.Builder
	exited     bool
	code       int
	readers    sync.WaitGroup
	exitCh     chan struct{} // closed when the child exits (wait fired)
}

// appendRing appends console output to the ring + updates the prompt (the
// last non-empty line, ≤promptMax chars — the helper's identical
// bookkeeping, in the host process).
func (s *winPtySession) appendRing(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = append(s.ring, b...)
	if len(s.ring) > ptyRingBytes {
		s.ring = s.ring[len(s.ring)-ptyRingBytes:]
	}
	s.totalBytes += int64(len(b))
	for _, c := range b {
		switch c {
		case '\n':
			line := strings.TrimRight(s.line.String(), " \r")
			if line != "" {
				if len(line) > 200 {
					line = line[len(line)-200:]
				}
				s.prompt = line
			}
			s.line.Reset()
		default:
			s.line.WriteRune(rune(c))
		}
	}
}

// deliverLocked returns the output since the last delivery (the ring-delta
// extraction, identical to ptySession.deliver — the ring wrap returns the
// whole ring + the missed count). The caller holds s.mu.
func (s *winPtySession) deliverLocked() (string, int64) {
	if s.totalBytes <= s.delivered {
		return "", 0
	}
	ringStart := s.totalBytes - int64(len(s.ring))
	var omitted int64
	var out string
	switch {
	case s.delivered >= ringStart:
		out = string(s.ring[s.delivered-ringStart:])
	default:
		omitted = ringStart - s.delivered
		out = string(s.ring)
	}
	s.delivered = s.totalBytes
	return out, omitted
}

// PtyStart starts an interactive session (an HCS console process).
func (b *windowsBackend) PtyStart(ctx context.Context, handle, command, cwd string, cols, rows int) (string, error) {
	// The environment-use lock (the WS2 concurrency rule).
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	if cols <= 0 {
		cols = ptyDefaultCols
	}
	if rows <= 0 {
		rows = ptyDefaultRows
	}
	c, err := hcsshim.OpenContainer(b.containerID(handle))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer c.Close()
	if cwd == "" {
		cwd = dataMount
	}
	// cmd /s /c "<command>" passes the command verbatim to the shell (the
	// exec contract, identical to ExecIn). EmulateConsole = the console
	// (Docker's -t on Windows); ConsoleSize is (height, width) per the HCS
	// schema.
	pc := &hcsshim.ProcessConfig{
		CommandLine:      `C:\Windows\System32\cmd.exe /d /s /c "` + command + `"`,
		WorkingDirectory: cwd,
		CreateStdInPipe:  true,
		CreateStdOutPipe: true,
		CreateStdErrPipe: true,
		EmulateConsole:   true,
		ConsoleSize:      [2]uint{uint(rows), uint(cols)},
	}
	p, err := c.CreateProcess(pc)
	if err != nil {
		return "", fmt.Errorf("starting the console process failed: %w", err)
	}
	stdin, stdout, stderr, err := p.Stdio()
	if err != nil {
		_ = p.Close()
		return "", fmt.Errorf("attaching to the console process failed: %w", err)
	}
	id, err := randomHex(8)
	if err != nil {
		_ = p.Close()
		return "", err
	}
	s := &winPtySession{id: id, handle: handle, proc: p, stdin: stdin, exitCh: make(chan struct{})}
	s.readers.Add(2)
	go s.readPipe(stdout)
	go s.readPipe(stderr)
	go func() {
		_ = p.Wait()
		code, cerr := p.ExitCode()
		if cerr != nil {
			code = 1
		}
		s.mu.Lock()
		s.exited = true
		s.code = code
		s.mu.Unlock()
		close(s.exitCh)
	}()
	b.ptyMu.Lock()
	b.ptySessions[id] = s
	b.ptyMu.Unlock()
	return id, nil
}

func (s *winPtySession) readPipe(r io.ReadCloser) {
	defer s.readers.Done()
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.appendRing(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// getWin looks up a session (the caller holds nothing — ptyMu guards it).
func (b *windowsBackend) getWin(handle, id string) (*winPtySession, bool) {
	b.ptyMu.Lock()
	defer b.ptyMu.Unlock()
	s, ok := b.ptySessions[id]
	if !ok || s.handle != handle {
		return nil, false
	}
	return s, true
}

// PtyWait blocks up to timeout for new output / a state change / the
// command exiting (a timeout is NOT an error — the model waits again).
func (b *windowsBackend) PtyWait(ctx context.Context, handle, sessionID string, timeout time.Duration) (PtyWaitResult, error) {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	s, ok := b.getWin(handle, sessionID)
	if !ok {
		return PtyWaitResult{}, fmt.Errorf("unknown pty session %q", sessionID)
	}
	timeout = ptyWaitTimeout(timeout)
	deadline := time.Now().Add(timeout)
	var lastState PtyState
	for {
		s.mu.Lock()
		var res PtyWaitResult
		switch {
		case s.exited:
			out, omitted := s.deliverLocked()
			res = PtyWaitResult{NewOutput: out, State: PtyStateExited, ExitCode: s.code, Prompt: s.prompt, TotalBytes: s.totalBytes, OmittedBytes: omitted}
		default:
			out, omitted := s.deliverLocked()
			state := PtyStateRunning // v1: the wait-reason refinement is a follow-up
			res = PtyWaitResult{NewOutput: out, State: state, Prompt: s.prompt, TotalBytes: s.totalBytes, OmittedBytes: omitted}
		}
		s.mu.Unlock()
		switch {
		case res.State == PtyStateExited:
			return res, nil
		case res.NewOutput != "" || res.State != lastState:
			return res, nil
		}
		lastState = res.State
		if time.Now().After(deadline) {
			return res, nil // timeout: the last known state (no error)
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(ptyPollInterval):
		}
	}
}

// PtyInput writes text to the session's stdin (the model's answer to a
// prompt, over the console's stdin pipe).
func (b *windowsBackend) PtyInput(ctx context.Context, handle, sessionID, text string) error {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	s, ok := b.getWin(handle, sessionID)
	if !ok {
		return fmt.Errorf("unknown pty session %q", sessionID)
	}
	if text == "" {
		return errors.New("pty input is empty (nothing to send)")
	}
	s.mu.Lock()
	exited, code := s.exited, s.code
	s.mu.Unlock()
	if exited {
		return fmt.Errorf("the session has already exited (exit code %d) — there is no process to send input to", code)
	}
	_, err := s.stdin.Write([]byte(text))
	return err
}

// closeWin tears one session down (the caller holds nothing; ptyMu is taken
// inside for the registry update).
func (b *windowsBackend) closeWin(s *winPtySession) int {
	_ = s.proc.Kill()
	select {
	case <-s.exitCh:
	case <-time.After(5 * time.Second):
	}
	// Drain the output buffers (the pipes reach EOF on the child's exit).
	done := make(chan struct{})
	go func() { s.readers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	s.mu.Lock()
	code := s.code
	s.mu.Unlock()
	_ = s.proc.Close()
	b.ptyMu.Lock()
	delete(b.ptySessions, s.id)
	b.ptyMu.Unlock()
	return code
}

// PtyClose ends the session (kill the console process) and returns its exit
// code (best effort).
func (b *windowsBackend) PtyClose(ctx context.Context, handle, sessionID string) (int, error) {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	s, ok := b.getWin(handle, sessionID)
	if !ok {
		return -1, fmt.Errorf("unknown pty session %q", sessionID)
	}
	return b.closeWin(s), nil
}

// PtyCloseAll ends every live session of the handle (the Stop/Remove/shutdown
// cleanup: a stopped environment takes its sessions with it).
func (b *windowsBackend) PtyCloseAll(ctx context.Context, handle string) error {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	var firstErr error
	b.ptyMu.Lock()
	var sessions []*winPtySession
	for _, s := range b.ptySessions {
		if s.handle == handle {
			sessions = append(sessions, s)
		}
	}
	b.ptyMu.Unlock()
	for _, s := range sessions {
		b.closeWin(s)
	}
	return firstErr
}
