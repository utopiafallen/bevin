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

// TestNormalizeBaseURL — bare LAN hosts become canonical absolute URLs
// (http defaulted, /v1 appended when no path); explicit scheme/path preserved.
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"localhost.lan:1235", "http://localhost.lan:1235/v1"},
		{"127.0.0.1:8080/", "http://127.0.0.1:8080/v1"},
		{"https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1"},
		{"https://api.openai.com", "https://api.openai.com/v1"},
		{"http://llama:8080/v1", "http://llama:8080/v1"},
		{"  http://host:1/v1  ", "http://host:1/v1"},
		{"http://host:1/custom", "http://host:1/custom"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestValidateBaseURLSelfHeal — a config saved by an older build with a bare
// schemeless base (the "unsupported protocol scheme" bug) repairs itself on
// load, and an undialable base fails validation instead of the first request.
func TestValidateBaseURLSelfHeal(t *testing.T) {
	c := &ServerConfig{
		Auth:   Auth{Mode: "internal", SessionSecret: "x"},
		Models: []Model{{Provider: "llama", BaseURL: "localhost.lan:1235", ModelID: "ornith"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("self-heal: %v", err)
	}
	if got := c.Models[0].BaseURL; got != "http://localhost.lan:1235/v1" {
		t.Fatalf("canonical base = %q, want http://localhost.lan:1235/v1", got)
	}
	// Aux bases are healed the same way.
	a := &ServerConfig{
		Auth:   Auth{Mode: "internal", SessionSecret: "x"},
		Models: []Model{{Provider: "llama", BaseURL: "http://h:1/v1", ModelID: "m"}},
		Aux:    []AuxModel{{Kinds: []string{"vision"}, Provider: "llama", BaseURL: "127.0.0.1:8081", ModelID: "v"}},
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("aux self-heal: %v", err)
	}
	if got := a.Aux[0].BaseURL; got != "http://127.0.0.1:8081/v1" {
		t.Fatalf("aux canonical base = %q", got)
	}
	for _, bad := range []string{"ftp://host/v1", "not a url", "http://"} {
		b := &ServerConfig{
			Auth:   Auth{Mode: "internal", SessionSecret: "x"},
			Models: []Model{{Provider: "openai", BaseURL: bad, ModelID: "m"}},
		}
		if err := b.Validate(); err == nil {
			t.Errorf("malformed base %q accepted", bad)
		}
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
	// A model with no profile (pre-existing configs have no profile field)
	// self-heals to general on validate (the flat-v1 rule).
	if c.Models[0].Profile != "general" {
		t.Fatalf("profile = %q, want the general default (blank self-heals)", c.Models[0].Profile)
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

func TestValidateSandbox(t *testing.T) {
	base := func(s Sandbox) *ServerConfig {
		return &ServerConfig{
			Models:  []Model{{Provider: "openai", BaseURL: "u", ModelID: "m"}},
			Auth:    Auth{SessionSecret: "x"},
			Sandbox: s,
		}
	}
	// Enabled with the default network: no error, network filled in.
	c := base(Sandbox{Enabled: true})
	if err := c.Validate(); err != nil {
		t.Fatalf("valid sandbox: %v", err)
	}
	if c.Sandbox.Network != "veth" {
		t.Fatalf("network = %q, want the veth default", c.Sandbox.Network)
	}
	// Explicit private is accepted.
	if err := base(Sandbox{Enabled: true, Network: "private"}).Validate(); err != nil {
		t.Fatalf("private network: %v", err)
	}
	// A typo in the network mode must fail loudly (not silently default).
	if err := base(Sandbox{Enabled: true, Network: "nat"}).Validate(); err == nil {
		t.Fatal("expected error for network mode typo")
	}
	// Core/memory bounds.
	if err := base(Sandbox{Enabled: true, Cores: 129}).Validate(); err == nil {
		t.Fatal("expected error for cores > 128")
	}
	if err := base(Sandbox{Enabled: true, MemoryMB: -1}).Validate(); err == nil {
		t.Fatal("expected error for negative memory")
	}
	// Disk cap bounds (0 = no cap; 1-1048576 MiB).
	if err := base(Sandbox{Enabled: true, DiskMB: -1}).Validate(); err == nil {
		t.Fatal("expected error for a negative disk cap")
	}
	if err := base(Sandbox{Enabled: true, DiskMB: 1048577}).Validate(); err == nil {
		t.Fatal("expected error for a disk cap above the bound")
	}
	// Per-user containers are not optional: Validate canonicalizes
	// Enabled to true (a legacy enabled=false is turned on) and the
	// values are ALWAYS validated — a typo fails loudly either way.
	c2 := base(Sandbox{Enabled: false, Network: "nonsense"})
	if err := c2.Validate(); err == nil {
		t.Fatal("expected error for a network mode typo (the values are validated even when a legacy file carries enabled=false)")
	}
	if !c2.Sandbox.Enabled {
		t.Fatal("Validate must canonicalize Sandbox.Enabled to true (per-user containers are not optional)")
	}
}

// TestValidateNoProgress — the [no_progress] detection params are admin-tier:
// zero/missing fields fall back to the defaults (a partial section must not
// disable the detector), and explicit values are preserved.
func TestValidateNoProgress(t *testing.T) {
	base := func(n NoProgress) *ServerConfig {
		return &ServerConfig{
			Models:     []Model{{Provider: "openai", BaseURL: "u", ModelID: "m"}},
			Auth:       Auth{SessionSecret: "x"},
			NoProgress: n,
		}
	}
	// A missing/empty section -> all defaults (5 / 10 / 5).
	c := base(NoProgress{})
	if err := c.Validate(); err != nil {
		t.Fatalf("empty no_progress: %v", err)
	}
	if c.NoProgress.RepeatN != 5 || c.NoProgress.Window != 10 || c.NoProgress.StateK != 5 {
		t.Fatalf("defaults = %+v, want RepeatN=5 Window=10 StateK=5", c.NoProgress)
	}
	// A partial section: the set field is kept, the rest default.
	c2 := base(NoProgress{RepeatN: 8})
	if err := c2.Validate(); err != nil {
		t.Fatalf("partial no_progress: %v", err)
	}
	if c2.NoProgress.RepeatN != 8 || c2.NoProgress.Window != 10 || c2.NoProgress.StateK != 5 {
		t.Fatalf("partial = %+v, want RepeatN=8 Window=10 StateK=5", c2.NoProgress)
	}
	// A fully explicit section is preserved.
	c3 := base(NoProgress{RepeatN: 7, Window: 12, StateK: 3})
	if err := c3.Validate(); err != nil {
		t.Fatalf("explicit no_progress: %v", err)
	}
	if c3.NoProgress != (NoProgress{RepeatN: 7, Window: 12, StateK: 3}) {
		t.Fatalf("explicit = %+v, want unchanged", c3.NoProgress)
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
	// A config with no "version" key is at-or-below the current version:
	// accepted as-is, and stamped with the current version on save. (No
	// migration steps exist — user directive: no live data worth migrating.)
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
		t.Fatalf("load versionless config: %v", err)
	}
	if got.Version != 0 {
		t.Fatalf("in-memory version = %d, want 0 (nothing was migrated)", got.Version)
	}
	if err := Save(dir, got); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path(dir))
	if !strings.Contains(string(b), "version = "+strconv.Itoa(ConfigVersion)) {
		t.Fatalf("saved config missing version stamp:\n%s", b)
	}
	// A config newer than this build is refused (the version key must
	// precede any table header to be top-level — that is also where Save
	// writes it).
	if err := os.WriteFile(Path(dir), []byte("version = 999\n"+v0), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected an error for a config newer than the build")
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

// TestResolveProfileName pins the profile resolution contract: blank
// self-heals to general (no migration), a known name passes through
// (whitespace trimmed), and an UNKNOWN name is a loud error (a typo'd
// profile silently changing a model's harness behavior must never happen).
func TestResolveProfileName(t *testing.T) {
	got, err := ResolveProfileName("models[0]", "")
	if err != nil {
		t.Fatalf("blank: %v", err)
	}
	if got != "general" {
		t.Fatalf("blank = %q, want general (self-heal)", got)
	}
	got, err = ResolveProfileName("models[0]", "  general  ")
	if err != nil {
		t.Fatalf("known: %v", err)
	}
	if got != "general" {
		t.Fatalf("whitespace-trimmed = %q, want general", got)
	}
	if _, err := ResolveProfileName("models[0]", "no-such-profile"); err == nil {
		t.Fatal("unknown profile accepted (must fail loudly)")
	}
}

// TestProfileRoundTrip pins that the profile field survives save+load (the
// toml + json tags) and that an unknown profile in a saved file is refused at
// LOAD (not just at save) — the config file is the commit of record.
func TestProfileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &ServerConfig{
		Auth:   Auth{Mode: "internal", SessionSecret: "x"},
		Models: []Model{testModel()},
	}
	cfg.Models[0].Profile = "general"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Models[0].Profile != "general" {
		t.Fatalf("round-trip profile = %q, want general", got.Models[0].Profile)
	}
	// An aux model's profile round-trips too (the assignment surface is
	// uniform between main + aux).
	cfg.Aux = []AuxModel{{Kinds: []string{"vision"}, Provider: "llama", BaseURL: "http://127.0.0.1:8081/v1", ModelID: "v"}}
	cfg.Aux[0].Profile = "general"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Aux[0].Profile != "general" {
		t.Fatalf("aux round-trip profile = %q, want general", got.Aux[0].Profile)
	}
	// A hand-edited config with an unknown profile must be refused at load.
	bad, _ := os.ReadFile(Path(dir))
	bad = []byte(strings.Replace(string(bad), "profile = \"general\"", "profile = \"bogus\"", 1))
	if err := os.WriteFile(Path(dir), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("load accepted an unknown profile (must fail loudly)")
	}
}

// TestValidateTools — [tools.web_search]: enabled ⇒ the known provider
// ("exa") + a non-blank API key are required (loud errors); a disabled
// entry is valid as-is (inert — the stale provider/key get re-validated on
// re-enable), which is what makes the admin's disable action a safe
// no-clobber.
func TestValidateTools(t *testing.T) {
	base := func(w WebSearch) *ServerConfig {
		return &ServerConfig{
			Models: []Model{testModel()},
			Auth:   Auth{SessionSecret: "x"},
			Tools:  Tools{WebSearch: w},
		}
	}
	// Disabled (the default for a pre-[tools] config): valid as-is.
	if err := base(WebSearch{}).Validate(); err != nil {
		t.Fatalf("disabled (default): %v", err)
	}
	// Enabled + exa + key: valid.
	if err := base(WebSearch{Enabled: true, Provider: "exa", APIKey: "exa-123"}).Validate(); err != nil {
		t.Fatalf("a valid enabled entry: %v", err)
	}
	// Enabled with an unknown (typo) provider: loud.
	if err := base(WebSearch{Enabled: true, Provider: "brave", APIKey: "k"}).Validate(); err == nil {
		t.Fatal("an unknown provider while enabled must be rejected")
	}
	// Enabled with a blank provider: loud.
	if err := base(WebSearch{Enabled: true, APIKey: "k"}).Validate(); err == nil {
		t.Fatal("enabled with a blank provider must be rejected")
	}
	// Enabled without a key: loud.
	if err := base(WebSearch{Enabled: true, Provider: "exa"}).Validate(); err == nil {
		t.Fatal("enabled without an API key must be rejected")
	}
	// Disabled with stale values: valid (inert).
	if err := base(WebSearch{Enabled: false, Provider: "exa", APIKey: "exa-old"}).Validate(); err != nil {
		t.Fatalf("a disabled entry with stale values: %v", err)
	}
	// The provider is trimmed before validation.
	c := base(WebSearch{Enabled: true, Provider: "  exa  ", APIKey: "k"})
	if err := c.Validate(); err != nil {
		t.Fatalf("a whitespace-padded provider: %v", err)
	}
	if c.Tools.WebSearch.Provider != "exa" {
		t.Fatalf("provider = %q, want the trimmed exa", c.Tools.WebSearch.Provider)
	}
}

func TestPushValidate(t *testing.T) {
	valid := func() *ServerConfig {
		return &ServerConfig{Models: []Model{{Provider: "openai", BaseURL: "u", ModelID: "m"}}, Auth: Auth{SessionSecret: "x"}}
	}
	// Web push: both VAPID keys or neither (a half pair is a broken setup).
	c := valid()
	c.Push = Push{VAPIDPublic: "pub-only"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "VAPID keys must be set together") {
		t.Fatalf("public without private: err = %v", err)
	}
	c = valid()
	c.Push = Push{VAPIDPrivate: "priv-only"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "VAPID keys must be set together") {
		t.Fatalf("private without public: err = %v", err)
	}
	c = valid()
	c.Push = Push{}
	if err := c.Validate(); err != nil {
		t.Fatalf("no keys (push off): %v", err)
	}
	c = valid()
	c.Push = Push{VAPIDPublic: "pub", VAPIDPrivate: "priv"}
	if err := c.Validate(); err != nil {
		t.Fatalf("both keys: %v", err)
	}
}
