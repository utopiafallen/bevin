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
	Admin    AdminAnswers   `json:"admin"`
	Models   []config.Model `json:"models"`
	Aux      []config.AuxModel `json:"aux"`
	Embedder config.Embedder   `json:"embedder"`
	Sidecars config.Sidecars   `json:"sidecars"`
}

// handleRe is a conservative handle pattern (lowercase alnum + - _ .).
var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

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
// were already installed are left in place).
func (m *Manager) Cancel() {
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
	}
	return nil
}

// FinalizeEdit commits an edited config in normal mode: it updates the models,
// aux, and embedder on the existing server.toml (preserving the session secret,
// auth, bind, port, and sidecar endpoints) without re-seeding the admin.
func (m *Manager) FinalizeEdit(ctx context.Context) (*config.ServerConfig, error) {
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
	cfg.Sidecars = a.Sidecars
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
// manages this — the user does not configure the embedder in the wizard.
func deriveEmbedder(llamaEndpoint string) config.Embedder {
	e := strings.TrimRight(strings.TrimSpace(llamaEndpoint), "/")
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

// Finalize performs the deferred commit: validate everything, seed the admin
// (idempotent), ensure the sidecars (idempotent, forward-only), then write
// server.toml (0600) as the commit of record. It is safe to call again (a
// re-run is a no-op for already-provisioned pieces).
func (m *Manager) Finalize(ctx context.Context) (*config.ServerConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finished {
		return config.Load(m.dataDir)
	}
	a := m.state.Answers
	if err := validateAdmin(a.Admin); err != nil {
		return nil, fmt.Errorf("admin: %w", err)
	}
	if err := validateModels(a.Models); err != nil {
		return nil, err
	}
	// Sidecar endpoints may be blank → auto-provisioned in EnsureAll below. The
	// embedder is derived from the llama-server endpoint after provisioning.

	secret, err := config.GenerateSessionSecret()
	if err != nil {
		return nil, err
	}
	cfg := &config.ServerConfig{
		DataDir:          m.dataDir,
		Bind:             m.bind,
		Logging:          config.Logging{Level: "info"},
		Auth:             config.Auth{Mode: "internal", SessionSecret: secret, SessionTTLSeconds: config.DefaultSessionTTLSeconds},
		Models:           a.Models,
		Aux:              a.Aux,
		Sidecars:         a.Sidecars,
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

	// Ensure sidecars (idempotent, forward-only). This provisions the
	// llama-server (the embedder) and records its loopback endpoint into cfg.
	if err := m.sidecars.EnsureAll(ctx, cfg); err != nil {
		return nil, fmt.Errorf("sidecars: %w", err)
	}

	// Derive the embedder from the (now provisioned) llama-server endpoint.
	cfg.Embedder = deriveEmbedder(cfg.Sidecars.LlamaServer)

	// Write server.toml LAST (the commit of record; 0600 — holds secrets).
	if err := config.Save(m.dataDir, cfg); err != nil {
		return nil, err
	}
	m.finished = true
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

func validateModels(models []config.Model) error {
	if len(models) == 0 {
		return errors.New("at least one main model is required")
	}
	seen := map[string]bool{}
	enabled := 0
	for i, m := range models {
		if _, err := provider.New(strings.ToLower(m.Provider), m.BaseURL, m.APIKey); err != nil {
			return fmt.Errorf("models[%d]: %v", i, err)
		}
		if strings.TrimSpace(m.BaseURL) == "" || strings.TrimSpace(m.ModelID) == "" {
			return fmt.Errorf("models[%d]: base_url and model_id are required", i)
		}
		if m.ID != "" {
			if seen[m.ID] {
				return fmt.Errorf("duplicate model id %q", m.ID)
			}
			seen[m.ID] = true
		}
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