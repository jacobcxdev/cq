//go:build linux

package proxy

import "testing"

func TestLinuxNamespaceConfigRejectsUnboundedAuthority(t *testing.T) {
	root := t.TempDir()
	valid := linuxNamespaceConfig{
		Version:    linuxNamespaceProtocolVersion,
		Executable: "/usr/bin/codex",
		Args:       []string{"--version"},
		WriteRoot:  root,
		Relays: []linuxRelayDefinition{{
			ID:     linuxRelayProxy,
			Port:   19431,
			Target: "127.0.0.1:19431",
		}},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	tests := map[string]func(*linuxNamespaceConfig){
		"version":             func(config *linuxNamespaceConfig) { config.Version++ },
		"relative root":       func(config *linuxNamespaceConfig) { config.WriteRoot = "relative" },
		"root authority":      func(config *linuxNamespaceConfig) { config.WriteRoot = "/" },
		"unknown relay":       func(config *linuxNamespaceConfig) { config.Relays[0].ID = 99 },
		"non-loopback target": func(config *linuxNamespaceConfig) { config.Relays[0].Target = "1.1.1.1:443" },
		"port mismatch":       func(config *linuxNamespaceConfig) { config.Relays[0].Port++ },
		"duplicate relay":     func(config *linuxNamespaceConfig) { config.Relays = append(config.Relays, config.Relays[0]) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			config.Args = append([]string(nil), valid.Args...)
			config.Relays = append([]linuxRelayDefinition(nil), valid.Relays...)
			mutate(&config)
			if err := config.validate(); err == nil {
				t.Fatal("accepted invalid namespace authority")
			}
		})
	}
}
