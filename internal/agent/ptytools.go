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

// The pty split (the design log's WS2 + WS3 reshape):
//
//	interactive_exec (the USER agent) — spawns a BACKGROUND SYSTEM TASK
//	whose job is "run <cmd>, interact with its prompts, report the
//	outcome". The user's turn ENDS IMMEDIATELY; the task runs its OWN
//	agent (the same tool loop, on the task's owned conversation,
//	background, through the model's slot pool) and its completion summary
//	lands in the user's conversation when it settles.
//
//	env_pty_start / env_pty_wait / env_pty_input / env_pty_close (the
//	TASK agent) — the pty session surface. A session is a command on a
//	real terminal in the user's environment:
//
//	START  env_pty_start {command, cwd?}  → a session id + the first
//	                                              output (the prompt, if any)
//	READ   env_pty_wait {session, timeout?} → the new output + the state:
//	                                              running | waiting_input |
//	                                              waiting | exited(+code)
//	TYPE   env_pty_input {session, text}   → the model's answer to a prompt
//	END    env_pty_close {session}         → the exit code (cleanup or
//	                                              abandonment)
//
// The contract (from the design): the pty loop inside the user's live
// turn would hold the model slot for minutes with the user watching a
// spinner — wrong for an interactive product. So the user's turn spawns
// the task and ends; the TASK drives the session. The wire filter
// (wireTools) offers interactive_exec only in non-task conversations and
// the env_pty_* tools only in task conversations; a stray direct call
// (the model imitating from history) is refused here too. The session's
// lifetime is bound to the TASK: the task runner's settle path closes
// every session the task conversation started (task end → the sessions
// go — a cancel is exactly the standard task op).

const (
	ptyWaitDefaultSec = 10
	ptyWaitMaxSec     = 120
	// interactiveTitleMax caps the task title (the command line, truncated
	// — the full command lives in the task description).
	interactiveTitleMax = 60
)

// isPtyTool reports whether name is one of the task-agent pty tools (the
// wire filter's pty half).
func isPtyTool(name string) bool {
	switch name {
	case "env_pty_start", "env_pty_wait", "env_pty_input", "env_pty_close":
		return true
	}
	return false
}

func ptyUnavailable() string {
	return "interactive sessions are unavailable on this server (no execution environment)."
}

func ptyUnknownSession(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown pty session")
}

func ptySessionGoneText(sessionID string) string {
	return fmt.Sprintf("no session %q: it is gone (sessions do not survive the environment stopping or a bevin restart). Start a new one with env_pty_start if it is still needed.", sessionID)
}

// ptyConvGuard is the task-conversation gate shared by the env_pty_*
// executors (defense in depth behind the wire filter — a stray call from
// a non-task conversation is refused, structured, model-readable).
func (a *Agent) ptyConvGuard(ctx context.Context, handle string) (string, string, bool) {
	convID := conversationFromContext(ctx)
	if convID == "" {
		return "", "this tool needs a conversation context (it is meant for the task's own conversation).", false
	}
	us, err := a.userStore(handle)
	if err != nil {
		return convID, "no user store: " + err.Error(), false
	}
	conv, err := us.GetConversation(convID)
	if err != nil {
		return convID, "no such conversation: " + err.Error(), false
	}
	if conv.OwnerTaskID == nil || *conv.OwnerTaskID == "" {
		return convID, "this is not a task conversation: the pty session tools (env_pty_*) belong to the task that interactive_exec started. In a user conversation, start the interactive command with interactive_exec instead.", false
	}
	return convID, "", true
}

func registerPtyTools(r *ToolRegistry, a *Agent) error {
	if err := r.Register(Tool{
		Name:        "interactive_exec",
		Description: "Run an INTERACTIVE command in the user's environment: a process that talks back — login/password/OTP prompts, REPLs (python, node, gdb), TUIs, watch loops. One-shot commands use exec instead. This tool spawns a BACKGROUND TASK that runs the command on a real terminal, answers whatever it prompts for, and reports back: the user's turn ends immediately, and the outcome (exit code, what was asked and answered, artifacts) lands in this conversation when the task completes. If the command needs a SECRET the user has not provided, the task reports blocked saying exactly what it needs.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "the command line (the environment's shell interprets it)",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "working directory inside the environment (absolute; default: the mapped data dir)",
				},
			},
			"required": []string{"command"},
		},
		Execute: a.execInteractiveExec,
	}); err != nil {
		return fmt.Errorf("register interactive_exec: %w", err)
	}
	if err := r.Register(Tool{
		Name:        "env_pty_start",
		Description: "Start a pty session: the task's command on a real terminal in the user's environment. Returns a session id + the first output (the prompt, if any). Drive it with env_pty_wait (read), env_pty_input (type), env_pty_close (end). The environment is started automatically if it is not running. Output is captured to a 64 KB tail.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "the command line (the environment's shell interprets it)",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "working directory inside the environment (absolute; default: the mapped data dir)",
				},
			},
			"required": []string{"command"},
		},
		Execute: a.execPtyStart,
	}); err != nil {
		return fmt.Errorf("register env_pty_start: %w", err)
	}
	if err := r.Register(Tool{
		Name:        "env_pty_wait",
		Description: fmt.Sprintf("Wait on a pty session and return its new output + its state. States: running (still active; the total-byte counter shows whether anything happened during the wait — no output yet is normal early on), waiting_input (blocked reading its stdin: send the answer with env_pty_input — the prompt excerpt shows what it is asking), waiting (blocked on something else: wait again), exited (done; the exit code is included). timeout_seconds bounds this call (default %d, max %d); if it returns with nothing new, wait again — the command may still be producing output.", ptyWaitDefaultSec, ptyWaitMaxSec),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "the session id from env_pty_start",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("max wait in seconds (default %d, max %d)", ptyWaitDefaultSec, ptyWaitMaxSec),
				},
			},
			"required": []string{"session"},
		},
		Execute: a.execPtyWait,
	}); err != nil {
		return fmt.Errorf("register env_pty_wait: %w", err)
	}
	if err := r.Register(Tool{
		Name:        "env_pty_input",
		Description: "Send text to a pty session's stdin (the answer to its prompt: a password, a path, a choice — include a trailing newline when the program expects one). Input to an exited session fails with its exit code.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "the session id from env_pty_start",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "the text to send",
				},
			},
			"required": []string{"session", "text"},
		},
		Execute: a.execPtyInput,
	}); err != nil {
		return fmt.Errorf("register env_pty_input: %w", err)
	}
	if err := r.Register(Tool{
		Name:        "env_pty_close",
		Description: "End a pty session (terminate its command) and return its exit code. Call it when the session has exited (to clean it up) or when you are abandoning it. The task's end also closes every session it started.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "the session id from env_pty_start",
				},
			},
			"required": []string{"session"},
		},
		Execute: a.execPtyClose,
	}); err != nil {
		return fmt.Errorf("register env_pty_close: %w", err)
	}
	return nil
}

// --- interactive_exec (the user agent's task spawner) ---

type interactiveExecArgs struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}

func (a *Agent) execInteractiveExec(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ia interactiveExecArgs
	if err := json.Unmarshal(args, &ia); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"command\" is required.", err), nil
	}
	ia.Command = strings.TrimSpace(ia.Command)
	if ia.Command == "" {
		return "interactive_exec needs a non-empty \"command\".", nil
	}
	cwd := strings.TrimSpace(ia.Cwd)
	if cwd != "" {
		p, err := containerPath(cwd)
		if err != nil {
			return err.Error(), nil
		}
		cwd = p
	}
	convID := conversationFromContext(ctx)
	if convID == "" {
		return "interactive_exec needs a conversation context (it is meant to run in the user's own conversation).", nil
	}
	us, err := a.userStore(handle)
	if err != nil {
		return "no user store: " + err.Error(), nil
	}
	if conv, err := us.GetConversation(convID); err != nil {
		return "no such conversation: " + err.Error(), nil
	} else if conv.OwnerTaskID != nil && *conv.OwnerTaskID != "" {
		return "this is a task conversation: a task cannot spawn another task (use the pty session tools directly — they are yours here).", nil
	}
	title := "interactive: " + ia.Command
	if len(title) > len("interactive: ")+interactiveTitleMax {
		title = "interactive: " + title[:len("interactive: ")+interactiveTitleMax-1] + "…"
	}
	task, err := a.SpawnTask(ctx, handle, TaskSpec{
		Title:        title,
		Description:  interactiveTaskMission(ia.Command, cwd),
		OriginConvID: convID,
	})
	if err != nil {
		return "the background task could not be started: " + err.Error(), nil
	}
	return fmt.Sprintf("started task %s to run the command interactively; your turn ends here — the outcome (exit code, what was asked and answered, artifacts) will be reported back to this conversation when the task completes.", task.ID), nil
}

// interactiveTaskMission is the task's unit of work (the framed first
// turn's Task description): the interactive contract the task agent
// follows — start the session, answer what it asks (never guess at
// secrets), report the outcome.
func interactiveTaskMission(command, cwd string) string {
	var b strings.Builder
	b.WriteString("Run this command INTERACTIVELY in the user's environment, on a real terminal:\n\n")
	b.WriteString(command + "\n")
	if cwd != "" {
		b.WriteString("\nWorking directory: " + cwd + "\n")
	}
	b.WriteString(`
How to work it:
- Start it with env_pty_start (the command; the same working directory when given).
- Read it with env_pty_wait. When the state is waiting_input, the prompt excerpt shows what it asks — send the answer with env_pty_input (a trailing newline when the program expects one), then wait again.
- Answer from what the prompt asks and what the command needs. NEVER invent or guess a secret (a password, a token, an OTP): when it asks for one the user has not provided, stop, and report the task blocked with advance_task (state "blocked") saying exactly what it needs — the report lands in the user's conversation and the user can answer it.
- When the command exits, env_pty_wait reports the exit code. Then report the task (advance_task: "done", or "blocked" with a note) — your final reply is the completion summary: the exit code, what the command asked and what you answered, and any artifacts it produced (paths).`)
	return b.String()
}

// --- the pty session tools (the task agent) ---

type ptyStartArgs struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}
type ptyWaitArgs struct {
	Session    string `json:"session"`
	TimeoutSec int    `json:"timeout_seconds"`
}
type ptyInputArgs struct {
	Session string `json:"session"`
	Text    string `json:"text"`
}
type ptyCloseArgs struct {
	Session string `json:"session"`
}

// trackPtySession records the session under the task conversation (the
// task-bound lifetime: the runner's settle path closes them).
func (a *Agent) trackPtySession(convID, sessionID string) {
	a.ptySessMu.Lock()
	defer a.ptySessMu.Unlock()
	set := a.ptySessions[convID]
	if set == nil {
		set = map[string]bool{}
		a.ptySessions[convID] = set
	}
	set[sessionID] = true
}

// untrackPtySession drops one session from the tracker (idempotent).
func (a *Agent) untrackPtySession(convID, sessionID string) {
	a.ptySessMu.Lock()
	defer a.ptySessMu.Unlock()
	if set := a.ptySessions[convID]; set != nil {
		delete(set, sessionID)
		if len(set) == 0 {
			delete(a.ptySessions, convID)
		}
	}
}

// closeTaskPtySessions ends every session the task conversation started
// (the design's WS2 contract: the session's lifetime is bound to the
// task — task end, whatever the outcome, ends the sessions).
// Best-effort: the environment may be gone (a stop removed the sessions
// with the machine — the backend reports them unknown then, and the
// tracker entry is dropped either way).
func (a *Agent) closeTaskPtySessions(handle, convID string) {
	a.ptySessMu.Lock()
	ids := make(map[string]bool, len(a.ptySessions[convID]))
	for id := range a.ptySessions[convID] {
		ids[id] = true
	}
	delete(a.ptySessions, convID)
	a.ptySessMu.Unlock()
	if len(ids) == 0 {
		return
	}
	bk := a.containerBackend()
	if bk == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for id := range ids {
		_, _ = bk.PtyClose(ctx, handle, id)
	}
}

// execPtyStart — env_pty_start.
func (a *Agent) execPtyStart(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var pa ptyStartArgs
	if err := json.Unmarshal(args, &pa); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"command\" is required.", err), nil
	}
	pa.Command = strings.TrimSpace(pa.Command)
	if pa.Command == "" {
		return "env_pty_start needs a non-empty \"command\".", nil
	}
	cwd := strings.TrimSpace(pa.Cwd)
	if cwd != "" {
		p, err := containerPath(cwd)
		if err != nil {
			return err.Error(), nil
		}
		cwd = p
	}
	convID, why, ok := a.ptyConvGuard(ctx, handle)
	if !ok {
		return why, nil
	}
	bk := a.containerBackend()
	if bk == nil {
		return ptyUnavailable(), nil
	}
	// Auto-ensure (the exec contract: the environment starts on first use).
	if _, err := bk.Ensure(ctx, handle); err != nil {
		return "the user's environment could not be started: " + err.Error() + " — report the task blocked (advance_task) saying the environment needs attention.", nil
	}
	id, err := bk.PtyStart(ctx, handle, pa.Command, cwd, 0, 0)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotRunning) {
			return "the user's environment is not running and could not be started: " + err.Error(), nil
		}
		return "the pty session could not start: " + err.Error(), nil
	}
	a.trackPtySession(convID, id)
	// The first read: the prompt often appears immediately (the model sees
	// it without an extra wait call).
	var first string
	if res, werr := bk.PtyWait(ctx, handle, id, 2*time.Second); werr == nil {
		first = renderPtyWait(res)
	}
	return fmt.Sprintf("started pty session %s (command: %s). Read its output with env_pty_wait; send input with env_pty_input; end it with env_pty_close.\n%s",
		id, pa.Command, first), nil
}

// execPtyWait — env_pty_wait.
func (a *Agent) execPtyWait(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var wa ptyWaitArgs
	if err := json.Unmarshal(args, &wa); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"session\" is required.", err), nil
	}
	wa.Session = strings.TrimSpace(wa.Session)
	if wa.Session == "" {
		return "env_pty_wait needs a \"session\" (the id env_pty_start returned).", nil
	}
	if _, why, ok := a.ptyConvGuard(ctx, handle); !ok {
		return why, nil
	}
	timeout := ptyWaitDefaultSec
	if wa.TimeoutSec > 0 {
		timeout = wa.TimeoutSec
	}
	if timeout > ptyWaitMaxSec {
		timeout = ptyWaitMaxSec
	}
	bk := a.containerBackend()
	if bk == nil {
		return ptyUnavailable(), nil
	}
	res, err := bk.PtyWait(ctx, handle, wa.Session, time.Duration(timeout)*time.Second)
	if err != nil {
		if ptyUnknownSession(err) {
			return ptySessionGoneText(wa.Session), nil
		}
		return "the wait failed: " + err.Error(), nil
	}
	return renderPtyWait(res), nil
}

// execPtyInput — env_pty_input.
func (a *Agent) execPtyInput(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ia ptyInputArgs
	if err := json.Unmarshal(args, &ia); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"session\" and \"text\" are required.", err), nil
	}
	ia.Session = strings.TrimSpace(ia.Session)
	if ia.Session == "" {
		return "env_pty_input needs a \"session\" (the id env_pty_start returned).", nil
	}
	if ia.Text == "" {
		return "env_pty_input needs non-empty \"text\" (an empty send is a no-op — send a newline if that is what the prompt expects).", nil
	}
	if _, why, ok := a.ptyConvGuard(ctx, handle); !ok {
		return why, nil
	}
	bk := a.containerBackend()
	if bk == nil {
		return ptyUnavailable(), nil
	}
	if err := bk.PtyInput(ctx, handle, ia.Session, ia.Text); err != nil {
		if ptyUnknownSession(err) {
			return ptySessionGoneText(ia.Session), nil
		}
		return "the input could not be sent: " + err.Error(), nil
	}
	return fmt.Sprintf("sent %d bytes to session %s — read the response with env_pty_wait.", len(ia.Text), ia.Session), nil
}

// execPtyClose — env_pty_close.
func (a *Agent) execPtyClose(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ca ptyCloseArgs
	if err := json.Unmarshal(args, &ca); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"session\" is required.", err), nil
	}
	ca.Session = strings.TrimSpace(ca.Session)
	if ca.Session == "" {
		return "env_pty_close needs a \"session\" (the id env_pty_start returned).", nil
	}
	convID, why, ok := a.ptyConvGuard(ctx, handle)
	if !ok {
		return why, nil
	}
	bk := a.containerBackend()
	if bk == nil {
		return ptyUnavailable(), nil
	}
	code, err := bk.PtyClose(ctx, handle, ca.Session)
	if err != nil {
		if ptyUnknownSession(err) {
			return ptySessionGoneText(ca.Session), nil
		}
		return "the close failed: " + err.Error(), nil
	}
	a.untrackPtySession(convID, ca.Session)
	if code >= 0 {
		return fmt.Sprintf("session %s ended (exit code %d).", ca.Session, code), nil
	}
	return fmt.Sprintf("session %s ended (the exit code is unknown — the command was terminated).", ca.Session), nil
}

// renderPtyWait renders the model-facing wait result: the state (with the
// exit code when exited), the new output (the 64 KB ring delta), the prompt
// excerpt (waiting_input: what it is asking), and the total counter (the
// progress signal — a stalled total on a running state is the "re-check or
// abandon" signal from the design).
func renderPtyWait(res sandbox.PtyWaitResult) string {
	var b strings.Builder
	switch res.State {
	case sandbox.PtyStateExited:
		fmt.Fprintf(&b, "state: exited (exit code %d)", res.ExitCode)
	case sandbox.PtyStateWaitingInput:
		b.WriteString("state: waiting_input")
		if res.Prompt != "" {
			fmt.Fprintf(&b, " — it is asking: %q", res.Prompt)
		}
	case sandbox.PtyStateWaiting:
		b.WriteString("state: waiting (blocked on something else: wait again)")
	default:
		b.WriteString("state: running")
	}
	if res.OmittedBytes > 0 {
		fmt.Fprintf(&b, " (the last 64 KB is shown; %d earlier bytes fell out of the ring — re-run with a narrower command if you need them)", res.OmittedBytes)
	}
	b.WriteString("\n")
	if res.NewOutput == "" {
		fmt.Fprintf(&b, "(no output during the wait — total %d bytes so far)\n", res.TotalBytes)
	} else {
		b.WriteString(res.NewOutput)
		if !strings.HasSuffix(res.NewOutput, "\n") {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "(total %d bytes so far)\n", res.TotalBytes)
	}
	return b.String()
}
