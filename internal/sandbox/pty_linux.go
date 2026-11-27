//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The linux pty backend (the design log's WS2 section). A session is the
// bevin-pty helper (cmd/bevin-pty) running in the container as a transient
// unit: the helper owns the pty, the ring, the input-file pump, and the
// wait-chain classification; the host talks to it ONLY through the two
// files in the bind-mounted data dir (the state file: helper writes, host
// reads; the input file: host writes, helper reads). The helper binary is
// copied to /run/bevin-pty per start (the container's tmpfs — no rootfs
// surgery, no disk-cap impact, always the current version).
const (
	ptyContainerHelperPath = "/run/bevin-pty" // the helper's home (container tmpfs)
	ptyContainerDataDir    = "/data"          // the mapped data dir (the session files' home in the container)
	ptyStopTimeout         = 15 * time.Second // the unit stop (PtyClose)
)

// ptyUnitName is the session's transient unit name.
func ptyUnitName(id string) string { return "bevin-pty-" + id + ".service" }

// reapOrphanPty reclaims pty sessions this process does not own — the
// leftovers of a bevin crash/restart while the container kept running:
// their units (still alive inside the container) are stopped, and their
// state files are deleted. This process's live sessions (the registry)
// are never touched. Best-effort throughout: a reap failure only delays
// the cleanup to the next Ensure.
func (b *linuxBackend) reapOrphanPty(ctx context.Context, handle, m string) {
	b.pty.mu.Lock()
	live := map[string]bool{}
	for id := range b.pty.byHandle[handle] {
		live[id] = true
	}
	b.pty.mu.Unlock()
	// The orphan units, inside the container (list-units through the
	// nested runner — the same protocol as the exec cleanup).
	// Throttle record first (a failed reap must not re-run on every
	// Ensure within the minute).
	b.ptyReapMu.Lock()
	b.ptyReapAt[handle] = time.Now()
	b.ptyReapMu.Unlock()
	out, _, code, err := b.run(ctx, "", "systemd-run", "-M", m, "--wait", "--quiet", "sh", "-c", "systemctl list-units --no-legend --no-pager 'bevin-pty-*.service' 2>/dev/null | awk '{print $1}'")
	if err == nil && code == 0 {
		for _, line := range strings.Split(string(out), "\n") {
			unit := strings.TrimSpace(line)
			if unit == "" {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(unit, "bevin-pty-"), ".service")
			if live[id] {
				continue
			}
			b.run(ctx, "", "systemd-run", "-M", m, "--wait", "--quiet", "sh", "-c", "systemctl stop "+unit+" 2>/dev/null")
		}
	}
	// The stale state files: everything not live (a stopped machine has
	// no live sessions — this also clears leftovers from a machine that
	// was down when bevin restarted).
	dir := ptySessionDir(b.dataDir, handle)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".state") {
			continue
		}
		id := strings.TrimSuffix(name, ".state")
		if live[id] {
			continue
		}
		os.Remove(filepath.Join(dir, name))
		os.Remove(inPathForState(filepath.Join(dir, name)))
	}
}

// reapOrphanPtyThrottled is the "already running" Ensure path: orphans can
// only exist after a bevin restart (fresh registry), so the reap runs at
// most once a minute per handle (the throttle is recorded by
// reapOrphanPty — a created-path reap counts too). The reap itself skips
// the live sessions (the registry), so a mixed state — this process's
// session running next to a pre-crash orphan — is reclaimed correctly.
func (b *linuxBackend) reapOrphanPtyThrottled(ctx context.Context, handle string) {
	b.ptyReapMu.Lock()
	last := b.ptyReapAt[handle]
	if time.Since(last) < 60*time.Second {
		b.ptyReapMu.Unlock()
		return
	}
	b.ptyReapMu.Unlock()
	b.reapOrphanPty(ctx, handle, b.machineName(handle))
}

// PtyStart starts an interactive session (the design log's WS2 section).
func (b *linuxBackend) PtyStart(ctx context.Context, handle, command, cwd string, cols, rows int) (string, error) {
	// The environment-use lock (one use of the environment at a time per
	// user — the WS2 rule: sessions and execs of the same user serialize).
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	if ok, err := b.running(ctx, handle); err != nil {
		return "", err
	} else if !ok {
		return "", ErrNotRunning
	}
	if cols <= 0 {
		cols = ptyDefaultCols
	}
	if rows <= 0 {
		rows = ptyDefaultRows
	}
	helper, err := helperBinaryPath()
	if err != nil {
		return "", err
	}
	m := b.machineName(handle)
	id, err := randomHex(8)
	if err != nil {
		return "", err
	}
	// The session's directory in the mapped data dir (the container sees
	// the same tree at /data/.bevin-pty).
	if err := os.MkdirAll(ptySessionDir(b.dataDir, handle), 0o700); err != nil {
		return "", err
	}
	// The helper into the container's tmpfs (+ the exec bit: copy-to
	// preserves the mode, the chmod is the guarantee).
	if _, errb, code, err := b.run(ctx, "", "machinectl", "copy-to", m, helper, ptyContainerHelperPath); err != nil || code != 0 {
		return "", fmt.Errorf("copying the pty helper into the container failed: %v (%s)", err, tail(errb))
	}
	if _, errb, code, err := b.run(ctx, "", "systemd-run", "-M", m, "--wait", "--quiet", "/bin/chmod", "755", ptyContainerHelperPath); err != nil || code != 0 {
		return "", fmt.Errorf("making the pty helper executable failed: %v (%s)", err, tail(errb))
	}
	unit := ptyUnitName(id)
	// Launch the helper as a transient unit (NO --wait: the unit lives for
	// the session's lifetime; the helper's stdio is the journal — all real
	// I/O flows over the data-dir files). The command goes to the
	// environment's shell (the exec contract: arbitrary code is the point).
	args := []string{"-M", m, "--unit", unit, ptyContainerHelperPath,
		"--state-file", ptyContainerDataDir + "/.bevin-pty/" + id + ".state"}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	args = append(args, "--cols", fmt.Sprintf("%d", cols), "--rows", fmt.Sprintf("%d", rows),
		"--", "/bin/sh", "-c", command)
	if _, errb, code, err := b.run(ctx, "", "systemd-run", args...); err != nil || code != 0 {
		return "", fmt.Errorf("starting the pty session failed: %v (%s)", err, tail(errb))
	}
	s := &ptySession{
		id:        id,
		handle:    handle,
		unit:      unit,
		statePath: filepath.Join(ptySessionDir(b.dataDir, handle), id+".state"),
	}
	b.pty.add(s)
	return id, nil
}

// PtyWait blocks up to timeout for new output / a state change / the
// command exiting (a timeout is NOT an error — the model waits again).
func (b *linuxBackend) PtyWait(ctx context.Context, handle, sessionID string, timeout time.Duration) (PtyWaitResult, error) {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	s, ok := b.pty.get(handle, sessionID)
	if !ok {
		return PtyWaitResult{}, fmt.Errorf("unknown pty session %q", sessionID)
	}
	timeout = ptyWaitTimeout(timeout)
	deadline := time.Now().Add(timeout)
	var lastState PtyState
	for {
		st, rerr := readPtyStateFile(s.statePath)
		if rerr == nil {
			newOut, omitted := s.deliver(st)
			state := mapPtyState(st.State)
			switch {
			case state == PtyStateExited:
				return PtyWaitResult{NewOutput: newOut, State: state, ExitCode: st.ExitCode, Prompt: st.Prompt, TotalBytes: st.TotalBytes, OmittedBytes: omitted}, nil
			case newOut != "" || state != lastState:
				return PtyWaitResult{NewOutput: newOut, State: state, Prompt: st.Prompt, TotalBytes: st.TotalBytes, OmittedBytes: omitted}, nil
			}
			lastState = state
		}
		if time.Now().After(deadline) {
			// Timeout: return the last known state (no error — the model
			// simply waits again).
			return PtyWaitResult{State: lastState}, nil
		}
		select {
		case <-ctx.Done():
			return PtyWaitResult{State: lastState}, ctx.Err()
		case <-time.After(ptyPollInterval):
		}
	}
}

// PtyInput writes text to the session's stdin (the host appends to the
// input file; the helper polls it).
func (b *linuxBackend) PtyInput(ctx context.Context, handle, sessionID, text string) error {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	s, ok := b.pty.get(handle, sessionID)
	if !ok {
		return fmt.Errorf("unknown pty session %q", sessionID)
	}
	if text == "" {
		return errors.New("pty input is empty (nothing to send)")
	}
	// A session that has already exited has no process to receive it.
	if st, rerr := readPtyStateFile(s.statePath); rerr == nil && mapPtyState(st.State) == PtyStateExited {
		return fmt.Errorf("the session has already exited (exit code %d) — there is no process to send input to", st.ExitCode)
	}
	inPath := inPathForState(s.statePath)
	f, err := os.OpenFile(inPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write([]byte(text))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// PtyClose ends the session (unit stop → the helper's SIGTERM handler kills
// the command's process group) and returns its exit code (best effort).
func (b *linuxBackend) PtyClose(ctx context.Context, handle, sessionID string) (int, error) {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	s, ok := b.pty.get(handle, sessionID)
	if !ok {
		return -1, fmt.Errorf("unknown pty session %q", sessionID)
	}
	return b.ptyCloseLocked(ctx, s)
}

// ptyCloseLocked tears one session down (the caller holds the handle lock).
func (b *linuxBackend) ptyCloseLocked(ctx context.Context, s *ptySession) (int, error) {
	m := b.machineName(s.handle)
	ctx2, cancel := context.WithTimeout(ctx, ptyStopTimeout)
	defer cancel()
	// The nested systemctl is the proven unit-stop path (cleanupExec's
	// pattern — systemctl -M is not relied on; the machine bus over
	// varlink may not be reachable).
	_, errb, code, err := b.run(ctx2, "", "systemd-run", "-M", m, "--wait", "sh", "-c",
		"systemctl stop "+s.unit+" 2>/dev/null; true")
	if err != nil || code != 0 {
		// A unit-stop failure is not fatal: the session's files are
		// cleaned up and the registry entry dropped either way (the
		// container's own teardown catches an orphaned unit).
		_ = errb
	}
	codeOut := -1
	if st, rerr := readPtyStateFile(s.statePath); rerr == nil && mapPtyState(st.State) == PtyStateExited {
		codeOut = st.ExitCode
	}
	// The session files (host side — the data dir is the user's, keep it
	// clean of session garbage).
	os.Remove(s.statePath)
	os.Remove(inPathForState(s.statePath))
	b.pty.remove(s.handle, s.id)
	return codeOut, nil
}

// PtyCloseAll ends every live session of the handle (the Stop/Remove/shutdown
// cleanup: a stopped environment takes its sessions with it).
func (b *linuxBackend) PtyCloseAll(ctx context.Context, handle string) error {
	lk := b.envMu.lock(handle)
	lk.Lock()
	defer lk.Unlock()
	var firstErr error
	for _, id := range b.pty.list(handle) {
		s, ok := b.pty.get(handle, id)
		if !ok {
			continue
		}
		if _, err := b.ptyCloseLocked(ctx, s); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// inPathForState returns the session's input-file path from its state-file
// path (<...>.state → <...>.in — the helper derives the same path; both
// ends must agree).
func inPathForState(statePath string) string {
	return strings.TrimSuffix(statePath, ".state") + ".in"
}
