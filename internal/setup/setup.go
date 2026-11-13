// Package setup implements first-time setup: an in-memory, token-free wizard
// state machine with a deferred commit (nothing is written to disk until
// Finalize), an idempotent forward-only SidecarManager, and admin seeding.
// Setup-mode is detected by the absence of server.toml; Finalize writes it
// (0600), seeds the admin user, and ensures the sidecars.
package setup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/store"
)

// AdminAnswers is the identity step: the admin is always the self-IdP account.
type AdminAnswers struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// Answers is the full set of wizard inputs (steps that are not review-only).
type Answers struct {
	Admin    AdminAnswers      `json:"admin"`
	OAuth    config.OAuth      `json:"oauth"`
	Models   []config.Model    `json:"models"`
	Aux      []config.AuxModel `json:"aux"`
	Embedder config.Embedder   `json:"embedder"`
	Sidecars config.Sidecars   `json:"sidecars"`
	Email    config.Email      `json:"email"`
}

// handleRe is a conservative handle pattern (lowercase alnum + - _ .).
var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

// ValidHandle reports whether s is a valid bevin handle (the same pattern the
// admin step enforces).
func ValidHandle(s string) bool { return handleRe.MatchString(s) }

// SetupState is the in-memory wizard state. It is discarded on cancel; nothing
// is persisted until Finalize.
type SetupState struct {
	Answers Answers
}

// Manager drives the wizard. It is safe for concurrent use.
type Manager struct {
	dataDir string
	bind    string
	port    int
	srv     *store.ServerStore
	prov    auth.Provider
	sidecars *SidecarManager

	mu     sync.Mutex
	state  SetupState
	finished bool

	// onFinalize, when set, is called (normal mode only) right after an edit
	// finalize succeeds, with the freshly saved config — the server uses it to
	// hot-reload the running agent so the edit applies without a restart. A
	// hook error is reported to the wizard (the config is already saved on
	// disk; the running agent stays on the old config until a restart).
	onFinalize func(cfg *config.ServerConfig) error

	// Sidecar provisioning (the display-only sidecars step): provMu guards
	// provState (the in-flight / finished run). See provision.go.
	provMu    sync.Mutex
	provState *provisionState
}

// SetOnFinalize registers the post-edit-finalize hook (normal mode). See the
// field doc on onFinalize.
func (m *Manager) SetOnFinalize(f func(cfg *config.ServerConfig) error) {
	m.onFinalize = f
}

// New builds a setup Manager. srv/prov are the live server store + auth
// provider (so Finalize can seed the admin into the same store the server
// uses); sidecars provisions the local sidecar processes.
func New(dataDir, bind string, port int, srv *store.ServerStore, prov auth.Provider, sidecars *SidecarManager) *Manager {
	if sidecars == nil {
		sidecars = NewSidecarManager(nil, nil)
	}
	return &Manager{dataDir: dataDir, bind: bind, port: port, srv: srv, prov: prov, sidecars: sidecars}
}

// IsSetup reports whether server.toml already exists (i.e. setup is done).
func (m *Manager) IsSetup() bool { return config.Exists(m.dataDir) }

// Finished reports whether this process has completed setup (in-memory).
func (m *Manager) Finished() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.finished || config.Exists(m.dataDir)
}

// Answers returns a copy of the current in-memory answers.
func (m *Manager) Answers() Answers {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Answers
}

// StepBack clears a step's answers (safe: no side effects, sidecars untouched).
func (m *Manager) StepBack(step string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearStep(step)
}

// Cancel discards all in-memory answers (safe: no side effects; sidecars that
// were already installed are left in place). It also stops an in-flight
// provisioning run + resets it (so a later StartProvision re-starts it); a
// partial download is resumable, so canceling never leaves a broken install.
func (m *Manager) Cancel() {
	m.provMu.Lock()
	if m.provState != nil {
		if m.provState.running {
			m.provState.cancel()
		}
		m.provState = nil
	}
	m.provMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = SetupState{}
}

func (m *Manager) clearStep(step string) {
	switch step {
	case "admin":
		m.state.Answers.Admin = AdminAnswers{}
	case "models":
		m.state.Answers.Models = nil
	case "aux":
		m.state.Answers.Aux = nil
	case "sidecars":
		m.state.Answers.Sidecars = config.Sidecars{}
	case "email":
		m.state.Answers.Email = config.Email{}
	}
}

// LoadFromConfig populates the wizard state from an existing server.toml so the
// wizard can be re-run in normal mode to edit the config. It preserves the
// existing session secret, auth, and sidecar endpoints (only the editable
// fields — models, aux, embedder — are surfaced for editing).
func (m *Manager) LoadFromConfig() error {
	cfg, err := config.Load(m.dataDir)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Answers = Answers{
		Models:   cfg.Models,
		Aux:      cfg.Aux,
		Embedder: cfg.Embedder,
		Sidecars: cfg.Sidecars,
		Email:    cfg.Email,
	}
	return nil
}

// FinalizeEdit commits an edited config in normal mode: it updates the models,
// aux, and embedder on the existing server.toml (preserving the session secret,
// auth, bind, port, and sidecar endpoints) without re-seeding the admin.
func (m *Manager) FinalizeEdit(ctx context.Context) (*config.ServerConfig, error) {
	// Sidecars are server-managed (display-only step): start the provisioning
	// (idempotent — in edit mode it probes the already-running sidecars, fast)
	// + wait for it BEFORE taking m.mu, so the recorded endpoints are fresh.
	m.StartProvision()
	if werr := m.WaitForProvision(ctx); werr != nil {
		log.Printf("bevin: setup: sidecar provisioning (edit): %v", werr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := config.Load(m.dataDir)
	if err != nil {
		return nil, err
	}
	a := m.state.Answers
	if err := validateModels(a.Models); err != nil {
		return nil, err
	}
	cfg.Models = a.Models
	cfg.Aux = a.Aux
	cfg.Email = a.Email
	// Sidecars are server-managed: use the endpoints the provisioning run
	// recorded (in edit mode, the already-running sidecars).
	cfg.Sidecars = m.recordedSidecarsLocked()
	// The embedder is server-managed (it is the llama-server sidecar), so its
	// config is derived from the llama-server endpoint, not user-entered.
	cfg.Embedder = deriveEmbedder(cfg.Sidecars.LlamaServer)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := config.Save(m.dataDir, cfg); err != nil {
		return nil, err
	}
	m.finished = true
	return cfg, nil
}

// CommitEdit saves a subset of the config areas the admin config pages edit —
// a non-nil argument replaces that area, a nil one is left unchanged — on the
// existing server.toml and returns the saved config. Unlike FinalizeEdit it
// does NOT run sidecar provisioning: in normal mode the sidecars are
// server-managed and already running, so their recorded endpoints + the
// derived embedder are preserved as-is and a model/email edit is a fast,
// side-effect-free write. The caller hot-reloads the running agent with the
// returned config (there is no in-memory wizard state involved).
func (m *Manager) CommitEdit(models *[]config.Model, aux *[]config.AuxModel, email *config.Email, oauth *config.OAuth) (*config.ServerConfig, error) {
	cfg, err := config.Load(m.dataDir)
	if err != nil {
		return nil, err
	}
	if models != nil {
		if err := validateModels(*models); err != nil {
			return nil, err
		}
		cfg.Models = *models
	}
	if aux != nil {
		cfg.Aux = *aux
	}
	if email != nil {
		e, err := validateEmail(*email)
		if err != nil {
			return nil, err
		}
		cfg.Email = e
	}
	if oauth != nil {
		o, err := normalizeOAuth(*oauth)
		if err != nil {
			return nil, err
		}
		cfg.Auth.OAuth = o
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := config.Save(m.dataDir, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetAdmin stores + validates the identity step.
func (m *Manager) SetAdmin(a AdminAnswers) error {
	if err := validateAdmin(a); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Answers.Admin = a
	return nil
}

// SetModels stores + validates the main-model catalog step.
func (m *Manager) SetModels(models []config.Model) error {
	if err := validateModels(models); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Answers.Models = models
	return nil
}

// SetAux stores the aux perception models step.
func (m *Manager) SetAux(aux []config.AuxModel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Answers.Aux = aux
	return nil
}

// deriveEmbedder builds the embedder config from the llama-server endpoint. The
// embedder IS the llama-server (running embeddinggemma-2 on loopback), so its
// base URL is the llama-server endpoint + /v1, with no API key. The server
// manages this — the user does not configure the embedder in the wizard. With
// no llama-server endpoint (a sidecar-free config) it returns a zero embedder:
// there is no local embedder to point at (never write a bare "/v1").
func deriveEmbedder(llamaEndpoint string) config.Embedder {
	e := strings.TrimRight(strings.TrimSpace(llamaEndpoint), "/")
	if e == "" {
		return config.Embedder{}
	}
	e = strings.TrimSuffix(e, "/v1") // the endpoint may already carry /v1
	return config.Embedder{
		BaseURL: e + "/v1",
		ModelID: "embeddinggemma-2",
		APIKey:  "",
	}
}

// SetSidecars stores the sidecar endpoints step. A blank endpoint means
// "auto-provision" — the SidecarManager downloads + launches it on loopback and
// records the resulting endpoint into the config at Finalize.
func (m *Manager) SetSidecars(sc config.Sidecars) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Answers.Sidecars = sc
	return nil
}

// SetEmail stores the (optional) email-notifier step. Email is optional: when
// it is not enabled (or has no host) the config is left blank (no sender).
func (m *Manager) SetEmail(e config.Email) error {
	e, err := validateEmail(e)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Answers.Email = e
	return nil
}

// SetOAuth stores + validates the optional external-IdP (OAuth) step.
func (m *Manager) SetOAuth(o config.OAuth) error {
	o, err := normalizeOAuth(o)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Answers.OAuth = o
	return nil
}

// normalizeOAuth trims the fields and applies defaults (scopes). When
// enabled, the endpoint URLs are required + must be absolute http(s) URLs.
// It is the source of truth for validation (the wizard mirrors the
// normalization in the client so the review step shows effective values).
func normalizeOAuth(o config.OAuth) (config.OAuth, error) {
	o.Name = strings.TrimSpace(o.Name)
	o.ClientID = strings.TrimSpace(o.ClientID)
	o.ClientSecret = strings.TrimSpace(o.ClientSecret)
	o.AuthURL = strings.TrimSpace(o.AuthURL)
	o.TokenURL = strings.TrimSpace(o.TokenURL)
	o.UserinfoURL = strings.TrimSpace(o.UserinfoURL)
	o.Scopes = strings.TrimSpace(o.Scopes)
	o.RedirectURI = strings.TrimSpace(o.RedirectURI)
	if !o.Enabled {
		return o, nil
	}
	if o.Name == "" {
		o.Name = "OAuth"
	}
	if o.ClientID == "" {
		return o, errors.New("oauth: a client ID is required when OAuth is enabled")
	}
	for _, u := range []struct {
		name, val string
	}{
		{"authorization (auth) URL", o.AuthURL},
		{"token URL", o.TokenURL},
		{"user-info URL", o.UserinfoURL},
		{"redirect URI", o.RedirectURI},
	} {
		if u.val == "" {
			return o, fmt.Errorf("oauth: %s is required when OAuth is enabled", u.name)
		}
		pu, err := url.Parse(u.val)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			return o, fmt.Errorf("oauth: %s must be an absolute http(s) URL", u.name)
		}
	}
	if o.Scopes == "" {
		o.Scopes = "openid profile email"
	}
	return o, nil
}

// SanitizeHandle maps arbitrary provider text (a preferred_username, an email
// local part, a subject) onto a valid bevin handle: lowercase, restricted to
// [a-z0-9._-], leading non-alphanumeric dropped, max 32 chars. It returns ""
// when nothing valid remains.
func SanitizeHandle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = strings.TrimLeft(out, "._-")
	if len(out) > 32 {
		out = out[:32]
	}
	if len(out) < 2 {
		return ""
	}
	return out
}

// Finalize performs the deferred commit: validate everything, seed the admin
// (idempotent), ensure the sidecars (idempotent, forward-only), then write
// server.toml (0600) as the commit of record. It is safe to call again (a
// re-run is a no-op for already-provisioned pieces).
func (m *Manager) Finalize(ctx context.Context) (*config.ServerConfig, error) {
	m.mu.Lock()
	if m.finished {
		m.mu.Unlock()
		return config.Load(m.dataDir)
	}
	a := m.state.Answers // captured copy (the answers are not mutated during Finalize)
	m.mu.Unlock()

	if err := validateAdmin(a.Admin); err != nil {
		return nil, fmt.Errorf("admin: %w", err)
	}
	if err := validateModels(a.Models); err != nil {
		return nil, err
	}
	oauth, err := normalizeOAuth(a.OAuth)
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}

	secret, err := config.GenerateSessionSecret()
	if err != nil {
		return nil, err
	}
	// Start the sidecar provisioning (idempotent — it may already have started
	// on the sidecars step) + wait for it. This runs WITHOUT m.mu held, so the
	// provisioning goroutine can read the answers (existingEndpoint) without
	// deadlocking. A sidecar provisioned on the step is not re-provisioned (no
	// port drift) and its endpoint is recorded; a run failure is not fatal here
	// (EnsureAll below re-tries the failed sidecar).
	m.StartProvision()
	if werr := m.WaitForProvision(ctx); werr != nil {
		log.Printf("bevin: setup: sidecar provisioning: %v (re-trying at finalize)", werr)
	}
	cfg := &config.ServerConfig{
		DataDir:          m.dataDir,
		Bind:             m.bind,
		Logging:          config.Logging{Level: "info"},
		Auth:             config.Auth{Mode: "internal", SessionSecret: secret, SessionTTLSeconds: config.DefaultSessionTTLSeconds, OAuth: oauth},
		Models:           a.Models,
		Aux:              a.Aux,
		Email:            a.Email,
		Sidecars:         m.recordedSidecars(),
		MaxWorkspaceSize: config.DefaultMaxWorkspaceSize,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Seed the admin (idempotent: skip if the handle already exists, else set
	// the password). This is the self-IdP admin account.
	if _, err := m.srv.GetUser(a.Admin.Handle); err != nil {
		if _, err := m.prov.CreateUserProvisioned(ctx, a.Admin.Handle, a.Admin.DisplayName, "admin", a.Admin.Password); err != nil {
			return nil, fmt.Errorf("seed admin: %w", err)
		}
	} else if err := m.prov.SetPassword(ctx, a.Admin.Handle, a.Admin.Password); err != nil {
		return nil, fmt.Errorf("set admin password: %w", err)
	}

	// Ensure sidecars (idempotent, forward-only). It probes the recorded
	// endpoints (healthy → no re-provision, no port drift) and re-provisions any
	// that are blank (a failed sidecar, retried here), recording the final
	// loopback endpoints into cfg.
	if err := m.sidecars.EnsureAll(ctx, cfg); err != nil {
		return nil, fmt.Errorf("sidecars: %w", err)
	}

	// Derive the embedder from the (now provisioned) llama-server endpoint.
	cfg.Embedder = deriveEmbedder(cfg.Sidecars.LlamaServer)

	// Write server.toml LAST (the commit of record; 0600 — holds secrets).
	if err := config.Save(m.dataDir, cfg); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.finished = true
	m.mu.Unlock()
	return cfg, nil
}

func validateAdmin(a AdminAnswers) error {
	if !handleRe.MatchString(a.Handle) {
		return errors.New("handle must be 2-32 chars, lowercase letters/digits, starting with a letter or digit")
	}
	if strings.TrimSpace(a.DisplayName) == "" {
		return errors.New("display name is required")
	}
	if len(a.Password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

// validateEmail checks the (optional) email-notifier step and applies the
// field PLACEHOLDERS as defaults for blank values (the wizard shows them as
// hints; leaving a field blank uses the hint value): port -> 587 (standard
// SMTP submission), from name -> "bevin", from address -> the username. When
// email is not enabled it is a no-op (a blank email is valid); when enabled,
// a host is required (the sender is built from it).
func validateEmail(e config.Email) (config.Email, error) {
	if !e.Enabled {
		return e, nil
	}
	if strings.TrimSpace(e.SMTPHost) == "" {
		return e, errors.New("email: an SMTP host is required when email is enabled")
	}
	if e.SMTPPort <= 0 {
		e.SMTPPort = 587 // placeholder default (standard SMTP submission port)
	} else if e.SMTPPort > 65535 {
		return e, errors.New("email: SMTP port must be 1-65535")
	}
	if e.FromName == "" {
		e.FromName = "bevin" // placeholder default
	}
	if e.FromAddress == "" {
		e.FromAddress = e.Username // placeholder: the username
	}
	if e.FromAddress == "" {
		return e, errors.New("email: a from address (or username) is required when email is enabled")
	}
	return e, nil
}

func validateModels(models []config.Model) error {
	if len(models) == 0 {
		return errors.New("at least one main model is required")
	}
	seen := map[string]bool{}
	enabled := 0
	for i := range models {
		m := &models[i]
		if _, err := provider.New(strings.ToLower(m.Provider), m.BaseURL, m.APIKey); err != nil {
			return fmt.Errorf("models[%d]: %v", i, err)
		}
		if strings.TrimSpace(m.BaseURL) == "" || strings.TrimSpace(m.ModelID) == "" {
			return fmt.Errorf("models[%d]: base_url and model_id are required", i)
		}
		// The internal ID is what everything references (slot pools,
		// conversation active model, per-user defaults) — derive it when the
		// form left it empty (same derivation as config.Validate, so the id
		// is stable across save + reload).
		if m.ID == "" {
			base := config.ModelIDSlug(m.ModelID)
			id, n := base, 2
			for seen[id] {
				id = fmt.Sprintf("%s-%d", base, n)
				n++
			}
			m.ID = id
		}
		if seen[m.ID] {
			return fmt.Errorf("duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
		if m.Slots < 1 {
			m.Slots = 1
		}
		if m.Enabled {
			enabled++
		}
		if m.DisplayName == "" {
			m.DisplayName = m.ModelID
		}
	}
	if enabled == 0 {
		return errors.New("at least one model must be enabled")
	}
	return nil
}