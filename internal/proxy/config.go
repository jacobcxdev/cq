package proxy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type CodexWindowPrimingConfig struct {
	Enabled        bool              `json:"enabled"`
	ModelOverrides map[string]string `json:"model_overrides,omitempty"`
}

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
	// PayloadDiagnosticsLog is the optional path to a JSONL file for payload
	// diagnostics. When set, the proxy logs request body metadata (including raw
	// request bodies) for every buffered request. Disabled by default.
	// WARNING: this log contains raw request bodies including prompts, tool
	// inputs, system prompts, compact summaries, and message content. Do not
	// share without review. Requires a proxy restart to take effect.
	PayloadDiagnosticsLog string `json:"payload_diagnostics_log,omitempty"`
	// CodexTurnRouting and CodexWSTurnRouting apply only after proxy restart.
	CodexTurnRouting              CodexRoutingMode         `json:"codex_turn_routing"`
	CodexWSTurnRouting            CodexRoutingMode         `json:"codex_ws_turn_routing"`
	CodexRoutingDefaultAccountKey codex.AccountKey         `json:"codex_routing_default_account_key,omitempty"`
	CodexRoutingAccountKeys       []codex.AccountKey       `json:"codex_routing_account_keys,omitempty"`
	CodexLeaseRetentionDays       int                      `json:"codex_lease_retention_days"`
	CodexContinuityStateDir       string                   `json:"codex_continuity_state_dir,omitempty"`
	CodexWindowPriming            CodexWindowPrimingConfig `json:"codex_window_priming,omitempty"`

	unknownFields map[string]json.RawMessage
}

var configKnownFields = map[string]bool{
	"port": true, "claude_upstream": true, "codex_upstream": true,
	"local_token": true, "headroom": true, "headroom_mode": true,
	"pinned_claude_account": true, "diagnostics_log": true,
	"payload_diagnostics_log": true, "codex_turn_routing": true,
	"codex_ws_turn_routing": true, "codex_lease_retention_days": true,
	"codex_routing_default_account_key": true,
	"codex_routing_account_keys":        true,
	"codex_continuity_state_dir":        true,
	"codex_window_priming":              true,
}

// UnmarshalJSON retains fields unknown to this build for N/N-1 safe writes.
func (c *Config) UnmarshalJSON(data []byte) error {
	type wireConfig Config
	var known wireConfig
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	unknown := make(map[string]json.RawMessage)
	for key, value := range raw {
		if !configKnownFields[key] {
			unknown[key] = append(json.RawMessage(nil), value...)
		}
	}
	*c = Config(known)
	c.unknownFields = unknown
	return nil
}

// MarshalJSON merges preserved future fields with fields known to this build.
func (c Config) MarshalJSON() ([]byte, error) {
	type wireConfig Config
	knownData, err := json.Marshal(wireConfig(c))
	if err != nil {
		return nil, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(knownData, &merged); err != nil {
		return nil, err
	}
	for key, value := range c.unknownFields {
		if !configKnownFields[key] {
			merged[key] = append(json.RawMessage(nil), value...)
		}
	}
	return json.Marshal(merged)
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

// ResolvedCodexContinuityStateDir returns the process-owned continuity state directory.
func (c *Config) ResolvedCodexContinuityStateDir() string {
	if c != nil && c.CodexContinuityStateDir != "" {
		return c.CodexContinuityStateDir
	}
	return configDir()
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
	if c.CodexTurnRouting == "" {
		c.CodexTurnRouting = CodexRoutingOff
	}
	if c.CodexWSTurnRouting == "" {
		c.CodexWSTurnRouting = CodexRoutingOff
	}
	if c.CodexLeaseRetentionDays == 0 {
		c.CodexLeaseRetentionDays = 7
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
	if err := c.CodexTurnRouting.validate("codex_turn_routing"); err != nil {
		return err
	}
	if err := c.CodexWSTurnRouting.validate("codex_ws_turn_routing"); err != nil {
		return err
	}
	if c.CodexLeaseRetentionDays < 1 || c.CodexLeaseRetentionDays > 365 {
		return fmt.Errorf("invalid codex_lease_retention_days %d: must be between 1 and 365", c.CodexLeaseRetentionDays)
	}
	if c.CodexContinuityStateDir != "" {
		clean := filepath.Clean(c.CodexContinuityStateDir)
		if !filepath.IsAbs(c.CodexContinuityStateDir) || clean != c.CodexContinuityStateDir || clean == string(filepath.Separator) {
			return fmt.Errorf("invalid codex_continuity_state_dir %q: must be a clean absolute non-root path", c.CodexContinuityStateDir)
		}
	}
	seenRoutingAccounts := make(map[codex.AccountKey]bool, len(c.CodexRoutingAccountKeys))
	for _, accountKey := range c.CodexRoutingAccountKeys {
		if accountKey == "" || seenRoutingAccounts[accountKey] {
			return errors.New("invalid codex_routing_account_keys: keys must be non-empty and unique")
		}
		seenRoutingAccounts[accountKey] = true
	}
	if len(seenRoutingAccounts) != 0 && c.CodexRoutingDefaultAccountKey != "" && !seenRoutingAccounts[c.CodexRoutingDefaultAccountKey] {
		return errors.New("invalid codex_routing_default_account_key: account is not allowed for routing")
	}
	for scope, modelID := range c.CodexWindowPriming.ModelOverrides {
		if strings.TrimSpace(scope) == "" || strings.TrimSpace(modelID) == "" {
			return fmt.Errorf("invalid Codex window priming model override %q", scope)
		}
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
		Port:                    DefaultPort,
		ClaudeUpstream:          DefaultUpstream,
		CodexUpstream:           DefaultCodexUpstream,
		LocalToken:              token,
		CodexTurnRouting:        CodexRoutingOff,
		CodexWSTurnRouting:      CodexRoutingOff,
		CodexLeaseRetentionDays: 7,
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
