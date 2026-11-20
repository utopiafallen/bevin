// The admin Containers page (admin.html #containers): the live per-environment
// health dashboard (state + CPU/RAM/disk usage) + the per-container resource
// caps + the restart-to-apply action. The container config (the [sandbox]
// section) moved here from the Server page — this page owns everything
// container-related for the admin.
package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

// containerRow is one user's environment for the health dashboard.
type containerRow struct {
	Handle  string  `json:"handle"`
	State   string  `json:"state"`
	Running bool    `json:"running"`
	CPUSec  float64 `json:"cpu_seconds"` // cumulative CPU-seconds since start
	RAMMB   uint64  `json:"ram_mb"`      // current resident memory
	DiskMB  uint64  `json:"disk_mb"`     // the user's disk footprint
}

// adminContainers is the live container health for the Containers page:
// every user's environment (its state + current CPU/RAM usage from the
// runtime + disk usage from the disk monitor) + the current per-container
// limits. The CPU is cumulative seconds since the environment started (the
// page computes an instantaneous % across its own polls); RAM is current
// resident memory; disk is the user's footprint (data dir + storage growth
// over the baseline — the same number the disk cap enforces).
func (a *App) adminContainers(w http.ResponseWriter, r *http.Request) {
	limit := a.liveCfg().Sandbox
	resp := map[string]any{
		"runtime_ok": false,
		"limits": map[string]any{
			"cores":     limit.Cores,
			"memory_mb": limit.MemoryMB,
			"disk_mb":   limit.DiskMB,
		},
		"containers": []containerRow{},
	}
	bk := a.sandboxBackend()
	if bk == nil {
		// No container backend (unsupported platform): the limits are still
		// editable (they apply on the next supported server); the dashboard
		// is empty + the page notes the runtime is unavailable.
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["runtime_ok"] = bk.Runtime(r.Context()).OK
	handles, ok := a.userHandles()
	if !ok {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	rows := make([]containerRow, 0, len(handles))
	for _, h := range handles {
		info, err := bk.Status(r.Context(), h)
		if err != nil {
			continue
		}
		row := containerRow{Handle: h, State: string(info.State), Running: info.State == sandbox.StateRunning}
		if row.Running {
			if st, err := bk.Stats(r.Context(), h); err == nil {
				row.CPUSec = st.CPUSec
				row.RAMMB = st.MemoryBytes / (1024 * 1024)
			}
		}
		if a.diskMon != nil {
			row.DiskMB = uint64(a.diskMon.Status(h).Used) / (1024 * 1024)
		}
		rows = append(rows, row)
	}
	resp["containers"] = rows
	writeJSON(w, http.StatusOK, resp)
}

// adminContainersRestart restarts environments to apply the backend's current
// limits (or to bounce a stuck one). Body: {"handle": "<handle>"} for one, or
// {} for every running environment. Each restarted environment's user is
// emailed that their environment was restarted (in-flight work was
// interrupted — please check it).
func (a *App) adminContainersRestart(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Handle string `json:"handle"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&p)
	bk := a.sandboxBackend()
	if bk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "container environments are not available on this server"})
		return
	}
	if p.Handle != "" {
		// One specific environment.
		info, err := bk.Status(r.Context(), p.Handle)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if info.State != sandbox.StateRunning {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "that environment is not running"})
			return
		}
		if _, err := bk.Restart(r.Context(), p.Handle); err != nil {
			log.Printf("bevin: container restart %s: %v", p.Handle, err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the environment could not be restarted"})
			return
		}
		a.notifyRestarted(p.Handle)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarted": []string{p.Handle}, "notified": 1})
		return
	}
	// Every running environment.
	restarted := a.restartRunningContainers(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarted": restarted, "notified": len(restarted)})
}

// restartRunningContainers restarts every RUNNING user environment (so it
// picks up the backend's current CPU/RAM limits — those are applied at
// (re)start, never to a running container) + emails each affected user. It
// returns the handles that were restarted successfully. The disk cap needs no
// restart (the monitor reads it live); this is for the CPU/RAM caps.
//
// Restarts run in parallel (each is an independent service / compute system)
// under a bounded context. This is synchronous on purpose: the admin save
// that triggered it waits for the apply so the response can report it
// definitively (a few environments is a few seconds).
func (a *App) restartRunningContainers(ctx context.Context) []string {
	bk := a.sandboxBackend()
	if bk == nil {
		return nil
	}
	handles, ok := a.userHandles()
	if !ok {
		return nil
	}
	var running []string
	for _, h := range handles {
		if info, err := bk.Status(ctx, h); err == nil && info.State == sandbox.StateRunning {
			running = append(running, h)
		}
	}
	if len(running) == 0 {
		return nil
	}
	rc, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done []string
	for _, h := range running {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if _, err := bk.Restart(rc, h); err != nil {
				log.Printf("bevin: container restart %s: %v", h, err)
				return
			}
			mu.Lock()
			done = append(done, h)
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	for _, h := range done {
		a.notifyRestarted(h)
	}
	if len(done) > 0 {
		log.Printf("bevin: restarted %d container(s) for the new limits: %v", len(done), done)
	}
	return done
}

// notifyRestarted emails a user that their environment was restarted for a
// system reconfiguration (in-flight work was interrupted — check it). This is
// the user affordance for the fact that a limit change kills in-flight work.
func (a *App) notifyRestarted(handle string) {
	a.notify(handle, "bevin: your environment was restarted",
		"Your bevin environment was restarted because the server administrator changed the container resource limits. Any work or tasks that were running in your environment were interrupted. Please check your environment and resume anything that was in progress.")
}
