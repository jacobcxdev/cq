package proxy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

const (
	// DefaultPort is the default proxy listen port.
	DefaultPort = 19280
	// DefaultUpstream is the default Claude API upstream.
	DefaultUpstream = "https://api.anthropic.com"
	// DefaultCodexUpstream is the default ChatGPT backend API upstream for Codex models.
	// ChatGPT OAuth tokens authenticate against this endpoint (not api.openai.com,
	// which requires the api.responses.write scope unavailable to OAuth clients).
	DefaultCodexUpstream = "https://chatgpt.com/backend-api/codex"
)

// Config holds proxy configuration persisted to disk.
type Config struct {
	Port           int    `json:"port"`
	ClaudeUpstream string `json:"claude_upstream"`
	CodexUpstream  string `json:"codex_upstream"`
	LocalToken     string `json:"local_token"`
	Headroom       bool   `json:"headroom,omitempty"`
	// HeadroomMode controls the compression strategy: "token" or "cache".
	// Omitted when empty so legacy configs without the field remain valid.
	// When omitted, cq defaults to cache mode. Explicit "token" preserves the
	// legacy token-optimised behaviour.
	HeadroomMode string `json:"headroom_mode,omitempty"`
	// PinnedClaudeAccount forces the proxy to route all Claude requests through
	// a specific account identified by email or AccountUUID. Omitted when empty.
	PinnedClaudeAccount string `json:"pinned_claude_account,omitempty"`
	DiagnosticsLog      string `json:"diagnostics_log,omitempty"`
}

// ResolvedHeadroomMode returns the effective HeadroomMode for this config.
// Explicit "token" maps to HeadroomModeToken; everything else (including an
// omitted headroom_mode and explicit "cache") maps to HeadroomModeCache.
func (c *Config) ResolvedHeadroomMode() HeadroomMode {
	if c.HeadroomMode == "token" {
		return HeadroomModeToken
	}
	return HeadroomModeCache
}

// HeadroomEnabled reports whether headroom compression should be started.
// It returns true when the legacy bool is set OR when an explicit headroom_mode
// is configured (non-empty), so that headroom_mode: "cache" alone is sufficient
// to enable compression without also requiring headroom: true.
func (c *Config) HeadroomEnabled() bool {
	return c.Headroom || c.HeadroomMode != ""
}

func (c *Config) setDefaults() {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.ClaudeUpstream == "" {
		c.ClaudeUpstream = DefaultUpstream
	}
	if c.CodexUpstream == "" {
		c.CodexUpstream = DefaultCodexUpstream
	}
}

func (c *Config) validate() error {
	if c.LocalToken == "" {
		return fmt.Errorf("local_token is required")
	}
	if _, err := url.Parse(c.ClaudeUpstream); err != nil {
		return fmt.Errorf("invalid claude_upstream URL: %w", err)
	}
	if _, err := url.Parse(c.CodexUpstream); err != nil {
		return fmt.Errorf("invalid codex_upstream URL: %w", err)
	}
	switch c.HeadroomMode {
	case "", "token", "cache":
		// valid
	default:
		return fmt.Errorf("invalid headroom_mode %q: must be \"token\" or \"cache\"", c.HeadroomMode)
	}
	return nil
}

// LoadConfig reads proxy config from disk, generating defaults on first run.
func LoadConfig() (*Config, error) {
	path := filepath.Join(configDir(), "proxy.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return generateDefaultConfig(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse proxy config: %w", err)
	}
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func generateDefaultConfig(path string) (*Config, error) {
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Port:           DefaultPort,
		ClaudeUpstream: DefaultUpstream,
		LocalToken:     token,
	}
	if err := saveConfig(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate proxy token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// SaveConfig writes cfg to the standard proxy config path atomically.
func SaveConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("proxy config is nil")
	}
	saved := *cfg
	saved.setDefaults()
	if err := saved.validate(); err != nil {
		return err
	}
	return saveConfig(filepath.Join(configDir(), "proxy.json"), &saved)
}

func saveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "cq")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "cq-config")
	}
	return filepath.Join(home, ".config", "cq")
}
