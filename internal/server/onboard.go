package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
)

// First-time user onboarding ("Meet Bevin"). A regular user's first sign-in
// gets a single-screen card (display name, default model, optional email) and
// ONE click completes it: agent.toml gains the per-user default model + the
// completion flag, and a "Meet Bevin" conversation is seeded with a welcome
// message so the user lands in a live chat, not an empty screen. The admin is
// excluded (chatGate — the admin has no user UI at all).

type onboardRequest struct {
	DisplayName string `json:"display_name"`
	ModelID     string `json:"model_id"`
	Email       string `json:"email"`
}

// onboardingStatus reports whether the user's first-time flow is done (and
// their current display name, so the card can prefill it).
func (a *App) onboardingStatus(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	ac, err := config.LoadAgent(filepath.Join(a.dataDir, "users", u.Handle))
	if err != nil {
		ac = config.DefaultAgent()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"complete":      ac.OnboardingComplete,
		"display_name":  u.DisplayName,
		"onboarding_at": ac.OnboardingAt,
	})
}

// onboardingComplete writes the onboarding state (per-user default model,
// display name, optional email), seeds the "Meet Bevin" conversation, and
// returns its id so the client can open it directly.
func (a *App) onboardingComplete(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	var req onboardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.ModelID = strings.TrimSpace(req.ModelID)
	req.Email = strings.TrimSpace(req.Email)

	if req.DisplayName != "" && len(req.DisplayName) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "display name too long (max 64)"})
		return
	}
	// model_id: blank = server default; otherwise it must be a configured +
	// enabled model.
	if req.ModelID != "" && !a.ag.ModelAvailable(req.ModelID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown or unavailable model"})
		return
	}
	if req.Email != "" && !strings.Contains(req.Email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid email address"})
		return
	}
	if req.DisplayName != "" {
		if err := a.srv.SetUserDisplayName(u.Handle, req.DisplayName); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Email != "" {
		if err := a.srv.SetUserEmail(u.Handle, req.Email); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	// Seed the conversation FIRST: it is the payload of the flow, and the
	// completion flag must only be persisted when the payload is in place —
	// otherwise a failed seeding would strand the user as "onboarded" with
	// no starting conversation (re-seeding is idempotent, so retrying after a
	// config-write failure is safe).
	name := req.DisplayName
	if name == "" {
		name = u.DisplayName
	}
	if name == "" {
		name = u.Handle
	}
	convID, err := a.ag.OnboardingConversation(r.Context(), u.Handle, req.ModelID, welcomeMessage(name))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	userDir := filepath.Join(a.dataDir, "users", u.Handle)
	ac, err := config.LoadAgent(userDir)
	if err != nil {
		ac = config.DefaultAgent()
	}
	ac.DefaultModelID = req.ModelID
	ac.OnboardingComplete = true
	ac.OnboardingAt = time.Now().UTC().Format(time.RFC3339)
	if err := config.SaveAgent(userDir, ac); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"complete":        true,
		"conversation_id": convID,
		"display_name":    name,
		// The seeded welcome message, so the client can render it as the
		// first bubble (there is no message re-fetch endpoint yet).
		"welcome": welcomeMessage(name),
	})
}

// welcomeMessage is the single seeded assistant message that opens the
// "Meet Bevin" conversation. It goes in the message (not the frozen prompt)
// so the conversation stays a normal conversation forever; it is honest
// about what works today and what is coming.
func welcomeMessage(name string) string {
	return "Hi " + name + " — I'm Bevin, your personal assistant.\n\n" +
		"Talk to me the way you'd talk to a sharp colleague: ask questions, send me text to clean up or rewrite, and I'll keep up with this conversation as it grows. " +
		"As bevin matures I'll take on more — workspaces and files, tasks, and memory that outlives a single conversation.\n\n" +
		"What would you like to start with?"
}
