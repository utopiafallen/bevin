//go:build windows

package main

import "golang.org/x/sys/windows"

// privilegeOK reports whether the bevin process holds an elevated
// (administrator) token — the Host Compute Service (which creates the
// Hyper-V isolated per-user containers) requires it.
func privilegeOK() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer windows.CloseHandle(windows.Handle(token))
	return token.IsElevated()
}

// privilegeMessage is the actionable refusal printed at startup when the
// process is not elevated.
func privilegeMessage() string {
	return `bevin must run as Administrator: the per-user containers (a mandatory core feature) use the Host Compute Service (HCS), which creates the Hyper-V isolated containers and requires an elevated process.

Re-run from an elevated terminal (open PowerShell as Administrator, then: bevin <flags>), or right-click the bevin binary and choose "Run as administrator".

Development/testing only: -allow-unprivileged skips this check; the per-user container features will then be unavailable.`
}
