package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML-friendly time.Duration ("30s", "10m", "1h").
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q (want e.g. 90s, 10m, 1h)", s)
	}
	*d = Duration(v)
	return nil
}

// D returns the standard library duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Filters are global download filters. Per-channel values override them when
// non-zero.
type Filters struct {
	// MaxFileSize skips videos larger than this many bytes (0 = no limit).
	MaxFileSize int64 `yaml:"max_file_size"`
	// MinDuration skips videos shorter than this many seconds (0 = no limit).
	MinDuration int `yaml:"min_duration"`
	// MaxDuration skips videos longer than this many seconds (0 = no limit).
	MaxDuration int `yaml:"max_duration"`
}

// Channel is one source channel.
type Channel struct {
	// Channel is "@username", "https://t.me/username" or a numeric id
	// ("-1001234567890"). Usernames are resolved on first run and cached.
	Channel string `yaml:"channel"`
	// Dir overrides the top-level folder name. Defaults to the channel
	// username/title.
	Dir     string `yaml:"dir"`
	Enabled *bool  `yaml:"enabled"`

	// Per-channel filter overrides.
	MaxFileSize int64 `yaml:"max_file_size"`
	MinDuration int   `yaml:"min_duration"`
	MaxDuration int   `yaml:"max_duration"`
	// Backfill overrides the global archive download flag.
	Backfill *bool `yaml:"backfill"`
}

// IsEnabled reports whether the channel should be processed.
func (c Channel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Config is the whole application configuration.
type Config struct {
	// Credentials. APIID and APIHash may also be supplied through the
	// TGDL_API_ID / TGDL_API_HASH environment variables, which win over
	// the YAML values. The phone number and 2FA password are never stored:
	// they are asked for interactively by `tgdl -cmd login`.
	APIID   int    `yaml:"api_id"`
	APIHash string `yaml:"api_hash"`

	// Runtime paths. Relative paths are resolved against the config file
	// directory, so the service can be started from anywhere.
	SessionFile string `yaml:"session_file"`
	StateFile   string `yaml:"state_file"`
	DownloadDir string `yaml:"download_dir"`

	// PollInterval is how often each channel is checked for new videos.
	PollInterval Duration `yaml:"poll_interval"`
	// Threads is the number of parallel chunk connections used for one file.
	Threads int `yaml:"threads"`
	// Retries is the number of attempts per message before it is skipped.
	Retries int `yaml:"retries"`
	// Backfill downloads the channel's existing archive on first run. When
	// false, only videos posted from now on are downloaded.
	Backfill *bool `yaml:"backfill"`
	// RedownloadMissing re-downloads a message when the file recorded in
	// state.db is gone. When false (default) a recorded message is never
	// downloaded again, so moving or deleting the media is safe.
	RedownloadMissing bool `yaml:"redownload_missing_files"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `yaml:"log_level"`

	Filters  Filters   `yaml:"filters"`
	Channels []Channel `yaml:"channels"`
}

func defaultConfig() Config {
	return Config{
		SessionFile:  "session.json",
		StateFile:    "state.db",
		DownloadDir:  "downloads",
		PollInterval: Duration(10 * time.Minute),
		Threads:      4,
		Retries:      5,
		Backfill:     boolPtr(true),
		LogLevel:     "info",
	}
}

// LoadConfig reads and validates a config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.finalize(path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Environment variables that override the API credentials from the config
// file. Keeping them out of config.yaml is useful when the file is readable by
// other users or checked into a dotfiles repo.
const (
	EnvAPIID   = "TGDL_API_ID"
	EnvAPIHash = "TGDL_API_HASH"
)

// applyEnv overrides the credentials with non-empty environment variables.
func (c *Config) applyEnv() error {
	if v := strings.TrimSpace(os.Getenv(EnvAPIID)); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil || id == 0 {
			return fmt.Errorf("%s: invalid api_id %q (want a positive integer)", EnvAPIID, v)
		}
		c.APIID = id
	}
	if v := strings.TrimSpace(os.Getenv(EnvAPIHash)); v != "" {
		c.APIHash = v
	}
	return nil
}

func (c *Config) finalize(path string) error {
	if c.APIID == 0 {
		return fmt.Errorf("api_id is required (set %s or api_id in the config; get it from https://my.telegram.org)", EnvAPIID)
	}
	if c.APIHash == "" {
		return fmt.Errorf("api_hash is required (set %s or api_hash in the config; get it from https://my.telegram.org)", EnvAPIHash)
	}
	if len(c.Channels) == 0 {
		return fmt.Errorf("channels: at least one channel is required")
	}
	for i, ch := range c.Channels {
		if strings.TrimSpace(ch.Channel) == "" {
			return fmt.Errorf("channels[%d].channel is empty", i)
		}
	}
	if c.Threads <= 0 {
		c.Threads = 4
	}
	if c.Threads > 16 {
		c.Threads = 16
	}
	if c.Retries <= 0 {
		c.Retries = 5
	}
	if c.PollInterval <= 0 {
		c.PollInterval = Duration(10 * time.Minute)
	}

	base := filepath.Dir(path)
	for _, p := range []*string{&c.SessionFile, &c.StateFile, &c.DownloadDir} {
		if *p == "" {
			continue
		}
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return nil
}

// BackfillEnabled reports whether the historical archive is downloaded.
func (c *Config) BackfillEnabled() bool {
	return c.Backfill == nil || *c.Backfill
}

// BackfillFor merges the channel-level flag over the global one.
func (c *Config) BackfillFor(ch Channel) bool {
	if ch.Backfill != nil {
		return *ch.Backfill
	}
	return c.BackfillEnabled()
}

func boolPtr(v bool) *bool { return &v }

// FilterFor merges channel-level filters over the global ones.
func (c *Config) FilterFor(ch Channel) Filters {
	f := c.Filters
	if ch.MaxFileSize != 0 {
		f.MaxFileSize = ch.MaxFileSize
	}
	if ch.MinDuration != 0 {
		f.MinDuration = ch.MinDuration
	}
	if ch.MaxDuration != 0 {
		f.MaxDuration = ch.MaxDuration
	}
	return f
}
