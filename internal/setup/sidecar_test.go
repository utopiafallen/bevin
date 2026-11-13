package setup

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// TestPickFreePort verifies pickFreePort returns a bindable loopback port and
// moves past a busy one.
func TestPickFreePort(t *testing.T) {
	// Occupy a port so pickFreePort has to move past it.
	busyL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busyL.Close()
	busy := busyL.Addr().(*net.TCPAddr).Port

	p, err := pickFreePort(busy)
	if err != nil {
		t.Fatal(err)
	}
	if p == busy {
		t.Fatalf("pickFreePort(%d) returned the busy port", busy)
	}
	if l2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err != nil {
		t.Fatalf("returned port %d not bindable: %v", p, err)
	} else {
		l2.Close()
	}

	// A free start port is preferred (no drift).
	freeL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := freeL.Addr().(*net.TCPAddr).Port
	freeL.Close()
	got, err := pickFreePort(free)
	if err != nil {
		t.Fatal(err)
	}
	if got != free {
		t.Fatalf("pickFreePort(%d) = %d, want the free port preferred", free, got)
	}
}

// TestLoopbackPort verifies the port is extracted from loopback endpoints only.
func TestLoopbackPort(t *testing.T) {
	cases := []struct {
		endpoint string
		want     int
	}{
		{"http://127.0.0.1:8081", 8081},
		{"http://127.0.0.1:8081/v1", 8081},
		{"http://localhost:6333", 6333},
		{"http://[::1]:8081", 8081},
		{"http://127.0.0.1", 0},          // no port
		{"http://example.com:8081", 0},   // not loopback
		{"http://remote:1234/v1", 0},     // not loopback
		{"", 0},
	}
	for _, c := range cases {
		if got := loopbackPort(c.endpoint); got != c.want {
			t.Errorf("loopbackPort(%q) = %d, want %d", c.endpoint, got, c.want)
		}
	}
}

// TestEnsurePrefersRecordedPort verifies a down sidecar is (re-)installed with
// its recorded loopback port (so a re-launch stays on its port, no drift).
func TestEnsurePrefersRecordedPort(t *testing.T) {
	fs := &fakeSidecars{} // probeInstalled=false → probe fails → install runs
	var gotPort int
	sm := NewSidecarManager(fs.probe, func(ctx context.Context, dataDir, kind string, preferredPort int) (string, error) {
		gotPort = preferredPort
		return "http://127.0.0.1:8123", nil
	})
	if _, err := sm.Ensure(context.Background(), t.TempDir(), KindLlamaServer, "http://127.0.0.1:8123"); err != nil {
		t.Fatal(err)
	}
	if gotPort != 8123 {
		t.Fatalf("install preferred port = %d, want the recorded port 8123", gotPort)
	}
}

// TestEnsurePrefersZeroForRemoteEndpoint verifies a non-loopback (remote)
// endpoint installs with no preferred port (the install uses its default).
func TestEnsurePrefersZeroForRemoteEndpoint(t *testing.T) {
	fs := &fakeSidecars{}
	var gotPort int
	sm := NewSidecarManager(fs.probe, func(ctx context.Context, dataDir, kind string, preferredPort int) (string, error) {
		gotPort = preferredPort
		return "http://127.0.0.1:8081", nil
	})
	if _, err := sm.Ensure(context.Background(), t.TempDir(), KindLlamaServer, "http://remote.lan:9000"); err != nil {
		t.Fatal(err)
	}
	if gotPort != 0 {
		t.Fatalf("install preferred port = %d, want 0 for a remote endpoint", gotPort)
	}
}

// TestSuperviseRespawnsAndStops verifies the supervisor (re-)provisions a down
// sidecar at boot and stops cleanly when the context is canceled.
func TestSuperviseRespawnsAndStops(t *testing.T) {
	old := superviseInterval
	superviseInterval = 20 * time.Millisecond
	defer func() { superviseInterval = old }()

	// The probe always reports "down" so every Ensure re-installs; the install
	// signals once via a channel (no shared counter → no race).
	installed := make(chan struct{}, 1)
	sm := NewSidecarManager(
		func(ctx context.Context, dataDir, kind, endpoint string) (bool, error) { return false, nil },
		func(ctx context.Context, dataDir, kind string, preferredPort int) (string, error) {
			select {
			case installed <- struct{}{}:
			default:
			}
			return "http://127.0.0.1:1", nil
		},
	)
	// A configured (recorded) sidecar: the supervisor must (re-)provision it.
	cfg := &config.ServerConfig{
		DataDir:  t.TempDir(),
		Sidecars: config.Sidecars{LlamaServer: "http://127.0.0.1:8081"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sm.Supervise(ctx, cfg)
		close(done)
	}()
	select {
	case <-installed:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor should have (re-)provisioned a down sidecar")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor should stop when the context is canceled")
	}
}

// TestSuperviseSkipsUnconfiguredSidecars verifies the supervisor NEVER installs
// a sidecar that setup never provisioned (no recorded endpoint) — a sidecar-
// free config stays sidecar-free for the life of the process.
func TestSuperviseSkipsUnconfiguredSidecars(t *testing.T) {
	old := superviseInterval
	superviseInterval = 10 * time.Millisecond
	defer func() { superviseInterval = old }()

	var installs int32
	sm := NewSidecarManager(
		func(ctx context.Context, dataDir, kind, endpoint string) (bool, error) { return false, nil },
		func(ctx context.Context, dataDir, kind string, preferredPort int) (string, error) {
			atomic.AddInt32(&installs, 1)
			return "http://127.0.0.1:1", nil
		},
	)
	cfg := &config.ServerConfig{DataDir: t.TempDir()} // no sidecar endpoints
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sm.Supervise(ctx, cfg)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond) // several supervise ticks
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor should stop when the context is canceled")
	}
	if got := atomic.LoadInt32(&installs); got != 0 {
		t.Fatalf("supervisor installed %d sidecars for an unconfigured config; want 0", got)
	}
}
