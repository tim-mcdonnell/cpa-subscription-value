// Package config parses the plugins.configs.cpa-subscription-value subtree
// that CPA hands us as YAML on plugin.register and plugin.reconfigure.
package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the plugin configuration with defaults applied.
type Config struct {
	// DataDir holds the SQLite database and any scratch files. It must be on a
	// mounted volume in Docker or it vanishes with the container.
	DataDir string `yaml:"data_dir"`
	// PollIntervalMinutes is how often the provider usage endpoints are polled
	// per account; 0 disables polling. They are rate-limited upstream.
	PollIntervalMinutes int `yaml:"poll_interval_minutes"`
	// RetentionDays bounds usage_events and meter_readings.
	RetentionDays int `yaml:"retention_days"`
	// ClaudeCacheWriteMultiplier prices Claude cache writes relative to input:
	// 1.25 for the 5-minute cache, 2.0 for the 1-hour cache. CPA does not tell
	// us which one a request used.
	ClaudeCacheWriteMultiplier float64 `yaml:"claude_cache_write_multiplier"`
	// PriceSyncHours is how often models.dev is re-fetched; 0 disables.
	PriceSyncHours int `yaml:"price_sync_hours"`
	// LogLevel for host.log messages we emit: debug|info.
	LogLevel string `yaml:"log_level"`
	// IngestBuffer is the channel depth between HandleUsage and the writer.
	IngestBuffer int `yaml:"ingest_buffer"`
}

// Defaults returns the baseline configuration.
func Defaults() Config {
	return Config{
		DataDir:                    "/CLIProxyAPI/data/cpa-subscription-value",
		PollIntervalMinutes:        20,
		RetentionDays:              365,
		ClaudeCacheWriteMultiplier: 1.25,
		PriceSyncHours:             24,
		LogLevel:                   "info",
		IngestBuffer:               4096,
	}
}

// Parse decodes YAML over the defaults and validates.
func Parse(raw []byte) (Config, error) {
	cfg := Defaults()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse plugin config: %w", err)
		}
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		cfg.DataDir = Defaults().DataDir
	}
	if cfg.PollIntervalMinutes < 0 {
		cfg.PollIntervalMinutes = 0
	}
	if cfg.PollIntervalMinutes > 0 && cfg.PollIntervalMinutes < 5 {
		cfg.PollIntervalMinutes = 5
	}
	if cfg.RetentionDays < 7 {
		cfg.RetentionDays = 7
	}
	if cfg.ClaudeCacheWriteMultiplier <= 0 {
		cfg.ClaudeCacheWriteMultiplier = Defaults().ClaudeCacheWriteMultiplier
	}
	if cfg.PriceSyncHours < 0 {
		cfg.PriceSyncHours = 0
	}
	if cfg.IngestBuffer < 64 {
		cfg.IngestBuffer = 64
	}
	switch strings.ToLower(strings.TrimSpace(cfg.LogLevel)) {
	case "debug", "info":
		cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	default:
		cfg.LogLevel = "info"
	}
	return cfg, nil
}

// PollInterval as a duration; zero when disabled.
func (c Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalMinutes) * time.Minute
}
