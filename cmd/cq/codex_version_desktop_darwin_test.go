//go:build darwin

package main

import (
	"slices"
	"testing"
)

func TestProbeCodexDesktopVersionPaths(t *testing.T) {
	const current = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
	const legacyChatGPT = "/Applications/ChatGPT.app/Contents/Resources/codex"
	const legacyCodex = "/Applications/Codex.app/Contents/Resources/codex"
	tests := []struct {
		name      string
		available map[string]string
		want      string
		wantPaths []string
	}{
		{
			name:      "current Desktop wins over legacy installs",
			available: map[string]string{current: "0.159.0", legacyChatGPT: "0.158.0", legacyCodex: "0.157.0"},
			want:      "0.159.0",
			wantPaths: []string{current},
		},
		{
			name:      "legacy ChatGPT fallback",
			available: map[string]string{legacyChatGPT: "0.158.0"},
			want:      "0.158.0",
			wantPaths: []string{current, legacyChatGPT},
		},
		{
			name:      "legacy Codex fallback",
			available: map[string]string{legacyCodex: "0.157.0"},
			want:      "0.157.0",
			wantPaths: []string{current, legacyChatGPT, legacyCodex},
		},
		{
			name:      "all probes fail",
			wantPaths: []string{current, legacyChatGPT, legacyCodex},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			got, ok := probeCodexDesktopVersionWith(func(path string) (string, bool) {
				paths = append(paths, path)
				version, found := tt.available[path]
				return version, found
			})
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("probe = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.want != "")
			}
			if !slices.Equal(paths, tt.wantPaths) {
				t.Fatalf("probed paths = %v, want %v", paths, tt.wantPaths)
			}
		})
	}
}
