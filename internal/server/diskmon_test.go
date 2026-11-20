package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/sandbox"
	"github.com/utopiafallen/bevin/internal/store"
)

// writeData clears dir's files and writes exactly one file of n bytes, so the
// user's data-dir size is exactly n (the DiskMonitor counts regular files).
func writeData(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiskMonitorHysteresis exercises the freeze/release hysteresis: a user
// freezes the moment usage crosses the cap, STAYS frozen while usage is in the
// band [90%, cap) (so a user hovering at the cap doesn't flap), and is released
// (with the onUnfreeze hook fired) once usage drops back below 90%.
func TestDiskMonitorHysteresis(t *testing.T) {
	dir := t.TempDir()
	const capMiB = 1 // a 1 MiB cap keeps the temp files small
	m := newDiskMonitor(dir, func() int { return capMiB }, func() ([]string, bool) { return []string{"alice"}, true })
	var unfroze []string
	m.SetOnUnfreeze(func(h string) { unfroze = append(unfroze, h) })

	data := sandbox.UserDataDir(dir, "alice")
	// Under the cap → not frozen.
	writeData(t, data, 950_000)
	m.scan()
	if m.Frozen("alice") {
		t.Fatal("950 KB under a 1 MiB cap must not freeze")
	}
	// Cross the cap → frozen.
	writeData(t, data, 1_100_000)
	m.scan()
	if !m.Frozen("alice") {
		t.Fatal("1.1 MB over a 1 MiB cap must freeze")
	}
	// In the hysteresis band (950 KB is above 90% of 1 MiB = 943 KB) → still frozen.
	writeData(t, data, 950_000)
	m.scan()
	if !m.Frozen("alice") {
		t.Fatal("950 KB (between 90% and 100%) must stay frozen (hysteresis)")
	}
	// Back under 90% → released, and the onUnfreeze hook fires exactly once.
	writeData(t, data, 900_000)
	m.scan()
	if m.Frozen("alice") {
		t.Fatal("900 KB (under 90% of the cap) must release")
	}
	if len(unfroze) != 1 || unfroze[0] != "alice" {
		t.Fatalf("onUnfreeze = %v, want exactly [alice]", unfroze)
	}
}

// TestDiskMonitorBaseline verifies the disk cap counts the GROWTH over the
// creation-time container baseline, not the container storage in full — the
// base OS image (captured as base_bytes at creation) does not count, only what
// the user added after.
func TestDiskMonitorBaseline(t *testing.T) {
	dir := t.TempDir()
	m := newDiskMonitor(dir, func() int { return 100 }, func() ([]string, bool) { return []string{"alice"}, true })
	sb := sandbox.UserSandboxDir(dir, "alice")
	if err := os.MkdirAll(sb, 0o755); err != nil {
		t.Fatal(err)
	}
	// 3 MiB base image (the baseline) + 5 MiB the user added.
	if err := os.WriteFile(filepath.Join(sb, "base"), make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb, "user"), make([]byte, 5<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	// state.json records the 3 MiB baseline (what the backend's Ensure writes).
	if err := os.WriteFile(filepath.Join(sb, "state.json"), []byte(fmt.Sprintf(`{"base_bytes":%d}`, 3<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	st := m.Recheck("alice")
	// Usage is ~5 MiB (the growth), NOT ~8 MiB (the full storage). If the base
	// image were counted it would land near 8 MiB, far above the window below.
	if st.Used < 5<<20 || st.Used >= 6<<20 {
		t.Fatalf("usage = %d, want ~5 MiB (growth over the 3 MiB baseline); the base image must not count", st.Used)
	}
}

// TestDiskGateBlocksFrozen checks the gate: a frozen user gets a 413 with the
// disk_exceeded body and the inner handler never runs; a non-frozen or
// anonymous user passes through untouched.
func TestDiskGateBlocksFrozen(t *testing.T) {
	dir := t.TempDir()
	m := newDiskMonitor(dir, func() int { return 1 }, func() ([]string, bool) { return []string{"alice"}, true })
	// Freeze alice: 2 MiB of data under a 1 MiB cap.
	writeData(t, sandbox.UserDataDir(dir, "alice"), 2<<20)
	m.Prime()
	if !m.Frozen("alice") {
		t.Fatal("setup: alice should be frozen (2 MiB under a 1 MiB cap)")
	}

	a := &App{diskMon: m}
	innerRan := false
	handler := a.diskGate(func(w http.ResponseWriter, r *http.Request) {
		innerRan = true
		w.WriteHeader(http.StatusOK)
	})

	// Frozen user → 413 + disk_exceeded, inner handler NOT called.
	r := httptest.NewRequest("POST", "/api/conversations", nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &store.User{Handle: "alice"}))
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("frozen: status = %d, want 413", w.Code)
	}
	if innerRan {
		t.Fatal("frozen: the inner handler must not run")
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("frozen: body not JSON: %v", err)
	}
	if body["disk_exceeded"] != true {
		t.Fatalf("frozen: body = %v, want disk_exceeded=true", body)
	}

	// Non-frozen user → passes through, inner handler runs.
	innerRan = false
	r2 := httptest.NewRequest("POST", "/api/conversations", nil)
	r2 = r2.WithContext(auth.ContextWithUser(r2.Context(), &store.User{Handle: "bob"}))
	w2 := httptest.NewRecorder()
	handler(w2, r2)
	if w2.Code != http.StatusOK || !innerRan {
		t.Fatalf("non-frozen: status=%d innerRan=%v, want 200 + inner ran", w2.Code, innerRan)
	}

	// Anonymous (no user in context) → passes through (the gate only blocks a
	// known-frozen user; auth is enforced by the outer chatGate).
	innerRan = false
	r3 := httptest.NewRequest("POST", "/api/conversations", nil)
	w3 := httptest.NewRecorder()
	handler(w3, r3)
	if w3.Code != http.StatusOK || !innerRan {
		t.Fatalf("anonymous: status=%d innerRan=%v, want 200 + inner ran", w3.Code, innerRan)
	}
}
