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
	"net/url"
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
const ConfigVersion = 5

// DefaultSessionTTL is 30 days.
const DefaultSessionTTLSeconds = 30 * 24 * 3600

// DefaultMaxWorkspaceSize is 5 GiB.
const DefaultMaxWorkspaceSize = int64(5 * 1024 * 1024 * 1024)

// ServerConfig is the admin-tier configuration (server.toml).
type ServerConfig struct {
	Version int    `toml:"version"`
	DataDir string `toml:"data_dir"`
	Bind    string `toml:"bind"`
	// BaseURI is the server's PUBLIC base (scheme + host[:port] + optional
	// subpath — it sits behind the reverse proxy's TLS, so https). This is
	// the one address the system can't derive itself, so the admin sets it;
	// every link the system generates for the outside world is derived from
	// it (OAuth redirect via OAuthRedirectURI, and password-reset /
	// notification / share links via PublicURL as they land). Optional:
	// blank is fine for a local-only setup, but required when OAuth is
	// enabled (Validate enforces it).
	BaseURI          string     `toml:"base_uri" json:"base_uri"`
	Logging          Logging    `toml:"logging"`
	Auth             Auth       `toml:"auth"`
	Models           []Model    `toml:"models"`
	Aux              []AuxModel `toml:"aux"`
	Embedder         Embedder   `toml:"embedder" json:"embedder"`
	Sidecars         Sidecars   `toml:"sidecars" json:"sidecars"`
	Email            Email      `toml:"email" json:"email"`
	MaxWorkspaceSize int64      `toml:"max_workspace_size"`
	// Pre-v5 OAuth legacy values, captured by Load from the raw file before
	// migrate() folds them into BaseURI (unexported: never persisted).
	// v3 stored an admin-entered [auth.oauth] redirect_uri; v4 stored
	// [auth.oauth] base_uri.
	legacyOAuthRedirect string
	legacyOAuthBase     string
	// pendingBase carries the recovered base through the v3->v4->v5 chain.
	pendingBase string
}

// PublicURL derives an absolute public link for a server-relative path
// ("/password-reset", "/share/x") from the public base: trailing slashes on
// the base are trimmed, the path is appended verbatim (callers pass paths
// with a leading slash). Blank base -> blank (the caller decides what to do
// — typically "no link can be generated").
func (c *ServerConfig) PublicURL(path string) string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURI), "/")
	if base == "" {
		return ""
	}
	return base + path
}

type Logging struct {
	Level string `toml:"level"` // debug | info | warn | error
}

type Auth struct {
	Mode              string `toml:"mode"` // "internal" — the server is always its own IdP; OAuth is an additive second IdP (see OAuth.Enabled)
	SessionSecret     string `toml:"session_secret"`
	SessionTTLSeconds int    `toml:"session_ttl_seconds"`
	OAuth             OAuth  `toml:"oauth"`
}

// OAuth is the external-IdP tier (authorization-code flow), e.g. authentik or
// any OIDC provider. The server ALWAYS keeps its own IdP (internal accounts)
// in parallel; when Enabled, users enabled in the provider are auto-provisioned
// on first login (no explicit admin add needed). Claims are read from a
// user-info endpoint (OIDC-style); the mapping is sub → identity,
// preferred_username / email local part / sub → handle, name → display name.
type OAuth struct {
	Enabled      bool   `toml:"enabled" json:"enabled"`
	Name         string `toml:"name" json:"name"` // shown on the sign-in button
	ClientID     string `toml:"client_id" json:"client_id"`
	ClientSecret string `toml:"client_secret" json:"client_secret"`
	AuthURL      string `toml:"auth_url" json:"auth_url"`
	TokenURL     string `toml:"token_url" json:"token_url"`
	UserinfoURL  string `toml:"userinfo_url" json:"userinfo_url"`
	Scopes       string `toml:"scopes" json:"scopes"` // default "openid profile email"
}

// OAuthCallbackPath is the route the OAuth callback lands on.
const OAuthCallbackPath = "/auth/oauth/callback"

// OAuthRedirectURI derives the OAuth redirect URI from the server's public
// base: base + /auth/oauth/callback. The admin pastes this into the
// provider's app registration. The base lives at the SERVER level (see
// ServerConfig.BaseURI) — it is the public address of the whole instance,
// and the OAuth redirect is just one derived link. Blank base -> blank
// (an enabled provider with a blank base is rejected by Validate).
func (c *ServerConfig) OAuthRedirectURI() string {
	return c.PublicURL(OAuthCallbackPath)
}

// Model is a main text model in the admin catalog. A model is a provider
// endpoint (Provider + BaseURL + APIKey) plus the API model name (ModelID).
type Model struct {
	ID          string `toml:"id" json:"id"`
	Provider    string `toml:"provider" json:"provider"` // openai | anthropic | llama
	BaseURL     string `toml:"base_url" json:"base_url"`
	APIKey      string `toml:"api_key" json:"api_key"`
	ModelID     string `toml:"model_id" json:"model_id"`
	DisplayName string `toml:"display_name" json:"display_name"`
	MaxContext  int    `toml:"max_context" json:"max_context"`
	Slots       int    `toml:"slots" json:"slots"`
	Enabled     bool   `toml:"enabled" json:"enabled"`
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

// AuxModel is an admin-set perception model, hidden from the user and used
// mid-conversation to comprehend one or more non-text assets. A single model may
// perceive multiple modalities (e.g. a VLM that takes both image + audio), so
// Kinds is a set, not a single value.
type AuxModel struct {
	Kinds    []string `toml:"kinds" json:"kinds"` // vision/audio/video — one or more
	Provider string   `toml:"provider" json:"provider"`
	BaseURL  string   `toml:"base_url" json:"base_url"`
	APIKey   string   `toml:"api_key" json:"api_key"`
	ModelID  string   `toml:"model_id" json:"model_id"`
	// Kind is the deprecated v1 field (a single modality). It exists only so a
	// v1 config's value survives unmarshal; migrate() folds it into Kinds.
	Kind string `toml:"kind,omitempty" json:"kind,omitempty"`
}

// Embedder is the local llama-server serving /v1/embeddings (embeddinggemma-2).
// It is server-managed: the embedder IS the llama-server sidecar, so the server
// derives its base URL (loopback) from the llama-server endpoint at setup. It is
// not user-configured in the wizard.
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

// Email is the server-managed SMTP configuration used to send email
// notifications to users + admin. It is optional: when SMTPHost is empty (or
// Enabled is false) the EmailSender is nil and no emails are sent. The password
// is a secret (server.toml is written 0600). This is the server-level notifier;
// a per-user email configuration for the agent's (future) email tool is a
// later revision.
type Email struct {
	Enabled     bool   `toml:"enabled" json:"enabled"`
	SMTPHost    string `toml:"smtp_host" json:"smtp_host"`
	SMTPPort    int    `toml:"smtp_port" json:"smtp_port"`
	Username    string `toml:"username" json:"username"`
	Password    string `toml:"password" json:"password"`
	FromAddress string `toml:"from_address" json:"from_address"`
	FromName    string `toml:"from_name" json:"from_name"`
	UseStartTLS bool   `toml:"use_starttls" json:"use_starttls"`
}

// Configured reports whether email sending is configured (enabled + a host is
// set). When false, the harness's EmailSender is nil and Send is a no-op.
func (e Email) Configured() bool { return e.Enabled && strings.TrimSpace(e.SMTPHost) != "" }

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
	// Pre-v5 OAuth values lived under [auth.oauth] in two shapes (v3: an
	// admin-entered redirect_uri; v4: a base_uri); the v5 struct keeps the
	// base at the server level and drops the fields, so the unmarshal above
	// discarded them. Re-read just those keys from the raw file for
	// migrate() to fold into BaseURI.
	if cfg.Version < 5 {
		var legacy struct {
			Auth struct {
				OAuth struct {
					RedirectURI string `toml:"redirect_uri"`
					BaseURI     string `toml:"base_uri"`
				} `toml:"oauth"`
			} `toml:"auth"`
		}
		if err := toml.Unmarshal(b, &legacy); err == nil {
			cfg.legacyOAuthRedirect = legacy.Auth.OAuth.RedirectURI
			cfg.legacyOAuthBase = legacy.Auth.OAuth.BaseURI
		}
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
		case 1:
			// v1 -> v2: aux "kind" (a single modality) became "kinds" (one or
			// more). Fold any v1 aux Kind into Kinds.
			for i := range c.Aux {
				if len(c.Aux[i].Kinds) == 0 && strings.TrimSpace(c.Aux[i].Kind) != "" {
					c.Aux[i].Kinds = []string{c.Aux[i].Kind}
				}
			}
			c.Version = 2
		case 2:
			// v2 -> v3: the schema gained the optional [email] section (the
			// server-managed SMTP notifier). A v2 config simply has an empty
			// Email (email disabled) — no field transform, just stamp the
			// version.
			c.Version = 3
		case 3:
			// v3 -> v4: the OAuth redirect_uri (admin-entered) became a value
			// derived from the public base — bevin knows its own callback
			// route, so only the base is configured. Recover the base from the
			// legacy redirect when it ends in the callback path; a
			// non-decomposable legacy value leaves the base empty for the
			// admin to re-enter. (The base still lives under [auth.oauth] in
			// v4 — the server-level move is the next step.)
			if legacy := strings.TrimSpace(c.legacyOAuthRedirect); strings.HasSuffix(legacy, OAuthCallbackPath) {
				c.pendingBase = strings.TrimSuffix(legacy, OAuthCallbackPath)
			}
			c.Version = 4
		case 4:
			// v4 -> v5: the OAuth base_uri moved from [auth.oauth] to the
			// server level (top-level base_uri) — it is the public address of
			// the whole instance, and every system-generated link (OAuth
			// redirect, password reset, ...) derives from it.
			if legacy := strings.TrimSpace(c.legacyOAuthBase); legacy != "" {
				c.pendingBase = legacy
			}
			c.BaseURI = c.pendingBase
			c.Version = 5
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
	// Public base URI: optional (a local-only setup generates no outside
	// links) but must be a well-formed absolute http(s) URL when set, and is
	// REQUIRED when OAuth is enabled (the redirect is derived from it).
	c.BaseURI = strings.TrimSpace(c.BaseURI)
	if c.BaseURI != "" {
		bu, err := url.Parse(c.BaseURI)
		if err != nil || (bu.Scheme != "http" && bu.Scheme != "https") || bu.Host == "" {
			return fmt.Errorf("base_uri: must be an absolute http(s) URL, e.g. https://bevin.example.com (got %q)", c.BaseURI)
		}
	}
	if c.Auth.OAuth.Enabled && c.BaseURI == "" {
		return fmt.Errorf("base_uri: a public base URI is required when OAuth is enabled (the OAuth redirect is derived from it — set it in the wizard's server step or the admin's Server page)")
	}
	// Email defaults (applied when the notifier is configured).
	if c.Email.Enabled {
		if c.Email.SMTPPort <= 0 {
			c.Email.SMTPPort = 587 // standard SMTP submission port
		}
		if c.Email.FromName == "" {
			c.Email.FromName = "bevin"
		}
		if c.Email.FromAddress == "" {
			c.Email.FromAddress = c.Email.Username
		}
	}
	// Internal model IDs are what everything references (slot pools,
	// conversation active model, per-user defaults) — fill any missing ones
	// (also repairs pre-existing files saved before IDs were derived).
	assignModelIDs(c.Models)
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

// ModelIDSlug derives a model's internal ID from its provider model id:
// lowercased, non-alphanumerics collapsed to single dashes, capped at 40
// chars. The internal ID is what every other piece references (conversation
// active model, per-user default, slot pools, depends_on), so it must never
// be empty.
func ModelIDSlug(modelID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(modelID) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ' || r == '/':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "model"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// assignModelIDs fills any missing internal model IDs, deterministically
// derived from model_id with a collision suffix (m, m-2, m-3, ...) so two
// entries can never collapse onto one slot pool.
func assignModelIDs(models []Model) {
	seen := map[string]bool{}
	for i := range models {
		if models[i].ID != "" {
			seen[models[i].ID] = true
			continue
		}
		base := ModelIDSlug(models[i].ModelID)
		id, n := base, 2
		for seen[id] {
			id = fmt.Sprintf("%s-%d", base, n)
			n++
		}
		models[i].ID = id
		seen[id] = true
	}
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
