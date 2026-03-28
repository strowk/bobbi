package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// SyncConfig holds synchronization settings from .bobbi/config.yaml.
type SyncConfig struct {
	Enabled           bool     `yaml:"enabled"`
	OwnerPrefix       string   `yaml:"owner_prefix"`
	StaleThreshold    string   `yaml:"stale_threshold"`
	HeartbeatInterval string   `yaml:"heartbeat_interval"`
	Agents            []string `yaml:"agents"`
}

// Config represents the full .bobbi/config.yaml file.
type Config struct {
	Sync SyncConfig `yaml:"sync"`
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
	return &cfg, nil
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
	valid := map[string]bool{"architecture": true, "solution": true, "evaluation": true, "review": true}
	for _, a := range c.Sync.Agents {
		if !valid[a] {
			return fmt.Errorf("sync.agents: invalid agent type %q (valid: architecture, solution, evaluation, review)", a)
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

// IsSyncedAgent returns true if the given agent repo directory name is in the sync.agents list.
func (c *Config) IsSyncedAgent(repoDir string) bool {
	if !c.Sync.Enabled {
		return false
	}
	for _, a := range c.Sync.Agents {
		if a == repoDir {
			return true
		}
	}
	return false
}
