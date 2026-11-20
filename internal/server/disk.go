package server

import (
	"net/http"

	"github.com/utopiafallen/bevin/internal/auth"
)

// diskGate blocks WORK endpoints for a user who is over their disk cap
// (frozen). It reads the handle from the session context (so it must be
// applied AFTER requireAuth/chatGate). A frozen user still reaches the
// cleanup surface (the disk page + the file/workspace deletes) and the
// read-only routes — those do NOT wrap this gate — so they can get back
// under the cap. 413 + disk_exceeded so the client redirects to /disk.
//
// The block is a deliberate BLOCKLIST (only the endpoints that add disk
// usage or consume a turn are gated), not a whitelist: a frozen user keeps
// every read + delete + the disk page, and only the "do more work" actions
// are refused until they're back under the cap.
func (a *App) diskGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := auth.UserFromContext(r.Context()); u != nil && a.diskMon != nil && a.diskMon.Frozen(u.Handle) {
			st := a.diskMon.Status(u.Handle)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"disk_exceeded": true,
				"used_bytes":    st.Used,
				"limit_bytes":   st.Limit,
				"message":       "your storage is over the limit — clean up on the disk page to continue",
			})
			return
		}
		next(w, r)
	}
}

// recheckDisk immediately re-walks one user's storage after a delete, so the
// frozen state (and the disk page) reflects the new usage without waiting for
// the next periodic scan. Nil-safe (a nil monitor, as in tests that don't
// start one, is a no-op).
func (a *App) recheckDisk(handle string) {
	if a.diskMon != nil {
		a.diskMon.Recheck(handle)
	}
}

// diskPage is the gated entry point to the disk cleanup page: /disk →
// /disk.html (the static page shell; its data comes from the gated /api/disk
// endpoints). An unauthenticated request goes to /login.
func (a *App) diskPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/disk.html", http.StatusFound)
}

// GET /api/disk — the user's disk usage + frozen state + cap. It triggers an
// immediate re-check (a full walk) so the numbers are fresh (the user just
// deleted something / is looking at the page). The per-workspace breakdown is
// a separate fetch to GET /api/workspaces (the page does it client-side).
func (a *App) diskStatus(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	if a.diskMon == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "disk monitor not available"})
		return
	}
	writeJSON(w, http.StatusOK, a.diskMon.Recheck(u.Handle))
}

// POST /api/disk/done — the cleanup page's "done cleaning up" button. It
// re-checks and refuses (413) while the usage is still at/above 90% of the
// cap (the same threshold the hysteresis releases at) — so the user cannot
// mark themselves done until they're actually back under the cap.
func (a *App) diskDone(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	if a.diskMon == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "disk monitor not available"})
		return
	}
	st := a.diskMon.Recheck(u.Handle)
	if st.Limit > 0 && st.Used >= int64(float64(st.Limit)*0.9) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"disk_exceeded": true,
			"used_bytes":    st.Used,
			"limit_bytes":   st.Limit,
			"message":       "still over the limit — delete more to get under 90% of your cap",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "frozen": st.Frozen})
}
