package store

import "testing"

// TestUserState — the per-user explicit state KV rows: an unset key reads
// (value, ok=false, nil), set/get round-trips, an overwrite wins, and the
// rows live independently of any conversation (the table exists on a fresh
// store, and an empty store answers "unset" rather than an error).
func TestUserState(t *testing.T) {
	us := openTestStore(t)
	defer us.Close()

	if v, ok, err := us.GetUserState(UserStateKeyOnboardingDone); err != nil || ok || v != "" {
		t.Fatalf("unset key = (%q, %v, %v), want (\"\", false, nil)", v, ok, err)
	}
	if err := us.SetUserState(UserStateKeyOnboardingDone, "1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok, err := us.GetUserState(UserStateKeyOnboardingDone)
	if err != nil || !ok || v != "1" {
		t.Fatalf("get after set = (%q, %v, %v), want (\"1\", true, nil)", v, ok, err)
	}
	if err := us.SetUserState(UserStateKeyOnboardingDone, "2"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	v, ok, err = us.GetUserState(UserStateKeyOnboardingDone)
	if err != nil || !ok || v != "2" {
		t.Fatalf("get after overwrite = (%q, %v, %v), want (\"2\", true, nil)", v, ok, err)
	}
}
