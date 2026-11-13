package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// AgentConfig is a user's per-user preferences (agent.toml, in the user's
// directory). The user picks a default main model; further preferences are
// added here as the agent matures.
type AgentConfig struct {
	DefaultModelID string `toml:"default_model_id"`

	// OnboardingComplete marks the first-time "Meet Bevin" flow as done
	// (OnboardingAt records when). Regular users only — the admin has no user
	// UI.
	OnboardingComplete bool   `toml:"onboarding_complete"`
	OnboardingAt       string `toml:"onboarding_at,omitempty"`
}

// DefaultAgent returns valid zero-value preferences.
func DefaultAgent() *AgentConfig { return &AgentConfig{} }

// LoadAgent reads agent.toml; a missing file yields defaults.
func LoadAgent(userDir string) (*AgentConfig, error) {
	b, err := os.ReadFile(AgentPath(userDir))
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultAgent(), nil
		}
		return nil, err
	}
	var a AgentConfig
	if err := toml.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("parse %s: %w", AgentFileName, err)
	}
	return &a, nil
}

// SaveAgent writes agent.toml atomically.
func SaveAgent(userDir string, a *AgentConfig) error {
	if err := os.MkdirAll(filepath.Clean(userDir), 0o755); err != nil {
		return err
	}
	b, err := toml.Marshal(a)
	if err != nil {
		return err
	}
	tmp := AgentPath(userDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, AgentPath(userDir))
}