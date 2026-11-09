package config

import (
	"os"
	"path/filepath"
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
		Aux:      []AuxModel{{Kind: "vision", Provider: "llama", BaseURL: "http://127.0.0.1:8081/v1", ModelID: "v"}},
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
	if got.Aux[0].Kind != "vision" || got.Aux[0].ModelID != "v" {
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
	got, err := CleanPath("~/alcyona")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "alcyona"); got != want {
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