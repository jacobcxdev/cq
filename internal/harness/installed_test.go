package harness

import (
	"context"
	"os"
	"testing"
)

// The native inventory is read-only; this optional test never changes settings
// or starts inference. Ordinary runs use the bounded fake-command fixtures.
func TestHarnessInstalledPluginInventoryReadOnly(t *testing.T) {
	executable := os.Getenv("CQ_TEST_DESKTOP_PLUGIN_CLI")
	if executable == "" {
		t.Skip("set CQ_TEST_DESKTOP_PLUGIN_CLI for installed read-only inventory")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := (PluginCLI{Executable: executable}).Installed(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native installed/enabled inventory parsed: %d plugin IDs", len(ids))
}
