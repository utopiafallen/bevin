package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The interactive (PTY) layer (the design log's WS2 section). A pty session
// runs a command in the user's environment on a pseudo-terminal (windows:
// an HCS console) and exposes the wait-chain state the model needs to drive
// it: is it running, blocked waiting for INPUT, blocked on something else,
// or exited?
//
// The state contract (Refinement A of the design log): the model-facing
// bucket is active | waiting_input | waiting | exited. "waiting_input"
// REQUIRES both the OS's blocked-on-stdin state and a prompt in the recent
// output — the task agent (which sees the output) makes that call from the
// state + the prompt excerpt. Linux classifies from the child's
// /proc/<pid>/syscall (a blocking read on fd 0 = waiting on input; only a
// process reading stdin does that); the helper (cmd/bevin-pty) writes the
// classification to a state file in the bind-mounted data dir (no
// side-channel protocol; the host polls it directly). Windows v1 reports
// running | exited with the prompt excerpt (the wait-reason refinement via
// NtQuerySystemInformation is a follow-up — the model judges from the
// output either way).

// PtyState is the model-facing wait-chain bucket for a pty session.
type PtyState string

const (
	PtyStateRunning      PtyState = "running"       // active (or state unknown)
	PtyStateWaitingInput PtyState = "waiting_input" // blocked reading its stdin
	PtyStateWaiting      PtyState = "waiting"       // blocked on something else (a child, the network)
	PtyStateExited       PtyState = "exited"
)

// PtyWaitResult is one PtyWait read: the output produced since the previous
// wait ("" when there is none), the current state, and — when exited — the
// exit code. OmittedBytes counts output lost to the ring wrap between this
// wait and the previous one (the 64 KB budget: the tail is kept, the count
// says how much was dropped).
type PtyWaitResult struct {
	NewOutput    string
	State        PtyState
	ExitCode     int // valid when State == PtyStateExited
	Prompt       string
	TotalBytes   int64
	OmittedBytes int64
}

// ptyRingBytes is the output ring: the LAST 64 KB of a session's output is
// retained (the budget decision — output is re-derivable: the model re-runs
// with a narrower command, exactly like exec's output tail).
const ptyRingBytes = 64 << 10

// The session defaults (both platforms) + the host's poll interval.
const (
	ptyDefaultCols  = 80
	ptyDefaultRows  = 24
	ptyPollInterval = 250 * time.Millisecond // the host's state-file / ring poll
)

// ptyStateFile is the state-file format the bevin-pty helper maintains in
// the bind-mounted data dir (host: <data>/users/<h>/.bevin-pty/<id>.state;
// container: /data/.bevin-pty/<id>.state — the helper writes, the host
// reads directly: no per-poll container hop, no side-channel protocol).
// The ring is base64 (JSON-safe). The helper rewrites it atomically
// (tmp + rename) whenever the state/output moves and at least once a
// second (a liveness marker for the host's poll).
type ptyStateFile struct {
	State      string `json:"state"` // running | waiting_input | waiting | exited
	ExitCode   int    `json:"exit_code"`
	TotalBytes int64  `json:"total_bytes"`
	Prompt     string `json:"prompt"` // last non-empty output line, ≤200 chars
	Ring       string `json:"ring"`   // base64: the last ptyRingBytes of output
	UpdatedAt  int64  `json:"updated_at"`
}

// ptySessionDir is the session's directory in the bind-mounted data dir
// (host side; the container sees the same tree at /data/.bevin-pty).
func ptySessionDir(dataDir, handle string) string {
	return filepath.Join(UserDataDir(dataDir, handle), ".bevin-pty")
}

// ptySession is the host-side bookkeeping for one live pty session.
type ptySession struct {
	id        string
	handle    string
	unit      string // linux: the transient unit name; windows: "" (the process is held in-memory)
	statePath string // host absolute path of the state file (linux); windows: "" (the ring is in-memory)
	// deliveredBytes is the TotalBytes watermark last delivered to the
	// model; deliver() hands out exactly the bytes after it (or the whole
	// ring + an omitted count when the ring wrapped past it).
	deliveredBytes int64
}

// deliver computes the output the model should see since the last read
// (the ring-delta extraction: the bytes of the ring after deliveredBytes,
// or the whole ring — with the missed count — when the ring wrapped past
// it).
func (s *ptySession) deliver(st *ptyStateFile) (newOut string, omitted int64) {
	if st.TotalBytes <= s.deliveredBytes {
		return "", 0
	}
	ring, err := base64.StdEncoding.DecodeString(st.Ring)
	if err != nil || len(ring) == 0 {
		return "", 0
	}
	ringStart := st.TotalBytes - int64(len(ring))
	switch {
	case s.deliveredBytes >= ringStart:
		newOut = string(ring[s.deliveredBytes-ringStart:])
	default:
		omitted = ringStart - s.deliveredBytes
		newOut = string(ring)
	}
	s.deliveredBytes = st.TotalBytes
	return newOut, omitted
}

// ptyRegistry is the in-memory session registry (one per backend): a
// bevin restart loses it (in-memory by design — the sessions are live
// processes, not durable state; a restart's Stop/Remove/shutdown cleanup
// is what the environment does to them).
type ptyRegistry struct {
	mu       sync.Mutex
	byID     map[string]*ptySession
	byHandle map[string]map[string]bool
}

func newPtyRegistry() *ptyRegistry {
	return &ptyRegistry{byID: map[string]*ptySession{}, byHandle: map[string]map[string]bool{}}
}

func (r *ptyRegistry) add(s *ptySession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[s.id] = s
	set := r.byHandle[s.handle]
	if set == nil {
		set = map[string]bool{}
		r.byHandle[s.handle] = set
	}
	set[s.id] = true
}

func (r *ptyRegistry) get(handle, id string) (*ptySession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	if !ok || s.handle != handle {
		return nil, false
	}
	return s, true
}

// remove drops the session (idempotent).
func (r *ptyRegistry) remove(handle, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byID[id]; ok && s.handle == handle {
		delete(r.byID, id)
	}
	if set := r.byHandle[handle]; set != nil {
		delete(set, id)
		if len(set) == 0 {
			delete(r.byHandle, handle)
		}
	}
}

// list returns the handle's live session ids (a snapshot).
func (r *ptyRegistry) list(handle string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.byHandle[handle] {
		ids = append(ids, id)
	}
	return ids
}

// envLocks serializes environment-USE operations per user (the design
// log's WS2 concurrency rule: the environment is the queue, not the model
// slot — two conversations driving the same container at once would race
// on the guest's locks (dpkg), its single session, and its state).
// Exec/ExecIn take it too: an exec `apt install` must not start mid-pty-apt.
type envLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock returns the handle's environment-use mutex (created on first use).
func (l *envLocks) lock(handle string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.m[handle]
	if !ok {
		m = &sync.Mutex{}
		l.m[handle] = m
	}
	return m
}

// newEnvLocks makes a ready envLocks (the backends hold one per process;
// the internal map must exist before the first lock).
func newEnvLocks() *envLocks {
	return &envLocks{m: map[string]*sync.Mutex{}}
}

// helperBinaryPath locates the bevin-pty helper (the linux pty's
// in-container runner): next to the bevin binary first, then the PATH.
func helperBinaryPath() (string, error) {
	self, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(self), "bevin-pty")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	if p, err := lookPath("bevin-pty"); err == nil {
		return p, nil
	}
	return "", errors.New("the bevin-pty helper binary is missing — build cmd/bevin-pty and ship it next to the bevin binary")
}

// lookPath is exec.LookPath without the import (the sandbox package keeps
// its own small exec surface).
func lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", name)
}

// mapPtyState maps the helper's state-file vocabulary onto the model-facing
// buckets (the helper already speaks the buckets — this is the
// defense-in-depth normalization for a hand-written state file).
func mapPtyState(s string) PtyState {
	switch PtyState(s) {
	case PtyStateRunning, PtyStateWaitingInput, PtyStateWaiting, PtyStateExited:
		return PtyState(s)
	default:
		return PtyStateRunning
	}
}

// readPtyStateFile reads one state file (a missing/corrupt file is an
// error — the session just hasn't written yet, or is being torn down).
func readPtyStateFile(path string) (*ptyStateFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st ptyStateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt pty state file: %w", err)
	}
	return &st, nil
}

// ptyWaitTimeout normalizes a wait timeout (the model's env_pty_wait passes
// its own default/cap through; a bare backend call gets the 10 s default).
func ptyWaitTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 10 * time.Second
	}
	return timeout
}
