package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyStatusRejectsWindowsVolumeRootContinuityDirectory(t *testing.T) {
	volumeRoot := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if filepath.VolumeName(volumeRoot) == "" {
		t.Fatalf("temporary directory has no Windows volume: %q", os.TempDir())
	}
	cfg := proxy.Config{
		LocalToken:              "token",
		ClaudeUpstream:          proxy.DefaultUpstream,
		CodexUpstream:           proxy.DefaultCodexUpstream,
		CodexLeaseRetentionDays: 7,
		CodexContinuityStateDir: volumeRoot,
		CodexTurnRouting:        proxy.CodexRoutingOff,
		CodexWSTurnRouting:      proxy.CodexRoutingOff,
	}
	if err := validateProxyStatusConfig(&cfg); err == nil || !strings.Contains(err.Error(), "must be a clean absolute non-root path") {
		t.Fatalf("validateProxyStatusConfig accepted Windows volume root %q", volumeRoot)
	}
}
