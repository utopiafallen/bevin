// Package config holds Alcyona's runtime configuration.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Config is the runtime configuration for the alcyona server.
type Config struct {
	Addr         string // HTTP listen address
	DataDir      string // directory holding the SQLite state file
	ModelBaseURL string // OpenAI-compatible base URL (e.g. http://localhost:11434/v1)
	ModelAPIKey  string // API key; empty for local servers that don't need one
	ModelName    string // model identifier (e.g. "llama3.1", "gpt-4o-mini")
}

// Load builds a Config, expanding a leading "~" in dataDir.
func Load(addr, dataDir string) *Config {
	if strings.HasPrefix(dataDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dataDir = filepath.Join(home, strings.TrimPrefix(dataDir, "~"))
		}
	}
	return &Config{
		Addr:         addr,
		DataDir:      dataDir,
		ModelBaseURL: envOr("ALCYONA_MODEL_BASE_URL", "http://localhost:11434/v1"),
		ModelAPIKey:  os.Getenv("ALCYONA_MODEL_API_KEY"),
		ModelName:    envOr("ALCYONA_MODEL_NAME", "local-model"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
