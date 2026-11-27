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

func (b *unsupportedBackend) ExecIn(context.Context, string, string, string, time.Duration) (Result, error) {
	return Result{}, ErrDisabled
}

// The PTY methods (unsupported — like the rest of this stub).
func (b *unsupportedBackend) PtyStart(context.Context, string, string, string, int, int) (string, error) {
	return "", ErrDisabled
}
func (b *unsupportedBackend) PtyWait(context.Context, string, string, time.Duration) (PtyWaitResult, error) {
	return PtyWaitResult{}, ErrDisabled
}
func (b *unsupportedBackend) PtyInput(context.Context, string, string, string) error {
	return ErrDisabled
}
func (b *unsupportedBackend) PtyClose(context.Context, string, string) (int, error) {
	return -1, ErrDisabled
}
func (b *unsupportedBackend) PtyCloseAll(context.Context, string) error {
	return ErrDisabled
}

func (b *unsupportedBackend) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, ErrDisabled
}

func (b *unsupportedBackend) WriteFileAtomic(context.Context, string, string, []byte) error {
	return ErrDisabled
}

func (b *unsupportedBackend) ContainerDataDir() string { return "" }

func (b *unsupportedBackend) Status(_ context.Context, _ string) (Info, error) {
	return Info{}, ErrDisabled
}

func (b *unsupportedBackend) Stats(context.Context, string) (Stats, error) {
	return Stats{}, ErrDisabled
}

func (b *unsupportedBackend) Stop(_ context.Context, _ string) (Info, error) {
	return Info{}, ErrDisabled
}

func (b *unsupportedBackend) Restart(context.Context, string) (Info, error) {
	return Info{}, ErrDisabled
}

func (b *unsupportedBackend) Remove(context.Context, string) error { return ErrDisabled }
