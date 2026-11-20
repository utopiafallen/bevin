package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
)

// TestAdminConfigMasksModelKeys pins the masked-secret pattern for the
// model/aux API keys (security 2026-11-20): the admin config read returns
// has_api_key, never the key itself; a blank key on save keeps the stored
// one (the UI's "leave blank to keep" contract), a provided key replaces it,
// and an aux key carries over only when provider+base+model are unchanged.
func TestAdminConfigMasksModelKeys(t *testing.T) {
	a, cfg := configApp(t)

	// READ: the model key is masked (has_api_key=true, api_key blank) —
	// the raw key must never leave the server.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("config read code = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Models []config.Model    `json:"models"`
		Aux    []config.AuxModel `json:"aux"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models = %+v, want the one saved model", got.Models)
	}
	if got.Models[0].APIKey != "" {
		t.Fatalf("the model API key leaked into the config read: %q", got.Models[0].APIKey)
	}
	if got.Models[0].HasAPIKey != true {
		t.Fatalf("has_api_key = false, want true (a key is stored)")
	}

	// SAVE with a blank key: the stored key survives (the UI is never given
	// the key to echo back, so blank is the "keep" signal).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"models": []config.Model{{ID: "m1", Provider: "openai", BaseURL: cfg.Models[0].BaseURL, APIKey: "", ModelID: "gpt-4o", DisplayName: "GPT-4o", MaxContext: 128000, Slots: 1, Enabled: true, Profile: "general"}},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("blank-key save code = %d body=%s", w.Code, w.Body.String())
	}
	onDisk, err := config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Models[0].APIKey != "sk-test" {
		t.Fatalf("a blank key should keep the stored one, got %q", onDisk.Models[0].APIKey)
	}

	// SAVE with a new key: it replaces the stored one.
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"models": []config.Model{{ID: "m1", Provider: "openai", BaseURL: cfg.Models[0].BaseURL, APIKey: "sk-new-456", ModelID: "gpt-4o", DisplayName: "GPT-4o", MaxContext: 128000, Slots: 1, Enabled: true, Profile: "general"}},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("new-key save code = %d body=%s", w.Code, w.Body.String())
	}
	onDisk, err = config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Models[0].APIKey != "sk-new-456" {
		t.Fatalf("a provided key should replace the stored one, got %q", onDisk.Models[0].APIKey)
	}

	// AUX: save one with a key; the read masks it; a blank-key save (same
	// provider+base+model) keeps it.
	auxOne := config.AuxModel{Kinds: []string{"vision"}, Provider: "openai", BaseURL: cfg.Models[0].BaseURL, APIKey: "sk-aux-789", ModelID: "gpt-4o", Profile: "general"}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{"aux": []config.AuxModel{auxOne}}))
	if w.Code != http.StatusOK {
		t.Fatalf("aux save code = %d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodGet, "/admin/api/config", nil))
	var gotAux struct {
		Aux []config.AuxModel `json:"aux"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &gotAux)
	if len(gotAux.Aux) != 1 {
		t.Fatalf("aux = %+v, want the one saved aux model", gotAux.Aux)
	}
	if gotAux.Aux[0].APIKey != "" {
		t.Fatalf("the aux API key leaked into the config read: %q", gotAux.Aux[0].APIKey)
	}
	if gotAux.Aux[0].HasAPIKey != true {
		t.Fatalf("aux has_api_key = false, want true")
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"aux": []config.AuxModel{{Kinds: []string{"vision"}, Provider: "openai", BaseURL: cfg.Models[0].BaseURL, APIKey: "", ModelID: "gpt-4o", Profile: "general"}},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("aux blank-key save code = %d body=%s", w.Code, w.Body.String())
	}
	onDisk, err = config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Aux[0].APIKey != "sk-aux-789" {
		t.Fatalf("an unchanged aux should keep its stored key, got %q", onDisk.Aux[0].APIKey)
	}

	// AUX re-key: the same aux with a CHANGED base URL and a blank key does
	// not inherit the old key (a different endpoint is a different key).
	w = httptest.NewRecorder()
	a.ServeHTTP(w, newRequest(http.MethodPost, "/admin/api/config", map[string]any{
		"aux": []config.AuxModel{{Kinds: []string{"vision"}, Provider: "openai", BaseURL: "http://other.example/v1", APIKey: "", ModelID: "gpt-4o", Profile: "general"}},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("aux re-key save code = %d body=%s", w.Code, w.Body.String())
	}
	onDisk, err = config.Load(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Aux[0].APIKey != "" {
		t.Fatalf("a changed aux endpoint must not inherit the old key, got %q", onDisk.Aux[0].APIKey)
	}
}
