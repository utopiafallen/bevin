package setup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	installed      map[string]bool // kind → installed (the probe reports healthy for these)
	probes, installs int
}

func (f *fakeSidecars) probe(ctx context.Context, dataDir, kind, endpoint string) (bool, error) {
	f.probes++
	if f.probeInstalled {
		return true, nil
	}
	return f.installed[kind], nil
}
func (f *fakeSidecars) install(ctx context.Context, dataDir, kind string, preferredPort int) (string, error) {
	f.installs++
	if f.installed == nil {
		f.installed = map[string]bool{}
	}
	f.installed[kind] = true
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
		ID: "m1", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "test",
		ModelID: "gpt-4o", DisplayName: "GPT-4o", Metadata: config.Metadata{Reasoning: 8, Knowledge: 9, Speed: 7},
		MaxContext: 128000, Slots: 1, Enabled: true,
	}})
	_ = m.SetSidecars(config.Sidecars{Qdrant: "http://127.0.0.1:6333", LlamaServer: "http://127.0.0.1:8081"})
	_ = m.SetEmail(config.Email{Enabled: true, SMTPHost: "smtp.example.com", SMTPPort: 587, Username: "bevin@example.com", Password: "pw", FromName: "Bevin"})
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
	// The email config is saved to server.toml (the port default + the from
	// address default are applied by Validate).
	if cfg.Email.SMTPHost != "smtp.example.com" || !cfg.Email.Configured() {
		t.Fatalf("email not saved: %+v", cfg.Email)
	}
	if cfg.Email.FromAddress != "bevin@example.com" {
		t.Fatalf("email from_address defaulted to %q (want the username)", cfg.Email.FromAddress)
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
	if _, err := downloadResumable(context.Background(), srv.URL, dest, KindQdrant, "test file"); err != nil {
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
	if _, err := downloadResumable(context.Background(), srv.URL, dest2, KindQdrant, "test file"); err != nil {
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

// TestCommitEdit — the admin config pages save a subset of the config areas
// (models / aux / email) on the existing server.toml WITHOUT re-running
// sidecar provisioning: the session secret, sidecar endpoints, and the
// derived embedder are preserved as-is, and the un-saved areas are untouched.
func TestCommitEdit(t *testing.T) {
	m, _, _ := newTestSetup(t)
	validAnswers(m)
	cfg, err := m.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Save ONLY models: a fresh catalog (slots clamp applied, a second model
	// added). Email + sidecars + secret must survive unchanged.
	newModels := []config.Model{
		{ID: "m1", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-x", ModelID: "gpt-4o", DisplayName: "GPT-4o", MaxContext: 128000, Slots: 0, Enabled: true}, // slots 0 -> clamped to 1
		{ID: "m2", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-x", ModelID: "gpt-4o-mini", DisplayName: "GPT-4o mini", MaxContext: 128000, Slots: 2, Enabled: true},
	}
	got, err := m.CommitEdit(&newModels, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(got.Models))
	}
	if got.Models[0].Slots != 1 {
		t.Fatalf("slots 0 should clamp to 1, got %d", got.Models[0].Slots)
	}
	if got.Email != cfg.Email {
		t.Fatalf("email should be preserved on a models-only save: %+v vs %+v", got.Email, cfg.Email)
	}
	if got.Sidecars != cfg.Sidecars {
		t.Fatalf("sidecars should be preserved on a models-only save")
	}
	if got.Auth.SessionSecret != cfg.Auth.SessionSecret {
		t.Fatal("session secret should be preserved (not regenerated)")
	}
	if got.Embedder != cfg.Embedder {
		t.Fatalf("embedder should be preserved on a models-only save: %+v vs %+v", got.Embedder, cfg.Embedder)
	}
	// The change landed on disk (reloading the config sees it).
	reloaded, err := config.Load(m.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Models) != 2 || reloaded.Models[1].ID != "m2" {
		t.Fatalf("models-only save should persist to disk, got %+v", reloaded.Models)
	}
	// A save with NO areas is not a valid save (the caller rejects it), but an
	// all-nil CommitEdit is a no-op write of the current config.
	same, err := m.CommitEdit(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Models) != 2 {
		t.Fatalf("all-nil CommitEdit should leave the models alone, got %d", len(same.Models))
	}
	// An invalid model catalog is rejected and does NOT clobber the saved one.
	bad := []config.Model{{ID: "mX", Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "sk-x", ModelID: "x", Enabled: false}} // none enabled
	if _, err := m.CommitEdit(&bad, nil, nil, nil); err == nil {
		t.Fatal("expected error for a model catalog with no enabled model")
	}
	after, err := config.Load(m.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Models) != 2 {
		t.Fatalf("a rejected save must not clobber the saved models, got %d", len(after.Models))
	}
}

// TestProvisionEditSkipsUnconfigured — in normal mode (a config already
// exists) the provisioning run keeps only the CONFIGURED sidecars alive; a
// config that records no sidecars gets none installed (setup mode, by
// contrast, always provisions all three). The unconfigured kinds report
// ready/skipped so the wizard's sidecars step can still advance.
func TestProvisionEditSkipsUnconfigured(t *testing.T) {
	dir := t.TempDir()
	// A config that exists but records no sidecars.
	cfg := &config.ServerConfig{
		Version: config.ConfigVersion,
		DataDir: dir,
		Bind:    "0.0.0.0",
		Auth:    config.Auth{SessionSecret: "seed"},
		Models:  []config.Model{{ID: "m1", Provider: "openai", BaseURL: "http://127.0.0.1:1", ModelID: "m1", DisplayName: "M1", MaxContext: 4096, Slots: 1, Enabled: true}},
	}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	srv, err := store.OpenServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	fs := &fakeSidecars{}
	sm := NewSidecarManager(fs.probe, fs.install)
	m := New(dir, "0.0.0.0", 5656, srv, auth.NewInternalProvider(srv, time.Hour), sm)
	// Normal mode: load the existing config into the wizard state.
	if err := m.LoadFromConfig(); err != nil {
		t.Fatal(err)
	}

	m.StartProvision()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.WaitForProvision(ctx); err != nil {
		t.Fatal(err)
	}
	if fs.installs != 0 {
		t.Fatalf("edit-mode provisioning installed sidecars: %d installs (want 0 — the config recorded none)", fs.installs)
	}
	done, running, statuses, sidecars := m.ProvisionStatus()
	if !done || running {
		t.Fatalf("run should be done (done=%v running=%v)", done, running)
	}
	if sidecars.Qdrant != "" || sidecars.LlamaServer != "" || sidecars.FFmpeg != "" {
		t.Fatalf("unconfigured sidecars got recorded endpoints: %+v", sidecars)
	}
	for _, s := range statuses {
		if s.Status != "ready" {
			t.Fatalf("unconfigured sidecar %s shows %q (want ready/skipped so the wizard step can advance)", s.Kind, s.Status)
		}
	}
}

func TestDeriveEmbedder(t *testing.T) {
	// No llama-server endpoint (sidecar-free config) → zero embedder, never a
	// bare "/v1".
	e := deriveEmbedder("")
	if e != (config.Embedder{}) {
		t.Fatalf("empty endpoint should give a zero embedder, got %+v", e)
	}
	if got := deriveEmbedder("http://127.0.0.1:8081"); got.BaseURL != "http://127.0.0.1:8081/v1" || got.ModelID != "embeddinggemma-2" {
		t.Fatalf("endpoint + /v1: %+v", got)
	}
	if got := deriveEmbedder("http://127.0.0.1:8081/v1"); got.BaseURL != "http://127.0.0.1:8081/v1" {
		t.Fatalf("endpoint with /v1 should not double-suffix: %+v", got)
	}
}

// startInlineSMTP is a throwaway single-connection SMTP sink that accepts
// anything well-formed (used by the TestEmail regression tests).
func startInlineSMTP(t *testing.T) (string, *atomic.Bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	delivered := &atomic.Bool{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		fmt.Fprintln(conn, "220 fake")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(strings.ToUpper(line))
			switch {
			case strings.HasPrefix(cmd, "AUTH"):
				fmt.Fprintln(conn, "235 Authentication successful")
			case strings.HasPrefix(cmd, "MAIL FROM:") && strings.Contains(line, "bevin@example.com"):
				delivered.Store(true)
				fmt.Fprintln(conn, "250 OK")
			case cmd == "QUIT":
				fmt.Fprintln(conn, "221 bye")
				return
			case strings.HasPrefix(cmd, "DATA"):
				fmt.Fprintln(conn, "354 OK")
			dataLoop:
				for {
					mline, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if mline == ".\r\n" || mline == ".\n" {
						break dataLoop
					}
				}
				fmt.Fprintln(conn, "250 OK")
			default: // EHLO, RCPT TO, ...
				if strings.HasPrefix(cmd, "EHLO") || strings.HasPrefix(cmd, "HELO") {
					fmt.Fprint(conn, "250-fake\n250-AUTH PLAIN\n250 OK\n")
					continue
				}
				fmt.Fprintln(conn, "250 OK")
			}
		}
	}()
	return ln.Addr().String(), delivered
}

// TestTestEmailDraftDials — regression for the wizard's test-email button
// reporting a fake success: the wizard's payload carries no "enabled" flag
// (the test section is only shown when the draft is enabled), so the backend
// must NOT let email.New return a nil sender and no-op into a 200 "ok". A
// draft with a host must actually dial the SMTP server and deliver.
func TestTestEmailDraftDials(t *testing.T) {
	m, _, _ := newTestSetup(t) // setup mode: no server.toml saved yet
	addr, delivered := startInlineSMTP(t)
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// Draft WITHOUT the enabled flag — the wizard's exact payload shape.
	err := m.TestEmail(context.Background(), config.Email{
		SMTPHost: host, SMTPPort: port,
		Username: "bevin@example.com", FromName: "Bevin",
	}, "user@example.com")
	if err != nil {
		t.Fatalf("draft test-email without enabled flag: %v (the fake-success bug: it must dial, not no-op)", err)
	}
	if !delivered.Load() {
		t.Fatal("the SMTP server never saw the message (no connection was attempted)")
	}

	// No host at all (setup mode, nothing saved) → an explicit error, not a
	// silent no-op.
	if err := m.TestEmail(context.Background(), config.Email{}, "user@example.com"); err == nil {
		t.Fatal("empty draft with no saved config must error, not silently succeed")
	}
}

// TestSetEmailPlaceholderDefaults — a blank email field uses its placeholder
// value as the default (the wizard's hints: port "587", from name "bevin",
// from address "blank = the username"); an explicit value is preserved and an
// out-of-range port is still rejected.
func TestSetEmailPlaceholderDefaults(t *testing.T) {
	m, _, _ := newTestSetup(t)
	// Blank port + from fields, only the host + username filled in.
	if err := m.SetEmail(config.Email{Enabled: true, SMTPHost: "smtp.example.com", Username: "me@example.com"}); err != nil {
		t.Fatalf("blank port/from fields should use placeholder defaults, got: %v", err)
	}
	e := m.Answers().Email
	if e.SMTPPort != 587 {
		t.Fatalf("blank port should default to 587 (the placeholder), got %d", e.SMTPPort)
	}
	if e.FromName != "bevin" {
		t.Fatalf("blank from name should default to the placeholder %q, got %q", "bevin", e.FromName)
	}
	if e.FromAddress != "me@example.com" {
		t.Fatalf("blank from address should default to the username, got %q", e.FromAddress)
	}
	// An explicit port is preserved (not overwritten by the default).
	if err := m.SetEmail(config.Email{Enabled: true, SMTPHost: "h", SMTPPort: 25, Username: "u", FromAddress: "u@x"}); err != nil {
		t.Fatalf("explicit port 25 should be accepted: %v", err)
	}
	if m.Answers().Email.SMTPPort != 25 {
		t.Fatal("an explicit port must be preserved (no defaulting over it)")
	}
	// Out-of-range is still rejected; host is still required when enabled.
	if err := m.SetEmail(config.Email{Enabled: true, SMTPHost: "h", SMTPPort: 99999}); err == nil {
		t.Fatal("port > 65535 must still be rejected")
	}
	if err := m.SetEmail(config.Email{Enabled: true}); err == nil {
		t.Fatal("an enabled email without a host must still be rejected")
	}
	// Disabled: nothing is validated or defaulted (a blank email is valid).
	if err := m.SetEmail(config.Email{Enabled: false, SMTPHost: "", SMTPPort: 0}); err != nil {
		t.Fatalf("a disabled/blank email must be accepted: %v", err)
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
	// Finalize also issues the admin's session cookie, so the completion
	// screen's "Go to the admin dashboard" button works immediately (the
	// setup cookie alone cannot enter the admin area). http.SetCookie appends,
	// so look across all Set-Cookie values.
	foundSession := false
	for _, sc := range w.Header().Values("Set-Cookie") {
		if strings.Contains(sc, "bevin_session=") {
			foundSession = true
		}
	}
	if !foundSession {
		t.Fatalf("finalize should set the admin session cookie, got Set-Cookie=%v", w.Header().Values("Set-Cookie"))
	}
}
