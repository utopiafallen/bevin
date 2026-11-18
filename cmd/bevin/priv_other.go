//go:build !linux && !windows

package main

// privilegeOK: on platforms without a supported container backend (darwin)
// the server can run unprivileged — the per-user container features are
// unavailable there, and the sandbox API reports the platform unsupported.
func privilegeOK() bool { return true }

// privilegeMessage is unused on these platforms (privilegeOK never fails).
func privilegeMessage() string { return "" }
