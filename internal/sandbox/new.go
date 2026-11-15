package sandbox

import "context"

// New returns the container backend for the running platform, or
// ErrDisabled when sandboxing is switched off in the config.
//
// The backend is constructed once (at boot / setup) and shared; it is
// safe for concurrent use (each call is independent OS plumbing).
func New(dataDir string, cfg Config) (Backend, error) {
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	return newOS(dataDir, cfg), nil
}

// Check runs the platform runtime check (the first-time-setup gate).
// It works even when sandboxing is disabled (so the wizard can tell
// the user what the runtime state is) — it never touches the user's
// containers.
func Check(ctx context.Context, dataDir string, cfg Config) Runtime {
	if !cfg.Enabled {
		return Runtime{false, "sandboxing is disabled in the server config"}
	}
	b, err := New(dataDir, cfg)
	if err != nil {
		return Runtime{false, err.Error()}
	}
	return b.Runtime(ctx)
}
