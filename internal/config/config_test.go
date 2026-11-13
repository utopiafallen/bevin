package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testModel() Model {
	return Model{
		ID: "m1", Provider: "openai", BaseURL: "https://api.openai.com/v1",
		APIKey: "sk-x", ModelID: "ornith", DisplayName: "Ornith",
		MaxContext: 200000, Slots: 2, Enabled: true,
		Metadata: Metadata{Reasoning: 8, Knowledge: 7, Speed: 6},
	}
}

func TestServerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &ServerConfig{
		Bind:     "0.0.0.0",
		Logging:  Logging{Level: "info"},
		Auth:     Auth{Mode: "internal", SessionSecret: "deadbeef", SessionTTLSeconds: 2592000},
		Models:   []Model{testModel()},
		Aux:      []AuxModel{{Kinds: []string{"vision"}, Provider: "llama", BaseURL: "http://127.0.0.1:8081/v1", ModelID: "v"}},
		Embedder: Embedder{BaseURL: "http://127.0.0.1:8080/v1", ModelID: "embeddinggemma-2"},
		Sidecars: Sidecars{Qdrant: "http://127.0.0.1:6334", LlamaServer: "http://127.0.0.1:8080", FFmpeg: "ffmpeg"},
	}
	if Exists(dir) {
		t.Fatal("Exists should be false before save")
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if !Exists(dir) {
		t.Fatal("Exists should be true after save")
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := got.Models[0]
	if m.ID != "m1" || m.MaxContext != 200000 || m.Metadata.Reasoning != 8 || m.Slots != 2 {
		t.Fatalf("model = %+v", m)
	}
	if got.Auth.SessionSecret != "deadbeef" {
		t.Fatalf("secret = %q", got.Auth.SessionSecret)
	}
	if len(got.Aux[0].Kinds) != 1 || got.Aux[0].Kinds[0] != "vision" || got.Aux[0].ModelID != "v" {
		t.Fatalf("aux = %+v", got.Aux[0])
	}
	if got.Embedder.ModelID != "embeddinggemma-2" {
		t.Fatalf("embedder = %+v", got.Embedder)
	}
	if got.MaxWorkspaceSize != DefaultMaxWorkspaceSize {
		t.Fatalf("max_workspace_size defaulted to %d", got.MaxWorkspaceSize)
	}
}

func TestSave0600(t *testing.T) {
	dir := t.TempDir()
	cfg := &ServerConfig{Auth: Auth{SessionSecret: "x"}, Models: []Model{testModel()}}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("server.toml perms = %o (want 0600)", got)
	}
}

func TestValidate(t *testing.T) {
	c := &ServerConfig{Models: []Model{{Provider: "openai", BaseURL: "u", ModelID: "m"}}, Auth: Auth{SessionSecret: "x"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if c.Models[0].Slots != 1 {
		t.Fatalf("slots defaulted = %d", c.Models[0].Slots)
	}
	if c.Bind != "0.0.0.0" {
		t.Fatalf("bind defaulted = %q", c.Bind)
	}

	bad := &ServerConfig{Models: []Model{{Provider: "openai", BaseURL: "u", ModelID: "m"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for missing session secret")
	}
	nom := &ServerConfig{Auth: Auth{SessionSecret: "x"}}
	if err := nom.Validate(); err == nil {
		t.Fatal("expected error for no models")
	}
	// provider/base_url/model_id required
	inc := &ServerConfig{Models: []Model{{Provider: "openai", BaseURL: "u"}}, Auth: Auth{SessionSecret: "x"}}
	if err := inc.Validate(); err == nil {
		t.Fatal("expected error for incomplete model")
	}
}

func TestCleanPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := CleanPath("~/bevin")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "bevin"); got != want {
		t.Fatalf("CleanPath = %q want %q", got, want)
	}
	rel, err := CleanPath("sub/dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(rel) {
		t.Fatalf("not absolute: %q", rel)
	}
	if got, _ := CleanPath(""); got != "" {
		t.Fatalf("empty should stay empty, got %q", got)
	}
}

func TestDefaultDataDir(t *testing.T) {
	d, err := DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if d == "" || !filepath.IsAbs(d) {
		t.Fatalf("data dir not absolute: %q", d)
	}
}

func TestGenerateSessionSecret(t *testing.T) {
	s1, err := GenerateSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := GenerateSessionSecret()
	if len(s1) != 64 {
		t.Fatalf("secret len = %d (want 64 hex chars)", len(s1))
	}
	if s1 == s2 {
		t.Fatal("secrets should differ")
	}
}

func TestAgentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// missing file -> defaults
	a, err := LoadAgent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.DefaultModelID != "" {
		t.Fatalf("default model id = %q", a.DefaultModelID)
	}
	a.DefaultModelID = "m1"
	if err := SaveAgent(dir, a); err != nil {
		t.Fatal(err)
	}
	b, err := LoadAgent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.DefaultModelID != "m1" {
		t.Fatalf("round-trip default model = %q", b.DefaultModelID)
	}
}

func TestConfigVersioning(t *testing.T) {
	dir := t.TempDir()
	// A v0 config (no "version" field) is migrated forward to the current
	// version on load.
	v0 := `[auth]
mode = "internal"
session_secret = "deadbeef"

[[models]]
id = "m1"
provider = "openai"
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
model_id = "ornith"
display_name = "Ornith"
max_context = 200000
slots = 2
enabled = true
`
	if err := os.WriteFile(Path(dir), []byte(v0), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load v0 config: %v", err)
	}
	if got.Version != ConfigVersion {
		t.Fatalf("migrated version = %d, want %d", got.Version, ConfigVersion)
	}
	// Saving a migrated config stamps the current version on disk.
	if err := Save(dir, got); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path(dir))
	if !strings.Contains(string(b), "version = "+strconv.Itoa(ConfigVersion)) {
		t.Fatalf("saved config missing version stamp:\n%s", b)
	}
	// A config newer than this build is refused. (The version key must precede
	// any table header to be top-level — that is also where Save writes it.)
	if err := os.WriteFile(Path(dir), []byte("version = 999\n"+v0), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected an error for a config newer than the build")
	}
}

func TestAuxKindToKindsMigration(t *testing.T) {
	dir := t.TempDir()
	// A v1 config with the deprecated aux "kind" (a single modality) is
	// migrated to "kinds" (one or more) on load.
	v1 := `version = 1

[auth]
mode = "internal"
session_secret = "deadbeef"

[[models]]
id = "m1"
provider = "openai"
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
model_id = "ornith"

[[aux]]
kind = "vision"
provider = "llama"
base_url = "http://127.0.0.1:8081/v1"
model_id = "v"
`
	if err := os.WriteFile(Path(dir), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load v1 config: %v", err)
	}
	if got.Version != ConfigVersion {
		t.Fatalf("migrated version = %d, want %d", got.Version, ConfigVersion)
	}
	if len(got.Aux) != 1 {
		t.Fatalf("aux count = %d, want 1", len(got.Aux))
	}
	if len(got.Aux[0].Kinds) != 1 || got.Aux[0].Kinds[0] != "vision" {
		t.Fatalf("migrated aux kinds = %v, want [vision]", got.Aux[0].Kinds)
	}
}

// TestBaseURIMigration — the public base URI has moved through two schema
// versions: v3 stored an admin-entered [auth.oauth] redirect_uri, v4 stored
// [auth.oauth] base_uri, v5 keeps it at the server level (top-level
// base_uri). A v3 file migrates v3->v4->v5 in one load: the base is
// recovered from the legacy redirect when it ends in the callback path.
// A v4 file migrates v4->v5: the base lifts out of [auth.oauth] as-is.
func TestBaseURIMigration(t *testing.T) {
	const base = "https://bevin.example.com"
	const redirect = base + "/auth/oauth/callback"

	// v3: the base is recovered from the legacy redirect_uri.
	v3 := `version = 3

[auth]
mode = "internal"
session_secret = "deadbeef"

[auth.oauth]
enabled = true
name = "TestIDP"
client_id = "app-1"
client_secret = "cs-1"
auth_url = "https://idp.example.com/o/authorize/"
token_url = "https://idp.example.com/o/token/"
userinfo_url = "https://idp.example.com/o/user/"
scopes = "openid profile email"
redirect_uri = "` + redirect + `"

[[models]]
id = "m1"
provider = "openai"
base_url = "https://api.openai.com/v1"
api_key = "sk-test"
model_id = "ornith"
max_context = 200000
slots = 2
enabled = true
`
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(v3), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load v3 config: %v", err)
	}
	if got.Version != ConfigVersion {
		t.Fatalf("migrated version = %d, want %d", got.Version, ConfigVersion)
	}
	if got.BaseURI != base {
		t.Fatalf("migrated base_uri = %q, want %s", got.BaseURI, base)
	}
	if got.OAuthRedirectURI() != redirect {
		t.Fatalf("derived redirect = %q, want %s", got.OAuthRedirectURI(), redirect)
	}
	// Saving the migrated config keeps the base at the server level and
	// drops both legacy keys.
	if err := Save(dir, got); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path(dir))
	if !strings.Contains(string(b), "base_uri = \""+base+"\"") {
		t.Fatalf("saved config missing top-level base_uri:\n%s", b)
	}
	if strings.Contains(string(b), "redirect_uri") {
		t.Fatalf("saved config must not carry the legacy redirect key:\n%s", b)
	}
	if strings.Count(string(b), "base_uri") != 1 {
		t.Fatalf("saved config must carry exactly one base_uri (top-level):\n%s", b)
	}

	// v4: the base lifts out of [auth.oauth] as-is.
	v4 := strings.Replace(v3, "version = 3", "version = 4", 1)
	v4 = strings.Replace(v4, "redirect_uri = \""+redirect+"\"", "base_uri = \""+base+"\"", 1)
	if err := os.WriteFile(Path(dir), []byte(v4), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = Load(dir)
	if err != nil {
		t.Fatalf("load v4 config: %v", err)
	}
	if got.BaseURI != base || got.OAuthRedirectURI() != redirect {
		t.Fatalf("v4 migration: base = %q, redirect = %q", got.BaseURI, got.OAuthRedirectURI())
	}

	// A v3 redirect that doesn't end in the callback path is not
	// decomposable — with OAuth enabled the load now FAILS validation
	// (enabled OAuth needs a base) instead of silently starting with no
	// redirect; the admin fixes it by setting the base.
	v3b := strings.Replace(v3, "redirect_uri = \""+redirect+"\"",
		"redirect_uri = \"https://somewhere.else/elsewhere\"", 1)
	if err := os.WriteFile(Path(dir), []byte(v3b), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected a validation error for an unreconstructable base with OAuth enabled")
	}
}

// TestPublicURLDerivation — every system-generated link is the public base +
// a server-relative path (trailing slashes normalized, subpaths preserved);
// a blank base derives nothing.
func TestPublicURLDerivation(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://bevin.example.com", "/auth/oauth/callback", "https://bevin.example.com/auth/oauth/callback"},
		{"https://bevin.example.com/", "/auth/oauth/callback", "https://bevin.example.com/auth/oauth/callback"},
		{"https://bevin.example.com:8443", "/auth/oauth/callback", "https://bevin.example.com:8443/auth/oauth/callback"},
		{"https://host.example/bevin", "/auth/oauth/callback", "https://host.example/bevin/auth/oauth/callback"},
		{"https://host.example/bevin///", "/auth/oauth/callback", "https://host.example/bevin/auth/oauth/callback"},
		{"https://bevin.example.com", "/password-reset?h=alice", "https://bevin.example.com/password-reset?h=alice"},
		{"", "/auth/oauth/callback", ""},
	}
	for _, c := range cases {
		cfg := &ServerConfig{BaseURI: c.base}
		if got := cfg.PublicURL(c.path); got != c.want {
			t.Errorf("PublicURL(%q) with base %q = %q, want %q", c.path, c.base, got, c.want)
		}
	}
	// The OAuth redirect is exactly PublicURL(the callback path).
	cfg := &ServerConfig{BaseURI: "https://bevin.example.com"}
	if got, want := cfg.OAuthRedirectURI(), "https://bevin.example.com/auth/oauth/callback"; got != want {
		t.Errorf("OAuthRedirectURI = %q, want %q", got, want)
	}
}

// TestBaseURIValidation — an optional base must be a well-formed absolute
// http(s) URL when set, and is REQUIRED when OAuth is enabled.
func TestBaseURIValidation(t *testing.T) {
	mk := func(base string, oauthEnabled bool) *ServerConfig {
		o := OAuth{}
		if oauthEnabled {
			o = OAuth{
				Enabled: true, Name: "TestIDP", ClientID: "app-1",
				AuthURL: "https://idp.example.com/o/authorize/", TokenURL: "https://idp.example.com/o/token/",
				UserinfoURL: "https://idp.example.com/o/user/", Scopes: "openid profile email",
			}
		}
		return &ServerConfig{
			BaseURI: base,
			Auth:    Auth{Mode: "internal", SessionSecret: "s", OAuth: o},
			Models:  []Model{testModel()},
		}
	}
	if err := mk("https://bevin.example.com", false).Validate(); err != nil {
		t.Fatalf("valid base, oauth off: %v", err)
	}
	if err := mk("", false).Validate(); err != nil {
		t.Fatalf("blank base, oauth off (local-only): %v", err)
	}
	if err := mk("https://bevin.example.com", true).Validate(); err != nil {
		t.Fatalf("valid base, oauth on: %v", err)
	}
	if err := mk("", true).Validate(); err == nil {
		t.Fatal("oauth enabled without a base must be rejected")
	}
	for _, bad := range []string{"bevin.example.com", "/bevin", "ftp://x/", "not a url"} {
		if err := mk(bad, false).Validate(); err == nil {
			t.Errorf("malformed base %q accepted", bad)
		}
	}
}

func TestResolveDisplayName(t *testing.T) {
	models := []Model{
		{ID: "a", Provider: "openai", BaseURL: "u", ModelID: "m", DisplayName: "Fast", MaxContext: 8000, Slots: 1, Enabled: true},
		{ID: "b", Provider: "openai", BaseURL: "u", ModelID: "m", DisplayName: "Fast", MaxContext: 8000, Slots: 4, Enabled: true},
		{ID: "c", Provider: "openai", BaseURL: "u", ModelID: "m", DisplayName: "Fast", MaxContext: 8000, Slots: 2, Enabled: false},
	}
	// No availability callback -> fall back to total slot capacity (b has 4).
	got, ok := ResolveDisplayName(models, "Fast", nil)
	if !ok || got.ID != "b" {
		t.Fatalf("fallback = %+v ok=%v, want b", got, ok)
	}
	// With an availability callback reporting idle slots, the idle model wins
	// even though it has fewer total slots.
	idle := map[string]int{"a": 1, "b": 0}
	got2, _ := ResolveDisplayName(models, "Fast", func(m Model) int { return idle[m.ID] })
	if got2.ID != "a" {
		t.Fatalf("idle = %q, want a (b is busy)", got2.ID)
	}
	// A display name with only disabled models -> not found.
	if _, ok := ResolveDisplayName([]Model{{ID: "z", DisplayName: "Off", Enabled: false}}, "Off", nil); ok {
		t.Fatal("disabled-only display name should not resolve")
	}
	// Unknown display name -> not found.
	if _, ok := ResolveDisplayName(models, "Nope", nil); ok {
		t.Fatal("unknown display name should not resolve")
	}
}

func TestModelAvailable(t *testing.T) {
	cases := []struct {
		name string
		m    Model
		want bool
	}{
		{"enabled + not auto-disabled", Model{Enabled: true}, true},
		{"enabled + auto-disabled", Model{Enabled: true, AutoDisabled: true}, false},
		{"disabled + not auto-disabled", Model{Enabled: false}, false},
		{"disabled + auto-disabled", Model{Enabled: false, AutoDisabled: true}, false},
	}
	for _, c := range cases {
		if got := c.m.Available(); got != c.want {
			t.Errorf("%s: Available() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEmailConfigured(t *testing.T) {
	if (Email{}).Configured() {
		t.Fatal("empty email should not be configured")
	}
	if (Email{Enabled: true}).Configured() {
		t.Fatal("enabled but no host should not be configured")
	}
	if (Email{SMTPHost: "smtp.example.com"}).Configured() {
		t.Fatal("host but not enabled should not be configured")
	}
	if !(Email{Enabled: true, SMTPHost: "smtp.example.com"}.Configured()) {
		t.Fatal("enabled + host should be configured")
	}
}

func TestEmailRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &ServerConfig{
		Auth:   Auth{Mode: "internal", SessionSecret: "x"},
		Models: []Model{testModel()},
		Email: Email{
			Enabled: true, SMTPHost: "smtp.example.com", SMTPPort: 587,
			Username: "bevin@example.com", Password: "pw",
			FromName: "Bevin", UseStartTLS: true,
		},
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email.SMTPHost != "smtp.example.com" || got.Email.SMTPPort != 587 {
		t.Fatalf("email = %+v", got.Email)
	}
	if got.Email.FromAddress != "bevin@example.com" {
		t.Fatalf("from_address defaulted to %q (want the username)", got.Email.FromAddress)
	}
	if !got.Email.Configured() {
		t.Fatal("round-tripped email should be configured")
	}
}

func TestResolveDisplayNameAutoDisabled(t *testing.T) {
	models := []Model{
		{ID: "a", Provider: "openai", BaseURL: "u", ModelID: "m", DisplayName: "Fast", MaxContext: 8000, Slots: 1, Enabled: true},
		{ID: "b", Provider: "openai", BaseURL: "u", ModelID: "m", DisplayName: "Fast", MaxContext: 8000, Slots: 4, Enabled: true, AutoDisabled: true},
	}
	// b is auto-disabled, so a (the only available one) wins even though it has
	// fewer slots.
	got, ok := ResolveDisplayName(models, "Fast", nil)
	if !ok || got.ID != "a" {
		t.Fatalf("got = %+v ok=%v, want a (b auto-disabled)", got, ok)
	}
	// A display name with only auto-disabled models -> not found.
	if _, ok := ResolveDisplayName([]Model{{ID: "z", DisplayName: "Off", Enabled: true, AutoDisabled: true}}, "Off", nil); ok {
		t.Fatal("auto-disabled-only display name should not resolve")
	}
}
