//go:build !linux && !windows

package setup

import (
	"context"
	"fmt"
)

// On platforms with no container backend (darwin, ...), the base image is
// meaningless: sandbox.Check reports the runtime unsupported and the
// mandatory setup gate refuses finalize with that message — bevin targets
// Linux + Windows, where the container runtime is a requirement.
func sandboxBaseReady(dataDir string) bool { return false }

func provisionSandboxBase(ctx context.Context, dataDir string, progress func(frac float64, detail string)) error {
	return fmt.Errorf("per-user containers are not supported on this platform")
}
