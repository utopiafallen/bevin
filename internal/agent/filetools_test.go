package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/sandbox"
	"github.com/utopiafallen/bevin/internal/store"
)

// edit_file (container-scoped): the test matrix — container-absolute paths,
// edit exactness (unique match / near-miss / ambiguous / replace_all), the
// failure RESULTS (no file, container not running, disk-cap frozen, no
// backend), and the P12 metadata refresh (workspace files only).
//
// fakeContainerBackend is an in-memory sandbox.Backend (no real container):
// the file tools are tested against it exactly as they run against the
// platform backends. The exec/ensure recording serves the exec tool's tests
// (auto-ensure ordering, the timeout diagnostic, the failure results).
type fakeContainerBackend struct {
	mu         sync.Mutex
	fs         map[string][]byte
	dataDir    string
	notRunning bool

	ensureCalls int
	ensureErr   error
	execCalls   []fakeExecCall
	execResult  sandbox.Result
	execErr     error

	// The pty layer (the interactive tools' test double).
	ptySessions      map[string]*fakePtySession
	ptySeq           int
	ptyStartCalls    []fakePtyStartCall
	ptyStartErr      error
	ptyInitialOutput string
	// ptyInitialWaiting makes new sessions report waiting_input (the prompt
	// flow); ptyAutoExitAfterInput exits each new session (with
	// ptyExitCode) as soon as the first input arrives (the one-answer
	// command flow).
	ptyInitialWaiting     bool
	ptyAutoExitAfterInput bool
	ptyExitCode           int
	// ptyInputEcho is the output a new session emits when its first input
	// arrives (a prompt echo / the answer's effect).
	ptyInputEcho  string
	ptyInputCalls []fakePtyInputCall
	ptyCloseCalls []string
	ptyCloseCode  int
}

// fakeExecCall is one recorded ExecIn (the exec tool's tests assert the
// auto-ensure ordering and the timeout/cwd plumbing).
type fakeExecCall struct {
	Handle  string
	Command string
	Cwd     string
	Timeout time.Duration
}

func newFakeContainerBackend() *fakeContainerBackend {
	return &fakeContainerBackend{fs: map[string][]byte{}, dataDir: "/data", ptySessions: map[string]*fakePtySession{}}
}

func (f *fakeContainerBackend) Runtime(context.Context) sandbox.Runtime {
	return sandbox.Runtime{OK: true, Detail: "fake"}
}
func (f *fakeContainerBackend) Ensure(_ context.Context, handle string) (sandbox.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	if f.ensureErr != nil {
		return sandbox.Info{}, f.ensureErr
	}
	return sandbox.Info{Handle: handle, State: sandbox.StateRunning}, nil
}
func (f *fakeContainerBackend) Exec(ctx context.Context, handle, command string, timeout time.Duration) (sandbox.Result, error) {
	return f.ExecIn(ctx, handle, command, "", timeout)
}
func (f *fakeContainerBackend) ExecIn(_ context.Context, handle, command, cwd string, timeout time.Duration) (sandbox.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, fakeExecCall{Handle: handle, Command: command, Cwd: cwd, Timeout: timeout})
	if f.execErr != nil {
		return sandbox.Result{}, f.execErr
	}
	return f.execResult, nil
}
func (f *fakeContainerBackend) ReadFile(_ context.Context, _, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notRunning {
		return nil, sandbox.ErrNotRunning
	}
	data, ok := f.fs[p]
	if !ok {
		return nil, fmt.Errorf("%w: %s", sandbox.ErrFileNotFound, p)
	}
	return append([]byte(nil), data...), nil
}
func (f *fakeContainerBackend) WriteFileAtomic(_ context.Context, _, p string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notRunning {
		return sandbox.ErrNotRunning
	}
	f.fs[p] = append([]byte(nil), content...)
	return nil
}
func (f *fakeContainerBackend) ContainerDataDir() string { return f.dataDir }
func (f *fakeContainerBackend) Status(context.Context, string) (sandbox.Info, error) {
	return sandbox.Info{}, nil
}
func (f *fakeContainerBackend) Stats(context.Context, string) (sandbox.Stats, error) {
	return sandbox.Stats{Running: true}, nil
}
func (f *fakeContainerBackend) Stop(context.Context, string) (sandbox.Info, error) {
	return sandbox.Info{}, nil
}
func (f *fakeContainerBackend) Restart(context.Context, string) (sandbox.Info, error) {
	return sandbox.Info{}, nil
}
func (f *fakeContainerBackend) Remove(context.Context, string) error { return nil }

// --- The in-memory pty layer (the interactive tools' test double) ---
//
// fakePtySession is one fake session: the test scripts its output + exit,
// the tool's wait/input/close calls move it exactly as the real backends
// would.
type fakePtySession struct {
	mu       sync.Mutex
	output   string // the full output so far
	deliver  int    // the offset already delivered by PtyWait
	exited   bool
	code     int
	waiting  bool // the wait-state to report while running
	prompt   string
	emitNext string // output emitted when the next input arrives (a prompt echo)
	// exitAfterInput exits the session (with exitCode) when the next input
	// arrives — the scripted "answer the one prompt and it runs off" flow.
	exitAfterInput bool
	exitCode       int
	inputs         []string
}

func (s *fakePtySession) wait() sandbox.PtyWaitResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := sandbox.PtyWaitResult{}
	if s.exited {
		res.State = sandbox.PtyStateExited
		res.ExitCode = s.code
	} else if s.waiting {
		res.State = sandbox.PtyStateWaitingInput
	} else {
		res.State = sandbox.PtyStateRunning
	}
	res.NewOutput = s.output[s.deliver:]
	s.deliver = len(s.output)
	res.TotalBytes = int64(len(s.output))
	res.Prompt = s.prompt
	return res
}
func (s *fakePtySession) input(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return fmt.Errorf("the session has already exited (exit code %d)", s.code)
	}
	s.inputs = append(s.inputs, text)
	if s.emitNext != "" {
		s.output += s.emitNext
		s.emitNext = ""
	}
	if s.exitAfterInput {
		s.exited = true
		s.code = s.exitCode
	}
	return nil
}

type fakePtyStartCall struct{ Command, Cwd string }
type fakePtyInputCall struct{ Session, Text string }

func (f *fakeContainerBackend) PtyStart(_ context.Context, _, command, cwd string, _, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ptyStartCalls = append(f.ptyStartCalls, fakePtyStartCall{Command: command, Cwd: cwd})
	if f.ptyStartErr != nil {
		return "", f.ptyStartErr
	}
	id := fmt.Sprintf("s%d", f.ptySeq)
	f.ptySeq++
	f.ptySessions[id] = &fakePtySession{prompt: "prompt$ ", waiting: f.ptyInitialWaiting}
	if f.ptyInitialOutput != "" {
		f.ptySessions[id].output = f.ptyInitialOutput
	}
	if f.ptyInputEcho != "" {
		f.ptySessions[id].emitNext = f.ptyInputEcho
	}
	if f.ptyAutoExitAfterInput {
		f.ptySessions[id].exitAfterInput = true
		f.ptySessions[id].exitCode = f.ptyExitCode
	}
	return id, nil
}
func (f *fakeContainerBackend) PtyWait(_ context.Context, _, sessionID string, _ time.Duration) (sandbox.PtyWaitResult, error) {
	f.mu.Lock()
	s, ok := f.ptySessions[sessionID]
	f.mu.Unlock()
	if !ok {
		return sandbox.PtyWaitResult{}, fmt.Errorf("unknown pty session %q", sessionID)
	}
	return s.wait(), nil
}
func (f *fakeContainerBackend) PtyInput(_ context.Context, _, sessionID, text string) error {
	f.mu.Lock()
	s, ok := f.ptySessions[sessionID]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown pty session %q", sessionID)
	}
	f.mu.Lock()
	f.ptyInputCalls = append(f.ptyInputCalls, fakePtyInputCall{Session: sessionID, Text: text})
	f.mu.Unlock()
	return s.input(text)
}
func (f *fakeContainerBackend) PtyClose(_ context.Context, _, sessionID string) (int, error) {
	f.mu.Lock()
	s, ok := f.ptySessions[sessionID]
	f.ptyCloseCalls = append(f.ptyCloseCalls, sessionID)
	f.mu.Unlock()
	if !ok {
		return -1, fmt.Errorf("unknown pty session %q", sessionID)
	}
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		s.code = f.ptyCloseCode
	}
	code := s.code
	s.mu.Unlock()
	return code, nil
}
func (f *fakeContainerBackend) PtyCloseAll(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.ptySessions {
		s.mu.Lock()
		if !s.exited {
			s.exited = true
			s.code = f.ptyCloseCode
		}
		s.mu.Unlock()
	}
	return nil
}

func (f *fakeContainerBackend) set(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fs[path] = []byte(content)
}

func (f *fakeContainerBackend) get(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.fs[path])
}

// wireContainerBackend installs a fresh fake backend on the test agent
// (the same wiring the server does at boot, via a live resolver).
func wireContainerBackend(t *testing.T, a *Agent) *fakeContainerBackend {
	t.Helper()
	bk := newFakeContainerBackend()
	a.SetContainerBackend(func() sandbox.Backend { return bk })
	return bk
}

func TestEditFileReplaces(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	bk.set("/data/workspaces/w1/app.go", "func main() {\n\tprintln(\"hello\")\n}\n")

	out, err := a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/workspaces/w1/app.go","old_string":"println(\"hello\")","new_string":"println(\"hi\")"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("out = %s", out)
	}
	if got := bk.get("/data/workspaces/w1/app.go"); !strings.Contains(got, `println("hi")`) {
		t.Fatalf("file = %q", got)
	}
}

func TestEditFileNearMiss(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	bk.set("/data/notes.md", "Hello world\nsecond line\nthird line\n")

	// Casing differs: the hint says so (never a fuzzy apply).
	out, err := a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/notes.md","old_string":"hello world","new_string":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no exact match") || !strings.Contains(out, "case-insensitive") {
		t.Fatalf("out = %s", out)
	}
	if bk.get("/data/notes.md") != "Hello world\nsecond line\nthird line\n" {
		t.Fatal("a near-miss must not modify the file")
	}

	// Whitespace is significant (exact match only).
	out, err = a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/notes.md","old_string":" second line","new_string":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "replaced") {
		t.Fatalf("a whitespace-drifted match must not apply: %s", out)
	}

	// Token-overlap but not an exact match: the closest region of what IS
	// there (the re-anchoring signal).
	out, err = a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/notes.md","old_string":"third line extra","new_string":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no exact match") || !strings.Contains(out, "third line") || strings.Contains(out, "nothing similar") {
		t.Fatalf("out = %s", out)
	}

	// Truly absent (no token overlap): the look-at-the-content advice.
	out, err = a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/notes.md","old_string":"zzz qqq xxx","new_string":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no exact match") || !strings.Contains(out, "nothing similar") {
		t.Fatalf("out = %s", out)
	}
}

func TestEditFileAmbiguous(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	bk.set("/data/list.txt", "one\ntwo\ntwo\ntwo\n")

	// Without replace_all: refused with the count + advice.
	out, err := a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/list.txt","old_string":"two","new_string":"3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3 occurrences") {
		t.Fatalf("out = %s", out)
	}
	if bk.get("/data/list.txt") != "one\ntwo\ntwo\ntwo\n" {
		t.Fatal("an ambiguous edit must not modify the file")
	}

	// With replace_all: every occurrence.
	out, err = a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/list.txt","old_string":"two","new_string":"3","replace_all":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced all 3 occurrences") {
		t.Fatalf("out = %s", out)
	}
	if bk.get("/data/list.txt") != "one\n3\n3\n3\n" {
		t.Fatalf("file = %q", bk.get("/data/list.txt"))
	}

	// Empty new_string deletes the match(es).
	out, err = a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/list.txt","old_string":"3\n","new_string":"","replace_all":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced all 3 occurrences") {
		t.Fatalf("out = %s", out)
	}
	if bk.get("/data/list.txt") != "one\n" {
		t.Fatalf("file = %q", bk.get("/data/list.txt"))
	}
}

func TestEditFileFailureResults(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	ctx := context.Background()
	run := func(args string) string {
		out, err := a.execEditFile(ctx, "u1", []byte(args))
		if err != nil {
			t.Fatalf("execEditFile(%s): %v", args, err)
		}
		return out
	}

	// No file: create it first (edit_file only edits existing files).
	out := run(`{"path":"/data/nope.txt","old_string":"x","new_string":"y"}`)
	if !strings.Contains(out, "no file at") {
		t.Fatalf("out = %s", out)
	}

	// Arg validation: empty old_string, old == new.
	out = run(`{"path":"/data/a.txt","old_string":"  ","new_string":"y"}`)
	if !strings.Contains(out, "non-empty") {
		t.Fatalf("out = %s", out)
	}
	out = run(`{"path":"/data/a.txt","old_string":"x","new_string":"x"}`)
	if !strings.Contains(out, "identical") {
		t.Fatalf("out = %s", out)
	}

	// Container not running: a distinct, actionable result.
	bk.mu.Lock()
	bk.notRunning = true
	bk.mu.Unlock()
	out = run(`{"path":"/data/a.txt","old_string":"x","new_string":"y"}`)
	if !strings.Contains(out, "container isn't running") {
		t.Fatalf("out = %s", out)
	}
	bk.mu.Lock()
	bk.notRunning = false
	bk.mu.Unlock()

	// Disk-cap frozen: the global disk cap pauses file writes.
	a.sched.SetFrozen(func(string) bool { return true })
	out = run(`{"path":"/data/a.txt","old_string":"x","new_string":"y"}`)
	if !strings.Contains(out, "disk cap") {
		t.Fatalf("out = %s", out)
	}
	a.sched.SetFrozen(nil)

	// Relative path: rejected with the container-absolute form.
	out = run(`{"path":"data/a.txt","old_string":"x","new_string":"y"}`)
	if !strings.Contains(out, "absolute") {
		t.Fatalf("out = %s", out)
	}
}

func TestEditFileNoBackend(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// No SetContainerBackend (an unsupported platform): the tool answers
	// "unavailable" instead of failing the turn.
	out, err := a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/a.txt","old_string":"x","new_string":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unavailable") {
		t.Fatalf("out = %s", out)
	}
}

// TestEditFileWorkspaceMetadata: an edit under the mapped data dir's
// workspaces/<id>/ refreshes the P12 row (search/ingest sees it); a non-
// workspace container file (e.g. /root/.bashrc) edits fine but adds no row.
func TestEditFileWorkspaceMetadata(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	us, err := a.userStore("u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := us.CreateWorkspaceWithID("w1", "proj", "", ""); err != nil {
		t.Fatal(err)
	}
	bk.set("/data/workspaces/w1/notes.md", "# notes\n\nold text\n")

	out, err := a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/workspaces/w1/notes.md","old_string":"old text","new_string":"new text"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("out = %s", out)
	}
	files, err := us.ListWorkspaceFiles("w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("P12 rows = %d, want 1: %+v", len(files), files)
	}
	if files[0].RelativePath != "notes.md" || files[0].Source != "agent" || files[0].Size != int64(len("# notes\n\nnew text\n")) {
		t.Fatalf("P12 row = %+v", files[0])
	}
	// Re-edit: the row refreshes (no duplicate).
	out, _ = a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/data/workspaces/w1/notes.md","old_string":"new text","new_string":"newer"}`))
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("out = %s", out)
	}
	files, _ = us.ListWorkspaceFiles("w1")
	if len(files) != 1 {
		t.Fatalf("P12 rows after re-edit = %d, want 1: %+v", len(files), files)
	}

	// A non-workspace container file: edits fine, no P12 row.
	bk.set("/root/.bashrc", "export FOO=old\n")
	out, err = a.execEditFile(context.Background(), "u1", []byte(
		`{"path":"/root/.bashrc","old_string":"FOO=old","new_string":"FOO=new"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced the match") {
		t.Fatalf("out = %s", out)
	}
	files, _ = us.ListWorkspaceFiles("w1")
	if len(files) != 1 {
		t.Fatalf("non-workspace edit added P12 rows: %+v", files)
	}
}

// TestUpsertWorkspaceFileStore is the store-level half of the P12 refresh
// (the agent's refreshWorkspaceFileMeta rides on it).
func TestUpsertWorkspaceFileStore(t *testing.T) {
	dir := t.TempDir()
	us, err := store.OpenUser(dir)
	if err != nil {
		t.Fatalf("OpenUser: %v", err)
	}
	defer us.Close()
	if _, err := us.CreateWorkspace("w", "W", ""); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	ws, err := us.ListWorkspaces()
	if err != nil || len(ws) != 1 {
		t.Fatalf("ListWorkspaces: %+v, %v", ws, err)
	}
	// Insert path.
	if err := us.UpsertWorkspaceFile(ws[0].ID, "a.md", "a.md", 10, "hash1", "agent", "doc"); err != nil {
		t.Fatalf("Upsert insert: %v", err)
	}
	rows, _ := us.ListWorkspaceFiles(ws[0].ID)
	if len(rows) != 1 || rows[0].Size != 10 || rows[0].Hash == nil || *rows[0].Hash != "hash1" {
		t.Fatalf("insert row: %+v", rows)
	}
	// Refresh path: same relative_path → the row updates, no duplicate.
	if err := us.UpsertWorkspaceFile(ws[0].ID, "a.md", "a.md", 25, "hash2", "agent", "doc"); err != nil {
		t.Fatalf("Upsert refresh: %v", err)
	}
	rows, _ = us.ListWorkspaceFiles(ws[0].ID)
	if len(rows) != 1 || rows[0].Size != 25 || *rows[0].Hash != "hash2" {
		t.Fatalf("refresh row: %+v", rows)
	}
}
