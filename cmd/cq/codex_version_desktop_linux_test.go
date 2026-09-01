//go:build linux

package main

import "testing"

func TestLinuxCodexRoutingBuildIgnoresMountedMacOSDesktopClient(t *testing.T) {
	if version, ok := probeCodexDesktopVersion(); ok || version != "" {
		t.Fatalf("Linux resolved macOS Desktop Codex build %q", version)
	}
}
