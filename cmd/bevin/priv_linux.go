//go:build linux

package main

import "os"

// privilegeOK reports whether the bevin process holds the privilege the
// mandatory per-user container runtime requires on linux: root
// (systemd-nspawn manages machines + their networking as root).
func privilegeOK() bool { return os.Geteuid() == 0 }

// privilegeMessage is the actionable refusal printed at startup when the
// process is not privileged.
func privilegeMessage() string {
	return `bevin must run as root: the per-user containers (a mandatory core feature) use systemd-nspawn, and managing the machines and their networking requires root.

Re-run with:   sudo bevin <flags>
(or run bevin as a systemd service — it runs as root and starts at boot.)

Development/testing only: -allow-unprivileged skips this check; the per-user container features will then be unavailable.`
}
