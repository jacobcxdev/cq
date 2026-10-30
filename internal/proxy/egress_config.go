package proxy

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// LoadCodexEgressConfig reads only the policy needed by the shared egress
// owner. Unrelated normal-runtime settings must not disable rescue traffic.
// Missing, malformed, or invalid egress policy still fails closed.
func LoadCodexEgressConfig() (*Config, error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return nil, errors.New("resolve Codex egress configuration failed")
	}
	return loadCodexEgressConfig(fsutil.OSFileSystem{}, paths.ConfigFile)
}

func loadCodexEgressConfig(fs fsutil.FileSystem, path string) (*Config, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, errors.New("read Codex egress configuration failed")
	}
	defer clear(data)
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("invalid Codex egress configuration: expected an object")
	}
	var fields struct {
		Proxy     json.RawMessage `json:"codex_egress_proxy"`
		Profile   json.RawMessage `json:"codex_wireguard_profile"`
		Interface json.RawMessage `json:"codex_egress_interface"`
		Origin    json.RawMessage `json:"codex_upstream"`
	}
	if json.Unmarshal(data, &fields) != nil {
		return nil, errors.New("parse Codex egress configuration failed")
	}
	cfg := &Config{}
	for _, field := range []struct {
		raw   json.RawMessage
		value *string
	}{
		{fields.Proxy, &cfg.CodexEgressProxy}, {fields.Profile, &cfg.CodexWireGuardProfile},
		{fields.Interface, &cfg.CodexEgressInterface}, {fields.Origin, &cfg.CodexUpstream},
	} {
		if field.raw != nil && (bytes.Equal(bytes.TrimSpace(field.raw), []byte("null")) || json.Unmarshal(field.raw, field.value) != nil) {
			return nil, errors.New("invalid Codex egress configuration: expected string fields")
		}
	}
	if err := ValidateCodexEgressSettings(cfg.CodexEgressProxy, cfg.CodexWireGuardProfile); err != nil {
		return nil, err
	}
	if err := validateCodexEgressInterface(cfg.CodexWireGuardProfile, cfg.CodexEgressInterface); err != nil {
		return nil, err
	}
	if cfg.CodexUpstream == "" {
		cfg.CodexUpstream = DefaultCodexUpstream
	}
	return cfg, nil
}
