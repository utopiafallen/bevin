//go:build windows

package browser

import "os/exec"

// Windows has no process groups: the tree kill is the profile-path sweep
// (killBrowserTree) instead.
func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pgid int) {}
