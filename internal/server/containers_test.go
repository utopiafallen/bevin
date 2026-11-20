package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/sandbox"
)

// fakeSandbox is a sandbox.Backend for the admin Containers tests: it reports
// the state + usage the test sets, records Restart/Stop calls, and is
// platform-agnostic (no real containers).
type fakeSandbox struct {
	mu        sync.Mutex
	running   map[string]bool
	cpuSec    float64
	memBytes  uint64
	restarted []string
	stopped   []string
}

func (f *fakeSandbox) Runtime(context.Context) sandbox.Runtime {
	return sandbox.Runtime{OK: true, Detail: "fake runtime"}
}
func (f *fakeSandbox) Ensure(context.Context, string) (sandbox.Info, error) {
	return sandbox.Info{}, nil
}
func (f *fakeSandbox) Exec(context.Context, string, string, time.Duration) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (f *fakeSandbox) ExecIn(context.Context, string, string, string, time.Duration) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (f *fakeSandbox) ContainerDataDir() string { return "/data" }
func (f *fakeSandbox) Status(_ context.Context, handle string) (sandbox.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[handle] {
		return sandbox.Info{Handle: handle, State: sandbox.StateRunning}, nil
	}
	return sandbox.Info{Handle: handle, State: sandbox.StateStopped}, nil
}
func (f *fakeSandbox) Stats(_ context.Context, handle string) (sandbox.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[handle] {
		return sandbox.Stats{}, nil
	}
	return sandbox.Stats{Running: true, CPUSec: f.cpuSec, MemoryBytes: f.memBytes}, nil
}
func (f *fakeSandbox) Stop(_ context.Context, handle string) (sandbox.Info, error) {
	f.mu.Lock()
	f.stopped = append(f.stopped, handle)
	f.mu.Unlock()
	return sandbox.Info{Handle: handle, State: sandbox.StateStopped}, nil
}
func (f *fakeSandbox) Restart(_ context.Context, handle string) (sandbox.Info, error) {
	f.mu.Lock()
	f.restarted = append(f.restarted, handle)
	f.mu.Unlock()
	return sandbox.Info{Handle: handle, State: sandbox.StateRunning}, nil
}
func (f *fakeSandbox) Remove(context.Context, string) error { return nil }

func (f *fakeSandbox) wasRestarted(handle string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.restarted {
		if h == handle {
			return true
		}
	}
	return false
}

// TestAdminContainersEndpoint — GET /admin/api/containers: admin-gated, the
// per-environment health (state + CPU/RAM usage from the backend, disk from
// the monitor — 0 here, no monitor in the test) + the current limits.
func TestAdminContainersEndpoint(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	fake := &fakeSandbox{running: map[string]bool{"alice": true}, cpuSec: 10.5, memBytes: 512 * 1024 * 1024}
	a.sandboxBk = fake
	a.cfg.Sandbox = config.Sandbox{Enabled: true, Cores: 2, MemoryMB: 2048, DiskMB: 10240}
	if _, err := srv.CreateUser("alice", "Alice", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.CreateUser("bob", "Bob", "user"); err != nil {
		t.Fatal(err)
	}

	// No session -> 403 (the admin gate).
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/containers", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated code = %d, want 403", w.Code)
	}

	// Admin -> the health + limits.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/containers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		RuntimeOK  bool `json:"runtime_ok"`
		Limits     struct {
			Cores    int `json:"cores"`
			MemoryMB int `json:"memory_mb"`
			DiskMB   int `json:"disk_mb"`
		} `json:"limits"`
		Containers []struct {
			Handle    string  `json:"handle"`
			State     string  `json:"state"`
			Running   bool    `json:"running"`
			CPUSec    float64 `json:"cpu_seconds"`
			RAMMB     uint64  `json:"ram_mb"`
			DiskMB    uint64  `json:"disk_mb"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.RuntimeOK {
		t.Fatalf("runtime_ok = false, want true")
	}
	if got.Limits.Cores != 2 || got.Limits.MemoryMB != 2048 || got.Limits.DiskMB != 10240 {
		t.Fatalf("limits = %+v, want {2 2048 10240}", got.Limits)
	}
	if len(got.Containers) < 2 {
		t.Fatalf("containers = %+v, want at least alice + bob", got.Containers)
	}
	var alice, bob *struct {
		Handle  string  `json:"handle"`
		Running bool    `json:"running"`
		CPUSec  float64 `json:"cpu_seconds"`
		RAMMB   uint64  `json:"ram_mb"`
	}
	for i := range got.Containers {
		c := &got.Containers[i]
		switch c.Handle {
		case "alice":
			alice = &struct {
				Handle  string  `json:"handle"`
				Running bool    `json:"running"`
				CPUSec  float64 `json:"cpu_seconds"`
				RAMMB   uint64  `json:"ram_mb"`
			}{c.Handle, c.Running, c.CPUSec, c.RAMMB}
		case "bob":
			bob = &struct {
				Handle  string  `json:"handle"`
				Running bool    `json:"running"`
				CPUSec  float64 `json:"cpu_seconds"`
				RAMMB   uint64  `json:"ram_mb"`
			}{c.Handle, c.Running, c.CPUSec, c.RAMMB}
		}
	}
	if alice == nil || bob == nil {
		t.Fatalf("missing alice or bob: %+v", got.Containers)
	}
	if !alice.Running || alice.CPUSec != 10.5 || alice.RAMMB != 512 {
		t.Fatalf("alice = %+v, want running + 10.5s CPU + 512 MiB RAM", alice)
	}
	if bob.Running {
		t.Fatalf("bob = %+v, want not running (zero usage)", bob)
	}
}

// TestAdminContainersRestartEndpoint — POST /admin/api/containers/restart:
// "all" bounces every running environment, a specific handle bounces only it.
func TestAdminContainersRestartEndpoint(t *testing.T) {
	a, srv := testApp(t, config.Email{})
	fake := &fakeSandbox{running: map[string]bool{"alice": true, "bob": true}}
	a.sandboxBk = fake
	for _, h := range []string{"alice", "bob"} {
		if _, err := srv.CreateUser(h, h, "user"); err != nil {
			t.Fatal(err)
		}
	}

	// A specific handle: only that one is restarted.
	fake.mu.Lock()
	fake.restarted = nil
	fake.mu.Unlock()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/containers/restart", map[string]any{"handle": "alice"}))
	if w.Code != http.StatusOK {
		t.Fatalf("specific restart code = %d body=%s", w.Code, w.Body.String())
	}
	if !fake.wasRestarted("alice") {
		t.Fatal("alice was not restarted")
	}
	if fake.wasRestarted("bob") {
		t.Fatal("bob must not be restarted on an alice-specific restart")
	}

	// All: every running environment is restarted.
	fake.mu.Lock()
	fake.restarted = nil
	fake.mu.Unlock()
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/containers/restart", map[string]any{}))
	if w.Code != http.StatusOK {
		t.Fatalf("restart-all code = %d body=%s", w.Code, w.Body.String())
	}
	if !fake.wasRestarted("alice") || !fake.wasRestarted("bob") {
		t.Fatalf("restart-all: alice=%v bob=%v, want both", fake.wasRestarted("alice"), fake.wasRestarted("bob"))
	}
}

// TestAdminConfigSaveRestartsOnLimitChange — saving the sandbox area restarts
// the running environments ONLY when the CPU/RAM limits changed (the disk cap
// is live — a disk-only change does not bounce anything). The response reports
// the restarted handles.
func TestAdminConfigSaveRestartsOnLimitChange(t *testing.T) {
	a, _ := configApp(t)
	fake := &fakeSandbox{running: map[string]bool{"alice": true}}
	a.sandboxBk = fake
	// The save flow rebuilds the backend (buildSandbox), so a directly-set
	// a.sandboxBk would be clobbered before the restart runs — inject the
	// fake through the factory hook instead.
	newSandboxBackend = func(string, sandbox.Config) (sandbox.Backend, error) { return fake, nil }
	t.Cleanup(func() { newSandboxBackend = sandbox.New })
	if _, err := a.srv.CreateUser("alice", "Alice", "user"); err != nil {
		t.Fatal(err)
	}

	// A CPU/RAM change (0 -> 4 cores, 0 -> 4096 MiB) restarts the running one.
	fake.mu.Lock()
	fake.restarted = nil
	fake.mu.Unlock()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"sandbox": map[string]any{"cores": 4, "memory_mb": 4096, "disk_mb": 0},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("save code = %d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		OK        bool     `json:"ok"`
		Reloaded  bool     `json:"reloaded"`
		Restarted []string `json:"restarted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Reloaded {
		t.Fatalf("save result = %+v, want ok+reloaded", res)
	}
	if len(res.Restarted) != 1 || res.Restarted[0] != "alice" {
		t.Fatalf("restarted = %v, want [alice] (the running environment)", res.Restarted)
	}
	if !fake.wasRestarted("alice") {
		t.Fatal("the backend Restart was not called for the limit change")
	}

	// A disk-only change (4 -> 4 cores, 4096 -> 4096 MiB, disk 0 -> 5120)
	// does NOT restart (the disk cap is live).
	fake.mu.Lock()
	fake.restarted = nil
	fake.mu.Unlock()
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"sandbox": map[string]any{"cores": 4, "memory_mb": 4096, "disk_mb": 5120},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("disk-only save code = %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Restarted) != 0 {
		t.Fatalf("disk-only change restarted %v, want none (the disk cap is live)", res.Restarted)
	}
	if fake.wasRestarted("alice") {
		t.Fatal("a disk-only change must not restart the environment")
	}
}
