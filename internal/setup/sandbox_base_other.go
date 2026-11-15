//go:build !linux && !windows

package setup

import (
	"context"
	"fmt"
)

// On platforms with no container backend (darwin, ...), the base image is
// meaningless: sandbox.Check reports the runtime unsupported, the setup
// gate refuses a non-opted-out finalize with that message, and the admin
// proceeds by ticking the skip box (saving [sandbox] disabled).
func sandboxBaseReady(dataDir string) bool { return false }

func provisionSandboxBase(ctx context.Context, dataDir string, progress func(frac float64, detail string)) error {
	return fmt.Errorf("per-user containers are not supported on this platform")
}
