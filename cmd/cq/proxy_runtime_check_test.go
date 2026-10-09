package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/installer"
)

func TestRuntimeCheckDoesNotCreateConfigurationOrCoordinator(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	var output bytes.Buffer
	if err := writeProxyRuntimeCheck(&output); err != nil {
		t.Fatal(err)
	}
	var check installer.RuntimeCheckV1
	if err := json.Unmarshal(output.Bytes(), &check); err != nil {
		t.Fatal(err)
	}
	if check.ProtocolVersion != installer.RuntimeUpgradeProtocolVersion {
		t.Fatal("protocol mismatch")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("preflight created state: %v", entries)
	}
}
func TestRuntimeCheckRejectsInvalidConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	dir := filepath.Join(root, "config", "cq")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeProxyRuntimeCheck(&output); err == nil {
		t.Fatal("invalid configuration accepted")
	}
}
