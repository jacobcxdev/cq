//go:build windows

package main

import (
	"io"
	"strings"
	"testing"
)

func TestCodexHarnessWindowsStdioAcceptsPackagedArguments(t *testing.T) {
	for _, test := range []struct {
		name, stock string
		args        []string
		want        bool
	}{
		{"desktop", "relative.exe", []string{"-c", "features.code_mode_host=true", "app-server", "--analytics-default-enabled"}, true},
		{"plain stdio", "relative.exe", []string{"app-server", "--stdio"}, true},
		{"normal CQ", "relative.exe", []string{"codex", "harness", "status"}, false},
		{"config value only", "relative.exe", []string{"-c", "app-server"}, false},
		{"no bridge marker", "", []string{"app-server"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(windowsHarnessStockCLIEnv, test.stock)
			handled, err := runWindowsHarnessStdioEntry(test.args, strings.NewReader(""), io.Discard, io.Discard)
			if handled != test.want || (err != nil) != test.want {
				t.Fatalf("handled = %t, error = %v, want %t", handled, err, test.want)
			}
		})
	}
}
