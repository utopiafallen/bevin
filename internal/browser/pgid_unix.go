//go:build !windows

package browser

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group (Linux): the
// browser's whole process tree is in the group, so killing the group kills
// the browser and its children in one signal.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the whole group (the browser's children).
func killProcessGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
