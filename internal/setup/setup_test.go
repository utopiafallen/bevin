package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/store"
)

func jsonReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func TestWizardTemplateRenders(t *testing.T) {
	var buf bytes.Buffer
	if err := wizardTmpl.Execute(&buf, map[string]any{"DataDir": "/tmp/x", "Server": map[string]any{"bind": "0.0.0.0", "port": 5656, "dataDir": "/tmp/x"}, "Steps": stepOrder}); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	out := buf.String()
	if len(out) < 100 {
		t.Fatalf("page too short (%d bytes)", len(out))
	}
	if !strings.Contains(out, "First-time setup") {
		t.Fatalf("page missing heading; head: %.200s", out)
	}
	if !strings.Contains(out, `["server"`) {
		t.Fatalf("page missing STEPS JSON; head: %.200s", out)
	}
	if !strings.Contains(out, `"port":5656`) {
		t.Fatalf("page missing server port; head: %.200s", out)
	}
	if !strings.Contains(out, `q("#ad_handle").placeholder`) {
		t.Fatalf("page missing admin handle placeholder fallback; head: %.200s", out)
	}
}

type fakeSidecars struct {
	probeInstalled bool
	probes, installs int
}

func (f *fakeSidecars) probe(ctx context.Context, dataDir, kind, endpoint string) (bool, error) {
	f.probes++
	return f.probeInstalled, nil
}
func (f *fakeSidecars) install(ctx context.Context, dataDir, kind string) (string, error) {
	f.installs++
	return "http://127.0.0.1:1", nil
}

func newTestSetup(t *testing.T) (*Manager, *fakeSidecars, string) {
	t.Helper()
	dir := t.TempDir()
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	prov := auth.NewInternalProvider(srv, time.Hour)
	fs := &fakeSidecars{}
	sm := NewSidecarManager(fs.probe, fs.install)
	return New(dir, "0.0.0.0", 5656, srv, prov, sm), fs, dir
}

func validAnswers(m *Manager) {
	_ = m.SetAdmin(AdminAnswers{Handle: "admin", DisplayName: "Admin", Password: "adminpass1"})
	_ = m.SetModels([]config.Model{{
		ID: "m1", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-x",
		ModelID: "gpt-4o", DisplayName: "GPT-4o", Metadata: config.Metadata{Reasoning: 8, Knowledge: 9, Speed: 7},
		MaxContext: 128000, Slots: 1, Enabled: true,
	}})
	_ = m.SetSidecars(config.Sidecars{Qdrant: "http://127.0.0.1:6333", LlamaServer: "http://127.0.0.1:8081"})
}

func TestStateValidation(t *testing.T) {
	m, _, _ := newTestSetup(t)
	if err := m.SetAdmin(AdminAnswers{Handle: "A", DisplayName: "x", Password: "short"}); err == nil {
		t.Fatal("expected admin validation error (bad handle + short password)")
	}
	if err := m.SetModels(nil); err == nil {
		t.Fatal("expected error for no models")
	}
	if err := m.SetModels([]config.Model{{Provider: "openai", BaseURL: "http://x", ModelID: "m", Enabled: false}}); err == nil {
		t.Fatal("expected error when no model is enabled")
	}
	if err := m.SetModels([]config.Model{{Provider: "wat", BaseURL: "http://x", ModelID: "m", Enabled: true}}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestFinalize(t *testing.T) {
	m, fs, dir := newTestSetup(t)
	validAnswers(m)

	cfg, err := m.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.DataDir != dir {
		t.Fatalf("cfg = %+v", cfg)
	}
	// server.toml written, mode 0600.
	info, err := os.Stat(filepath.Join(dir, "server.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("server.toml mode = %o, want 0600", info.Mode().Perm())
	}
	// admin seeded.
	if !m.IsSetup() {
		t.Fatal("IsSetup should be true after finalize")
	}
	// sidecars: qdrant + llama probed (endpoints set) then installed; ffmpeg installed directly.
	if fs.installs != 3 {
		t.Fatalf("installs = %d, want 3", fs.installs)
	}
	// Idempotent second finalize (reads back, no re-seed / no re-install).
	installedBefore := fs.installs
	cfg2, err := m.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fs.installs != installedBefore {
		t.Fatalf("second finalize re-installed sidecars (%d -> %d)", installedBefore, fs.installs)
	}
	if cfg2.Auth.SessionSecret == "" {
		t.Fatal("reloaded config missing session secret")
	}
}

func TestCancelAndStepBack(t *testing.T) {
	m, _, _ := newTestSetup(t)
	validAnswers(m)
	m.SetAdmin(AdminAnswers{Handle: "admin", DisplayName: "A", Password: "adminpass1"})
	if a := m.Answers(); a.Admin.Handle == "" {
		t.Fatal("admin should be set")
	}
	m.StepBack("admin")
	if a := m.Answers(); a.Admin.Handle != "" {
		t.Fatal("stepback should clear admin")
	}
	m.SetAdmin(AdminAnswers{Handle: "admin", DisplayName: "A", Password: "adminpass1"})
	m.Cancel()
	if a := m.Answers(); a.Admin.Handle != "" || len(a.Models) != 0 {
		t.Fatal("cancel should clear all answers")
	}
	// Nothing was written.
	if config.Exists(m.dataDir) {
		t.Fatal("cancel should not write server.toml")
	}
}

func TestEnsureIdempotent(t *testing.T) {
	fs := &fakeSidecars{probeInstalled: true}
	sm := NewSidecarManager(fs.probe, fs.install)
	if _, err := sm.Ensure(context.Background(), t.TempDir(), KindQdrant, "http://x"); err != nil {
		t.Fatal(err)
	}
	if fs.installs != 0 {
		t.Fatal("Ensure should no-op when probe reports installed")
	}
}

func TestDownloadResumable(t *testing.T) {
	data := []byte("hello world, this is a test payload 12345")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			var from int
			fmt.Sscanf(rng, "bytes=%d-", &from)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[from:])
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	// Fresh full download.
	dest := filepath.Join(t.TempDir(), "file.bin")
	if _, err := downloadResumable(context.Background(), srv.URL, dest); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(data) {
		t.Fatalf("content mismatch: %q", got)
	}

	// Resume from a partial .part.
	dir := t.TempDir()
	dest2 := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(dest2+".part", []byte("hello wo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadResumable(context.Background(), srv.URL, dest2); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(dest2)
	if string(got2) != string(data) {
		t.Fatalf("resume content mismatch: %q", got2)
	}
}

func TestLoadFromConfigAndFinalizeEdit(t *testing.T) {
	m, _, _ := newTestSetup(t)
	validAnswers(m)
	cfg, err := m.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Load the config into the wizard state (normal-mode edit).
	if err := m.LoadFromConfig(); err != nil {
		t.Fatal(err)
	}
	a := m.Answers()
	if len(a.Models) != 1 || a.Models[0].ID != "m1" {
		t.Fatalf("expected 1 model m1, got %+v", a.Models)
	}
	// Edit: add a second model + mark the first auto-disabled (simulating the
	// agent's runtime auto-disable).
	edited := append([]config.Model{}, a.Models...)
	edited[0].AutoDisabled = true
	edited = append(edited, config.Model{
		ID: "m2", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "***",
		ModelID: "gpt-4o-mini", DisplayName: "GPT-4o mini", MaxContext: 128000, Slots: 1, Enabled: true,
	})
	if err := m.SetModels(edited); err != nil {
		t.Fatal(err)
	}
	cfg2, err := m.FinalizeEdit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg2.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg2.Models))
	}
	// The session secret is preserved (not regenerated) on edit.
	if cfg2.Auth.SessionSecret != cfg.Auth.SessionSecret {
		t.Fatal("session secret should be preserved on edit")
	}
	// The auto-disabled state survives the edit (m1 was auto-disabled).
	for _, m := range cfg2.Models {
		if m.ID == "m1" && !m.AutoDisabled {
			t.Fatal("auto-disabled state should be preserved on edit")
		}
		if m.ID == "m2" && m.AutoDisabled {
			t.Fatal("new model should not be auto-disabled")
		}
	}
}

func TestSetupHandler(t *testing.T) {
	m, _, _ := newTestSetup(t)
	transitioned := false
	h := NewHandler(m, "tok123", func() { transitioned = true })
	mux := http.NewServeMux()
	h.Routes(mux)

	// No token → 401.
	r := httptest.NewRequest(http.MethodGet, "/setup/state", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-token code = %d", w.Code)
	}

	// With token → 200 JSON.
	r = httptest.NewRequest(http.MethodGet, "/setup/state?token=tok123", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("state code = %d body=%s", w.Code, w.Body.String())
	}
	var st map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["setup"] != false {
		t.Fatalf("expected setup=false, got %v", st["setup"])
	}

	// Advance with a valid admin.
	body, _ := json.Marshal(advancePayload{Step: "admin", Admin: AdminAnswers{Handle: "admin", DisplayName: "A", Password: "adminpass1"}})
	r = httptest.NewRequest(http.MethodPost, "/setup/advance?token=tok123", jsonReader(body))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("advance code = %d body=%s", w.Code, w.Body.String())
	}

	// Advance with an invalid admin → 400.
	body, _ = json.Marshal(advancePayload{Step: "admin", Admin: AdminAnswers{Handle: "A", DisplayName: "x", Password: "short"}})
	r = httptest.NewRequest(http.MethodPost, "/setup/advance?token=tok123", jsonReader(body))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid advance code = %d", w.Code)
	}

	// Fill the remaining answers directly on the manager (the API does this
	// per-step), then finalize through the ORIGINAL handler — its transition
	// callback must fire.
	validAnswers(m)
	body, _ = json.Marshal(map[string]any{})
	r = httptest.NewRequest(http.MethodPost, "/setup/finalize?token=tok123", jsonReader(body))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("finalize code = %d body=%s", w.Code, w.Body.String())
	}
	if !transitioned {
		t.Fatal("finalize should trigger the in-process transition")
	}
	if !m.IsSetup() {
		t.Fatal("after finalize, setup should be complete")
	}
}
