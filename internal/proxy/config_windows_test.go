package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRejectsWindowsVolumeRootStateDirectories(t *testing.T) {
	volumeRoot := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if filepath.VolumeName(volumeRoot) == "" {
		t.Fatalf("temporary directory has no Windows volume: %q", os.TempDir())
	}

	tests := map[string]func(*Config){
		"continuity": func(cfg *Config) { cfg.CodexContinuityStateDir = volumeRoot },
		"resilience": func(cfg *Config) { cfg.ProxyResilienceStateDir = volumeRoot },
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				LocalToken:              "token",
				ClaudeUpstream:          DefaultUpstream,
				CodexUpstream:           DefaultCodexUpstream,
				CodexLeaseRetentionDays: 7,
				CodexTurnRouting:        CodexRoutingOff,
				CodexWSTurnRouting:      CodexRoutingOff,
			}
			configure(&cfg)
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "must be a clean absolute non-root path") {
				t.Fatalf("validate accepted Windows volume root %q", volumeRoot)
			}
		})
	}
}
