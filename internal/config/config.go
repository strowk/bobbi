package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentEnabledConfig holds the enabled flag for a single agent type.
type AgentEnabledConfig struct {
	Enabled *bool `yaml:"enabled"`
}

// AgentsConfig holds per-agent configuration (only evaluator and reviewer can be disabled).
type AgentsConfig struct {
	Evaluator *AgentEnabledConfig `yaml:"evaluator"`
	Reviewer  *AgentEnabledConfig `yaml:"reviewer"`
}

// GitHubCIConfig holds GitHub-specific Green CI settings.
type GitHubCIConfig struct {
	RequiredChecks []string `yaml:"required_checks"`
}

// GitLabCIConfig holds GitLab-specific Green CI settings.
type GitLabCIConfig struct {
	RequiredJobs []string `yaml:"required_jobs"`
}

// GreenCIConfig holds Green CI settings for a single agent type.
type GreenCIConfig struct {
	Enabled     bool            `yaml:"enabled"`
	TrunkBranch string          `yaml:"trunk_branch"`
	GitHub      *GitHubCIConfig `yaml:"github"`
	GitLab      *GitLabCIConfig `yaml:"gitlab"`
}

// SyncConfig holds synchronization settings from .bobbi/config.yaml.
type SyncConfig struct {
	Enabled           bool                      `yaml:"enabled"`
	OwnerPrefix       string                    `yaml:"owner_prefix"`
	StaleThreshold    string                    `yaml:"stale_threshold"`
	HeartbeatInterval string                    `yaml:"heartbeat_interval"`
	Agents            []string                  `yaml:"agents"`
	GreenCI           map[string]*GreenCIConfig `yaml:"green_ci"`
}

// Config represents the full .bobbi/config.yaml file.
type Config struct {
	Agents AgentsConfig `yaml:"agents"`
	Sync   SyncConfig   `yaml:"sync"`
}

// Load reads and parses .bobbi/config.yaml from the given base directory.
// Returns a zero Config if the file doesn't exist.
func Load(baseDir string) (*Config, error) {
	path := filepath.Join(baseDir, ".bobbi", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Validate unknown keys under "agents" section
	if err := validateAgentsKeys(data); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validateAgentsKeys checks that the agents section only contains known keys (evaluator, reviewer).
func validateAgentsKeys(data []byte) error {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil // already handled by main unmarshal
	}
	agentsRaw, ok := raw["agents"]
	if !ok || agentsRaw == nil {
		return nil
	}
	agentsMap, ok := agentsRaw.(map[string]interface{})
	if !ok {
		return nil
	}
	validKeys := map[string]bool{"evaluator": true, "reviewer": true}
	for key := range agentsMap {
		if !validKeys[key] {
			return fmt.Errorf("agents: unknown agent key %q (valid: evaluator, reviewer)", key)
		}
	}
	return nil
}

// Validate checks that required fields are present when sync is enabled.
func (c *Config) Validate() error {
	if !c.Sync.Enabled {
		return nil
	}
	if c.Sync.OwnerPrefix == "" {
		return fmt.Errorf("sync.owner_prefix is required when sync.enabled is true")
	}
	if len(c.Sync.Agents) == 0 {
		return fmt.Errorf("sync.agents is required and must be non-empty when sync.enabled is true")
	}
	// Validate agent names
	valid := map[string]bool{"architect": true, "solver": true, "evaluator": true, "reviewer": true}
	for _, a := range c.Sync.Agents {
		if !valid[a] {
			return fmt.Errorf("sync.agents: invalid agent type %q (valid: architect, solver, evaluator, reviewer)", a)
		}
	}
	// Validate Green CI settings
	for agentName, gci := range c.Sync.GreenCI {
		if gci == nil || !gci.Enabled {
			continue
		}
		// Agent must be a valid agent name
		if !valid[agentName] {
			return fmt.Errorf("sync.green_ci.%s: not a valid agent type (valid: architect, solver, evaluator, reviewer)", agentName)
		}
		// Agent must be in sync.agents
		inSyncAgents := false
		for _, a := range c.Sync.Agents {
			if a == agentName {
				inSyncAgents = true
				break
			}
		}
		if !inSyncAgents {
			return fmt.Errorf("sync.green_ci.%s: agent is not in sync.agents", agentName)
		}
		// Exactly one provider key must be present
		hasGitHub := gci.GitHub != nil
		hasGitLab := gci.GitLab != nil
		if !hasGitHub && !hasGitLab {
			return fmt.Errorf("sync.green_ci.%s: exactly one provider key (github or gitlab) is required when green_ci is enabled", agentName)
		}
		if hasGitHub && hasGitLab {
			return fmt.Errorf("sync.green_ci.%s: exactly one provider key (github or gitlab) must be present, found both", agentName)
		}
		// Validate provider-specific required list
		if hasGitHub {
			if len(gci.GitHub.RequiredChecks) == 0 {
				return fmt.Errorf("sync.green_ci.%s: github.required_checks must be present and non-empty when green_ci is enabled", agentName)
			}
		}
		if hasGitLab {
			if len(gci.GitLab.RequiredJobs) == 0 {
				return fmt.Errorf("sync.green_ci.%s: gitlab.required_jobs must be present and non-empty when green_ci is enabled", agentName)
			}
		}
	}

	// Validate durations if provided
	if c.Sync.StaleThreshold != "" {
		if _, err := time.ParseDuration(c.Sync.StaleThreshold); err != nil {
			return fmt.Errorf("sync.stale_threshold: %w", err)
		}
	}
	if c.Sync.HeartbeatInterval != "" {
		if _, err := time.ParseDuration(c.Sync.HeartbeatInterval); err != nil {
			return fmt.Errorf("sync.heartbeat_interval: %w", err)
		}
	}
	return nil
}

// GetStaleThreshold returns the parsed stale threshold duration (default 5m).
func (c *Config) GetStaleThreshold() time.Duration {
	if c.Sync.StaleThreshold == "" {
		return 5 * time.Minute
	}
	d, _ := time.ParseDuration(c.Sync.StaleThreshold)
	return d
}

// GetHeartbeatInterval returns the parsed heartbeat interval duration (default 2m).
func (c *Config) GetHeartbeatInterval() time.Duration {
	if c.Sync.HeartbeatInterval == "" {
		return 2 * time.Minute
	}
	d, _ := time.ParseDuration(c.Sync.HeartbeatInterval)
	return d
}

// IsAgentEnabled returns true if the given agent type is enabled.
// Architect and solver are always enabled. Evaluator and reviewer default to true.
func (c *Config) IsAgentEnabled(agentType string) bool {
	switch agentType {
	case "evaluator":
		if c.Agents.Evaluator != nil && c.Agents.Evaluator.Enabled != nil {
			return *c.Agents.Evaluator.Enabled
		}
		return true
	case "reviewer":
		if c.Agents.Reviewer != nil && c.Agents.Reviewer.Enabled != nil {
			return *c.Agents.Reviewer.Enabled
		}
		return true
	default:
		return true // architect and solver are always enabled
	}
}

// IsSyncedAgent returns true if the given agent type name is in the sync.agents list.
func (c *Config) IsSyncedAgent(agentType string) bool {
	if !c.Sync.Enabled {
		return false
	}
	for _, a := range c.Sync.Agents {
		if a == agentType {
			return true
		}
	}
	return false
}

// IsGreenCI returns true if Green CI is enabled for the given agent type.
func (c *Config) IsGreenCI(agentType string) bool {
	if !c.Sync.Enabled || c.Sync.GreenCI == nil {
		return false
	}
	gci, ok := c.Sync.GreenCI[agentType]
	return ok && gci != nil && gci.Enabled
}

// GetGreenCIConfig returns the Green CI config for the given agent type, or nil.
func (c *Config) GetGreenCIConfig(agentType string) *GreenCIConfig {
	if c.Sync.GreenCI == nil {
		return nil
	}
	return c.Sync.GreenCI[agentType]
}

// GetTrunkBranch returns the trunk branch for the given agent's Green CI config (default "main").
func (c *Config) GetTrunkBranch(agentType string) string {
	gci := c.GetGreenCIConfig(agentType)
	if gci == nil || gci.TrunkBranch == "" {
		return "main"
	}
	return gci.TrunkBranch
}

// GreenCIProvider returns "github" or "gitlab" for the given agent's Green CI config.
func (c *Config) GreenCIProvider(agentType string) string {
	gci := c.GetGreenCIConfig(agentType)
	if gci == nil {
		return ""
	}
	if gci.GitHub != nil {
		return "github"
	}
	if gci.GitLab != nil {
		return "gitlab"
	}
	return ""
}
