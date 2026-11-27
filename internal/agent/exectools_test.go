package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

// exec (the design log's AGENT SANDBOX TOOL section): the matrix — the
// auto-ensure ordering (Ensure before ExecIn), the default/capped timeout,
// the cwd validation, the result format (exit + stdout + stderr; a non-zero
// exit is a RESULT), the 64 KB output tail, and the failure results
// (timeout hint, ensure failure, no backend).

func TestExecTool(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	bk.execResult = sandbox.Result{Stdout: "hello\n", ExitCode: 0}

	out, err := a.execExec(context.Background(), "u1", []byte(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit code: 0") || !strings.Contains(out, "stdout:\nhello") {
		t.Fatalf("out = %s", out)
	}
	// Auto-ensure: exactly one Ensure, before the ExecIn.
	bk.mu.Lock()
	ensures, calls := bk.ensureCalls, len(bk.execCalls)
	var cwd string
	var timeout time.Duration
	if calls == 1 {
		cwd = bk.execCalls[0].Cwd
		timeout = bk.execCalls[0].Timeout
	}
	bk.mu.Unlock()
	if ensures != 1 {
		t.Fatalf("ensure calls = %d, want 1", ensures)
	}
	if calls != 1 {
		t.Fatalf("exec calls = %d, want 1", calls)
	}
	if cwd != "" {
		t.Fatalf("default cwd = %q, want empty (the data dir)", cwd)
	}
	if timeout != 60*time.Second {
		t.Fatalf("default timeout = %s, want 60s", timeout)
	}

	// A non-zero exit is a result, not an error (the model reads the code).
	bk.mu.Lock()
	bk.execResult = sandbox.Result{Stdout: "ok\n", Stderr: "warned\n", ExitCode: 2}
	bk.mu.Unlock()
	out, err = a.execExec(context.Background(), "u1", []byte(`{"command":"false"}`))
	if err != nil {
		t.Fatal("a non-zero exit must not error the call")
	}
	if !strings.Contains(out, "exit code: 2") || !strings.Contains(out, "stderr:\nwarned") {
		t.Fatalf("out = %s", out)
	}

	// No output at all.
	bk.mu.Lock()
	bk.execResult = sandbox.Result{ExitCode: 0}
	bk.mu.Unlock()
	out, _ = a.execExec(context.Background(), "u1", []byte(`{"command":"true"}`))
	if !strings.Contains(out, "(no output)") {
		t.Fatalf("out = %s", out)
	}
}

func TestExecToolTimeoutClamp(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	bk.execResult = sandbox.Result{}

	for _, arg := range []string{`{"command":"x"}`, `{"command":"x","timeout_seconds":99999}`} {
		if _, err := a.execExec(context.Background(), "u1", []byte(arg)); err != nil {
			t.Fatal(err)
		}
	}
	bk.mu.Lock()
	defer bk.mu.Unlock()
	if len(bk.execCalls) != 2 {
		t.Fatalf("exec calls = %d, want 2", len(bk.execCalls))
	}
	if bk.execCalls[0].Timeout != 60*time.Second {
		t.Fatalf("default timeout = %s, want 60s", bk.execCalls[0].Timeout)
	}
	if bk.execCalls[1].Timeout != 300*time.Second {
		t.Fatalf("clamped timeout = %s, want 300s", bk.execCalls[1].Timeout)
	}
}

func TestExecToolCwd(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)

	// Absolute (platform-native): passed through.
	if _, err := a.execExec(context.Background(), "u1", []byte(`{"command":"ls","cwd":"/data/workspaces/w1"}`)); err != nil {
		t.Fatal(err)
	}
	// Relative: rejected (container-absolute paths only).
	out, err := a.execExec(context.Background(), "u1", []byte(`{"command":"ls","cwd":"data/w1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "absolute") {
		t.Fatalf("out = %s", out)
	}
	bk.mu.Lock()
	defer bk.mu.Unlock()
	if len(bk.execCalls) != 1 || bk.execCalls[0].Cwd != "/data/workspaces/w1" {
		t.Fatalf("exec calls = %+v", bk.execCalls)
	}
}

func TestExecToolOutputTail(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)
	// 80 KB of stdout: the result keeps the LAST 64 KB + the notice.
	big := strings.Repeat("x", 80<<10)
	bk.mu.Lock()
	bk.execResult = sandbox.Result{Stdout: big, ExitCode: 0}
	bk.mu.Unlock()

	out, err := a.execExec(context.Background(), "u1", []byte(`{"command":"yes x | head -c 81920"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bytes of earlier output omitted") {
		t.Fatalf("missing the omission notice: %s", out[:200])
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), strings.Repeat("x", 100)) {
		t.Fatal("the tail of the output must be kept")
	}
	if len(out) > execOutputTailBytes+256 { // tail + the notice, nothing more
		t.Fatalf("result too large: %d bytes", len(out))
	}
}

func TestExecToolFailureResults(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	bk := wireContainerBackend(t, a)

	// The timeout: the two-branch hint (input → interactive_exec; long →
	// raise the timeout).
	bk.mu.Lock()
	bk.execErr = fmt.Errorf("exec timed out after %s", "60s")
	bk.mu.Unlock()
	out, err := a.execExec(context.Background(), "u1", []byte(`{"command":"apt install git"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timed out after 60 s") || !strings.Contains(out, "interactive_exec") || !strings.Contains(out, "raise timeout_seconds") {
		t.Fatalf("timeout result = %s", out)
	}

	// The environment could not be started (no exec attempted).
	bk.mu.Lock()
	bk.execErr = nil
	bk.ensureErr = fmt.Errorf("bevin must run as root")
	prevCalls := len(bk.execCalls)
	bk.mu.Unlock()
	out, err = a.execExec(context.Background(), "u1", []byte(`{"command":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "could not be started") || !strings.Contains(out, "bevin must run as root") {
		t.Fatalf("ensure failure = %s", out)
	}
	bk.mu.Lock()
	if len(bk.execCalls) != prevCalls {
		t.Fatalf("exec ran despite the ensure failure: %+v", bk.execCalls[prevCalls:])
	}
	bk.ensureErr = nil
	bk.mu.Unlock()

	// An infrastructure exec failure (not a timeout): the raw detail.
	bk.mu.Lock()
	bk.execErr = fmt.Errorf("exec failed: dial tcp refused (boom)")
	bk.mu.Unlock()
	out, err = a.execExec(context.Background(), "u1", []byte(`{"command":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "could not run") || !strings.Contains(out, "boom") {
		t.Fatalf("infra failure = %s", out)
	}
}

func TestExecToolNoBackend(t *testing.T) {
	a, _, _ := newTestAgent(t)
	defer a.Close()
	// No SetContainerBackend (an unsupported platform): "unavailable".
	out, err := a.execExec(context.Background(), "u1", []byte(`{"command":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unavailable") {
		t.Fatalf("out = %s", out)
	}
}

func TestIsExecTimeout(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("exec timed out after 60s"), true},
		{fmt.Errorf("exec timed out after 5m0s"), true},
		{fmt.Errorf("exec failed: boom"), false},
		{sandbox.ErrNotRunning, false},
	}
	for _, c := range cases {
		if got := sandbox.IsExecTimeout(c.err); got != c.want {
			t.Errorf("IsExecTimeout(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
