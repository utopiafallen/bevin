//go:build !windows && !linux

package sandbox

import (
	"context"
	"time"
)

// unsupported — darwin and friends are not container targets (the
// design scope is linux + windows). The setup wizard's runtime check
// reports this as "not supported on this platform", and the sandbox
// API 503s rather than 500s.
type unsupportedBackend struct{ reason string }

func (b *unsupportedBackend) Runtime(_ context.Context) Runtime {
	return Runtime{false, b.reason}
}

func (b *unsupportedBackend) Ensure(context.Context, string) (Info, error) {
	return Info{}, ErrDisabled
}

func (b *unsupportedBackend) Exec(context.Context, string, string, time.Duration) (Result, error) {
	return Result{}, ErrDisabled
}

func (b *unsupportedBackend) Status(_ context.Context, _ string) (Info, error) {
	return Info{}, ErrDisabled
}

func (b *unsupportedBackend) Stop(_ context.Context, _ string) (Info, error) {
	return Info{}, ErrDisabled
}

func (b *unsupportedBackend) Remove(context.Context, string) error { return ErrDisabled }
