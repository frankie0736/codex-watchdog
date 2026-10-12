// Package config loads and validates watchdog configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type HomeConfig struct {
	Path string `toml:"path"`
}

type Config struct {
	PollIntervalSeconds        int          `toml:"poll_interval_seconds"`
	CapacityErrorGraceSeconds  int          `toml:"capacity_error_grace_seconds"`
	CapacityErrorMaxAgeSeconds int          `toml:"capacity_error_max_age_seconds"`
	ActiveMaxAgeSeconds        int          `toml:"active_max_age_seconds"`
	GlobalMaxSendsPerMinute    int          `toml:"global_max_sends_per_minute"`
	PerThreadCooldownSeconds   int          `toml:"per_thread_cooldown_seconds"`
	QueueRetrySeconds          int          `toml:"queue_retry_seconds"`
	DeliveryTimeoutSeconds     int          `toml:"delivery_timeout_seconds"`
	MaxAttemptsPerTurn         int          `toml:"max_attempts_per_turn"`
	QueueTimeoutSeconds        int          `toml:"queue_timeout_seconds"`
	StartupRecoverRecentErrors bool         `toml:"startup_recover_recent_errors"`
	RequireAttached            bool         `toml:"require_attached"`
	DryRun                     bool         `toml:"dry_run"`
	Message                    string       `toml:"message"`
	CodexBin                   string       `toml:"codex_bin"`
	AllowedSources             []string     `toml:"allowed_sources"`
	AutoDiscoverHomes          bool         `toml:"auto_discover_homes"`
	CodexHomes                 []HomeConfig `toml:"codex_home"`
	StateDir                   string       `toml:"state_dir"`
}

// Default returns defaults: live sending on, interactive sources only.
func Default() Config {
	return Config{
		PollIntervalSeconds:        5,
		CapacityErrorGraceSeconds:  30,
		CapacityErrorMaxAgeSeconds: 3600,
		ActiveMaxAgeSeconds:        6 * 3600,
		GlobalMaxSendsPerMinute:    5,
		PerThreadCooldownSeconds:   300,
		QueueRetrySeconds:          120,
		DeliveryTimeoutSeconds:     300,
		MaxAttemptsPerTurn:         3,
		QueueTimeoutSeconds:        30,
		StartupRecoverRecentErrors: true,
		RequireAttached:            true,
		DryRun:                     false,
		Message:                    "继续",
		CodexBin:                   "codex",
		AllowedSources:             []string{"cli", "vscode"},
		AutoDiscoverHomes:          true,
		StateDir:                   "~/.local/state/codex-watchdog",
	}
}

func DefaultPath() string {
	return ExpandHome("~/.config/codex-watchdog/config.toml")
}

// Load reads path on top of defaults. A missing file at the default path is not an error.
func Load(path string, explicit bool) (Config, error) {
	cfg := Default()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return cfg, cfg.Validate()
		}
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return cfg, fmt.Errorf("config %s: unknown keys %v", path, undecoded)
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	positive := map[string]int{
		"poll_interval_seconds":          c.PollIntervalSeconds,
		"capacity_error_max_age_seconds": c.CapacityErrorMaxAgeSeconds,
		"active_max_age_seconds":         c.ActiveMaxAgeSeconds,
		"global_max_sends_per_minute":    c.GlobalMaxSendsPerMinute,
		"delivery_timeout_seconds":       c.DeliveryTimeoutSeconds,
		"max_attempts_per_turn":          c.MaxAttemptsPerTurn,
		"queue_timeout_seconds":          c.QueueTimeoutSeconds,
	}
	for k, v := range positive {
		if v <= 0 {
			return fmt.Errorf("%s must be > 0, got %d", k, v)
		}
	}
	nonNegative := map[string]int{
		"capacity_error_grace_seconds": c.CapacityErrorGraceSeconds,
		"per_thread_cooldown_seconds":  c.PerThreadCooldownSeconds,
		"queue_retry_seconds":          c.QueueRetrySeconds,
	}
	for k, v := range nonNegative {
		if v < 0 {
			return fmt.Errorf("%s must be >= 0, got %d", k, v)
		}
	}
	if strings.TrimSpace(c.Message) == "" {
		return errors.New("message must not be empty")
	}
	if c.CodexBin == "" {
		return errors.New("codex_bin must not be empty")
	}
	if !c.AutoDiscoverHomes && len(c.CodexHomes) == 0 {
		return errors.New("no codex_home configured and auto_discover_homes is false")
	}
	if c.StateDir == "" {
		return errors.New("state_dir must not be empty")
	}
	return nil
}

func (c Config) PollInterval() time.Duration { return seconds(c.PollIntervalSeconds) }
func (c Config) Grace() time.Duration        { return seconds(c.CapacityErrorGraceSeconds) }
func (c Config) ErrorMaxAge() time.Duration  { return seconds(c.CapacityErrorMaxAgeSeconds) }
func (c Config) ActiveMaxAge() time.Duration { return seconds(c.ActiveMaxAgeSeconds) }
func (c Config) Cooldown() time.Duration     { return seconds(c.PerThreadCooldownSeconds) }
func (c Config) QueueRetry() time.Duration   { return seconds(c.QueueRetrySeconds) }
func (c Config) DeliveryTimeout() time.Duration {
	return seconds(c.DeliveryTimeoutSeconds)
}
func (c Config) QueueTimeout() time.Duration { return seconds(c.QueueTimeoutSeconds) }

func (c Config) SourceAllowed(source string) bool {
	for _, s := range c.AllowedSources {
		if strings.EqualFold(s, source) {
			return true
		}
	}
	return false
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
