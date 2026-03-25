package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistTemplate(t *testing.T) {
	var buf bytes.Buffer
	data := plistData{
		Label:    "dev.jacobcx.cq.refresh",
		Binary:   "/opt/homebrew/bin/cq",
		Interval: 1800,
		LogPath:  "/Users/test/Library/Logs/cq/refresh.log",
	}
	if err := plistTemplate.Execute(&buf, data); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"<string>dev.jacobcx.cq.refresh</string>",
		"<string>/opt/homebrew/bin/cq</string>",
		"<string>refresh</string>",
		"<integer>1800</integer>",
		"<true/>",
		"<string>Background</string>",
		"<string>/Users/test/Library/Logs/cq/refresh.log</string>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestAgentPlistPath(t *testing.T) {
	path, err := agentPlistPath()
	if err != nil {
		t.Fatalf("agentPlistPath: %v", err)
	}
	if filepath.Base(path) != agentLabel+".plist" {
		t.Errorf("base = %q, want %q", filepath.Base(path), agentLabel+".plist")
	}
	if !strings.Contains(path, "LaunchAgents") {
		t.Errorf("path %q missing LaunchAgents component", path)
	}
}

func TestAgentLogPath(t *testing.T) {
	path, err := agentLogPath()
	if err != nil {
		t.Fatalf("agentLogPath: %v", err)
	}
	if filepath.Base(path) != "refresh.log" {
		t.Errorf("base = %q, want refresh.log", filepath.Base(path))
	}
	if !strings.Contains(path, filepath.Join("Logs", "cq")) {
		t.Errorf("path %q missing Logs/cq component", path)
	}
}

func TestUninstallAgentNoOp(t *testing.T) {
	// When no plist exists, uninstall should succeed silently.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Ensure LaunchAgents dir exists but no plist.
	if err := os.MkdirAll(filepath.Join(dir, "Library", "LaunchAgents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := uninstallAgent(); err != nil {
		t.Errorf("uninstallAgent with no plist: %v", err)
	}
}
