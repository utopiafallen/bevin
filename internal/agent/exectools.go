package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

// exec (the design log's AGENT SANDBOX TOOL section): the blocking one-shot
// command in the user's persistent per-user environment. The environment's
// shell interprets the command (arbitrary code is the point — bevin does
// not sanitize it); the environment is AUTO-ENSURED on first use (isolation
// + persistence come from the runtime). A non-zero exit is a RESULT, not an
// error; everything that errors is infrastructure (no backend, the
// environment could not be started, a timeout) and gets a structured
// model-facing result with the recovery path:
//
//	timeout → the two-branch hint (it was waiting on input → re-run via
//	interactive_exec; it was genuinely long → raise timeout_seconds).
//
// Output budget: the LAST 64 KB of the combined result + an "N bytes of
// earlier output omitted" notice — exec output is re-derivable (the model
// re-runs with a narrower command or head/tail), unlike a web page read.

const (
	execDefaultTimeoutSec = 60
	execMaxTimeoutSec     = 300
	execOutputTailBytes   = 64 << 10
)

type execArgs struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func registerExecTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "exec",
		Description: "Run a NON-INTERACTIVE command in the user's persistent environment (a full OS: Ubuntu 26.04 LTS on linux, Windows Server 2025 LTSC on windows — install what you need; it persists across restarts). Pass non-interactive flags where they exist (apt: -y; DEBIAN_FRONTEND=noninteractive); if the command may prompt, use interactive_exec instead. Returns the exit code, stdout and stderr — a non-zero exit is a result, not an error.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "the command line (the environment's shell interprets it)",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "working directory inside the environment (absolute: /... on linux, C:\\... on windows; default: the mapped data dir)",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("max run time in seconds (default %d, max %d)", execDefaultTimeoutSec, execMaxTimeoutSec),
				},
			},
			"required": []string{"command"},
		},
		Execute: a.execExec,
	})
}

// execExec — exec.
func (a *Agent) execExec(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ea execArgs
	if err := json.Unmarshal(args, &ea); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"command\" is required.", err), nil
	}
	ea.Command = strings.TrimSpace(ea.Command)
	if ea.Command == "" {
		return "exec needs a non-empty \"command\".", nil
	}
	cwd := strings.TrimSpace(ea.Cwd)
	if cwd != "" {
		p, err := containerPath(cwd)
		if err != nil {
			return err.Error(), nil
		}
		cwd = p
	}
	timeout := execDefaultTimeoutSec
	if ea.TimeoutSeconds > 0 {
		timeout = ea.TimeoutSeconds
	}
	if timeout > execMaxTimeoutSec {
		timeout = execMaxTimeoutSec
	}
	bk := a.containerBackend()
	if bk == nil {
		return "command execution is unavailable on this server (no execution environment).", nil
	}
	// Auto-ensure on first use: the target is the user's OWN persistent
	// per-user environment (the scoping that makes it "the user's
	// environment" — isolation + persistence from the runtime).
	if _, err := bk.Ensure(ctx, handle); err != nil {
		return "the user's environment could not be started: " + err.Error() + " — tell the user their environment needs attention.", nil
	}
	res, err := bk.ExecIn(ctx, handle, ea.Command, cwd, time.Duration(timeout)*time.Second)
	if err != nil {
		switch {
		case sandbox.IsExecTimeout(err):
			return fmt.Sprintf("the command timed out after %d s. If it was waiting for interactive input (a prompt or a confirmation), run it via interactive_exec instead; if it was genuinely long, raise timeout_seconds (max %d).", timeout, execMaxTimeoutSec), nil
		case errors.Is(err, sandbox.ErrNotRunning):
			return "the user's environment is not running and could not be started: " + err.Error(), nil
		default:
			return "the command could not run in the user's environment: " + err.Error(), nil
		}
	}
	return formatExecResult(res), nil
}

// formatExecResult renders the model-facing exec result: the exit code +
// stdout + stderr, with the combined text capped at the last 64 KB (the
// output budget — re-derivable, so the tail + the omitted count is the
// contract).
func formatExecResult(res sandbox.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit code: %d\n", res.ExitCode)
	switch {
	case res.Stdout == "" && res.Stderr == "":
		b.WriteString("(no output)\n")
	default:
		if res.Stdout != "" {
			b.WriteString("stdout:\n")
			b.WriteString(res.Stdout)
			if !strings.HasSuffix(res.Stdout, "\n") {
				b.WriteString("\n")
			}
		}
		if res.Stderr != "" {
			if res.Stdout != "" {
				b.WriteString("\n")
			}
			b.WriteString("stderr:\n")
			b.WriteString(res.Stderr)
			if !strings.HasSuffix(res.Stderr, "\n") {
				b.WriteString("\n")
			}
		}
	}
	out := b.String()
	if len(out) > execOutputTailBytes {
		omitted := len(out) - execOutputTailBytes
		out = fmt.Sprintf("[%d bytes of earlier output omitted — re-run with a narrower command or head/tail]\n%s",
			omitted, out[len(out)-execOutputTailBytes:])
	}
	return out
}
