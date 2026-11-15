package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// commandFunc executes an OS command and reports its output. The linux
// backend is built around it so tests can substitute a fake (no real
// systemd/nspawn needed).
//
// exitCode is -1 when the process could not be started at all (missing
// binary, context cancelled before exec) and >=0 for a real exit.
type commandFunc func(ctx context.Context, dir, name string, argv ...string) (stdout, stderr []byte, exitCode int, err error)

// osRun is the real implementation.
func osRun(ctx context.Context, dir, name string, argv ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, name, argv...)
	if dir != "" {
		cmd.Dir = dir
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode := -1
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	return outBuf.Bytes(), errBuf.Bytes(), exitCode, err
}
