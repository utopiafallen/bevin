// Package config loads and writes bevin's TOML configuration.
//
// Configuration is TOML files only — no environment variables. Two tiers live
// on disk: server.toml (admin tier, at the data-dir root; holds secrets so it
// is written 0600) and a per-user agent.toml (user preferences). The data
// directory defaults to the server binary's own directory; it is CLI-only
// (-data), never editable from the UI.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	ServerFileName = "server.toml"
	AgentFileName  = "agent.toml"
)

// ConfigVersion is the schema version of server.toml. Bump it when a release
// changes the schema and add a matching step in migrate() so older configs are
// upgraded in place on load; Load refuses a config newer than this build.
const ConfigVersion = 1

// DefaultSessionTTL is 30 days.
const DefaultSessionTTLSeconds = 30 * 24 * 3600

// DefaultMaxWorkspaceSize is 5 GiB.
const DefaultMaxWorkspaceSize = int64(5 * 1024 * 1024 * 1024)

// ServerConfig is the admin-tier configuration (server.toml).
type ServerConfig struct {
	Version int `toml:"version"`
	DataDir string     `toml:"data_dir"`
	Bind             string     `toml:"bind"`
	Logging          Logging    `toml:"logging"`
	Auth             Auth       `toml:"auth"`
	Models           []Model    `toml:"models"`
	Aux              []AuxModel `toml:"aux"`
	Embedder         Embedder   `toml:"embedder"`
	Sidecars         Sidecars   `toml:"sidecars"`
	MaxWorkspaceSize int64      `toml:"max_workspace_size"`
}

type Logging struct {
	Level string `toml:"level"` // debug | info | warn | error
}

type Auth struct {
	Mode              string `toml:"mode"` // internal | oauth (future)
	SessionSecret     string `toml:"session_secret"`
	SessionTTLSeconds int    `toml:"session_ttl_seconds"`
	OAuth             OAuth  `toml:"oauth"`
}

// OAuth is the (future) external IdP tier, e.g. authentik. Unused while
// Auth.Mode == "internal".
type OAuth struct {
	ProviderURL  string `toml:"provider_url"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
}

// Model is a main text model in the admin catalog. A model is a provider
// endpoint (Provider + BaseURL + APIKey) plus the API model name (ModelID).
type Model struct {
	ID          string   `toml:"id" json:"id"`
	Provider    string   `toml:"provider" json:"provider"` // openai | anthropic | llama
	BaseURL     string   `toml:"base_url" json:"base_url"`
	APIKey      string   `toml:"api_key" json:"api_key"`
	ModelID     string   `toml:"model_id" json:"model_id"`
	DisplayName string   `toml:"display_name" json:"display_name"`
	MaxContext  int      `toml:"max_context" json:"max_context"`
	Slots       int      `toml:"slots" json:"slots"`
	Enabled     bool     `toml:"enabled" json:"enabled"`
	// AutoDisabled is set by the agent when the provider reports this model is
	// unavailable (e.g. the model was deleted/renamed upstream) and cleared when
	// a later attempt succeeds. It is independent of the admin's Enabled flag:
	// a model is usable only when Enabled && !AutoDisabled.
	AutoDisabled bool     `toml:"auto_disabled" json:"auto_disabled"`
	Metadata     Metadata `toml:"metadata" json:"metadata"`
}

// Metadata is the admin choice-metadata surfaced to help users pick a model
// (each on a 1-10 scale).
type Metadata struct {
	Reasoning int `toml:"reasoning" json:"reasoning"`
	Knowledge int `toml:"knowledge" json:"knowledge"`
	Speed     int `toml:"speed" json:"speed"`
}

// Available reports whether the model is usable by the agent: admin-enabled and
// not auto-disabled (e.g. the provider reported it unavailable). The two flags
// are independent, so an admin can disable a model while the auto-disable state
// is tracked separately.
func (m Model) Available() bool { return m.Enabled && !m.AutoDisabled }

// AuxModel is an admin-set perception model (vision/audio/video), hidden from
// the user and used mid-conversation to comprehend a non-text asset.
type AuxModel struct {
	Kind     string `toml:"kind" json:"kind"` // vision | audio | video
	Provider string `toml:"provider" json:"provider"`
	BaseURL  string `toml:"base_url" json:"base_url"`
	APIKey   string `toml:"api_key" json:"api_key"`
	ModelID  string `toml:"model_id" json:"model_id"`
}

// Embedder is the local llama-server serving /v1/embeddings
// (embeddinggemma-2).
type Embedder struct {
	BaseURL string `toml:"base_url" json:"base_url"`
	APIKey  string `toml:"api_key" json:"api_key"`
	ModelID string `toml:"model_id" json:"model_id"`
}

// Sidecars holds endpoints/paths of external sidecar processes. In production
// these bind to loopback so only bevin (behind the reverse proxy) reaches
// them.
type Sidecars struct {
	Qdrant      string `toml:"qdrant" json:"qdrant"`
	LlamaServer string `toml:"llama_server" json:"llama_server"`
	FFmpeg      string `toml:"ffmpeg" json:"ffmpeg"`
}

// Path is the location of server.toml for a data dir.
func Path(dir string) string { return filepath.Join(filepath.Clean(dir), ServerFileName) }

// AgentPath is the location of a user's agent.toml (in the user's dir).
func AgentPath(dir string) string { return filepath.Join(filepath.Clean(dir), AgentFileName) }

// Exists reports whether server.toml is present (drives setup-mode detection).
func Exists(dir string) bool {
	_, err := os.Stat(Path(dir))
	return err == nil
}

// Load reads and validates server.toml from the data dir. A config whose
// version is newer than this build is refused; an older config is migrated in
// place to the current schema (the migrated version is persisted the next time
// the config is saved).
func Load(dir string) (*ServerConfig, error) {
	dir = filepath.Clean(dir)
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		return nil, err
	}
	var cfg ServerConfig
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ServerFileName, err)
	}
	if cfg.Version > ConfigVersion {
		return nil, fmt.Errorf("server.toml version %d is newer than this bevin build (v%d) — upgrade bevin", cfg.Version, ConfigVersion)
	}
	if err := migrate(&cfg); err != nil {
		return nil, err
	}
	if cfg.DataDir == "" {
		cfg.DataDir = dir
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// migrate upgrades a parsed config from its stored version up to ConfigVersion.
// Each release that changes the schema adds one numbered step; the loop applies
// them in order so a config several versions old is walked forward one step at
// a time.
func migrate(c *ServerConfig) error {
	for c.Version < ConfigVersion {
		switch c.Version {
		case 0:
			// v0 -> v1: the schema gained the "version" field. v0 configs were
			// already structurally valid v1, so no field transform is needed —
			// just stamp the version. Future schema changes add cases here.
			c.Version = 1
		default:
			return fmt.Errorf("server.toml: unknown config version %d", c.Version)
		}
	}
	return nil
}

// Save writes server.toml atomically with 0600 permissions (it holds secrets).
func Save(dir string, cfg *ServerConfig) error {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if cfg.DataDir == "" {
		cfg.DataDir = dir
	}
	cfg.Version = ConfigVersion
	b, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := Path(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, Path(dir)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Validate applies defaults and checks required fields.
func (c *ServerConfig) Validate() error {
	// Defaults.
	if c.Bind == "" {
		c.Bind = "0.0.0.0"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Auth.SessionTTLSeconds <= 0 {
		c.Auth.SessionTTLSeconds = DefaultSessionTTLSeconds
	}
	if c.MaxWorkspaceSize <= 0 {
		c.MaxWorkspaceSize = DefaultMaxWorkspaceSize
	}
	for i := range c.Models {
		if c.Models[i].Slots < 1 {
			c.Models[i].Slots = 1 // every model has at least one execution slot
		}
		if c.Models[i].DisplayName == "" {
			c.Models[i].DisplayName = c.Models[i].ModelID
		}
	}
	// Required.
	if len(c.Models) == 0 {
		return errors.New("server.toml: at least one [[models]] entry is required")
	}
	for i, m := range c.Models {
		if m.Provider == "" || m.BaseURL == "" || m.ModelID == "" {
			return fmt.Errorf("server.toml: models[%d] (%q) needs provider, base_url and model_id", i, m.ID)
		}
	}
	if c.Auth.SessionSecret == "" {
		return errors.New("server.toml: auth.session_secret is required")
	}
	return nil
}

// DefaultDataDir is the server binary's own directory (cross-platform: works
// for a Windows .exe and a Linux ELF alike).
func DefaultDataDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err2 := filepath.EvalSymlinks(exe); err2 == nil {
		exe = r
	}
	return filepath.Dir(exe), nil
}

// CleanPath expands a leading "~" and resolves to an absolute, cleaned path.
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		rel := strings.TrimPrefix(p, "~")
		rel = strings.TrimPrefix(rel, "/")
		rel = strings.TrimPrefix(rel, `\`)
		p = filepath.Join(home, rel)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// GenerateSessionSecret returns a 32-byte random secret, hex-encoded.
func GenerateSessionSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ResolveDisplayName maps a (possibly overloaded) display name to the concrete
// Model a conversation should use. Several configured models may share one
// display name (many-to-one); among the AVAILABLE models (Enabled and not
// auto-disabled) with that display name, it returns the one with the most idle
// execution capacity.
//
// idleSlots reports a model's currently idle slots (from the per-model slot
// scheduler); pass nil before that exists to fall back to total slot capacity,
// then catalog order (ties keep the earlier model). The conversation stores the
// returned Model and keeps it fixed for the rest of the conversation, re-running
// resolution only when the user picks a different model, at which point the new
// choice is what gets resolved.
func ResolveDisplayName(models []Model, displayName string, idleSlots func(Model) int) (Model, bool) {
	var candidates []Model
	for _, m := range models {
		if m.Available() && m.DisplayName == displayName {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return Model{}, false
	}
	best, bestScore := candidates[0], -1
	for i, m := range candidates {
		score := m.Slots
		if idleSlots != nil {
			score = idleSlots(m)
		}
		if i == 0 || score > bestScore {
			best, bestScore = m, score
		}
	}
	return best, true
}