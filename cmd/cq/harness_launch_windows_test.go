//go:build windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOfficialWindowsCodexPackage(t *testing.T) {
	for _, tc := range []struct {
		name string
		full string
		want bool
	}{
		{"x64", "OpenAI.Codex_26.1117.4957.0_x64__2p2nqsd0c76g0", true},
		{"arm64", "OpenAI.Codex_26.1117.4957.0_arm64__2p2nqsd0c76g0", true},
		{"other publisher", "OpenAI.Codex_26.1117.4957.0_x64__other", false},
		{"other name", "OpenAI.Codex.Candidate_26.1117.4957.0_x64__2p2nqsd0c76g0", false},
		{"resource", "OpenAI.Codex_26.1117.4957.0_x64_en-us_2p2nqsd0c76g0", false},
		{"malformed version", "OpenAI.Codex_26.1117.x.0_x64__2p2nqsd0c76g0", false},
		{"malformed architecture", "OpenAI.Codex_26.1117.4957.0_x86__2p2nqsd0c76g0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := officialWindowsCodexPackage(tc.full); got != tc.want {
				t.Fatalf("officialWindowsCodexPackage(%q) = %v, want %v", tc.full, got, tc.want)
			}
		})
	}
}

func TestWindowsHarnessChildEnv(t *testing.T) {
	base := []string{"PATH=C:\\bin", "CODEX_CLI_PATH=old", "cq_windows_harness_stock_cli=old", "USER_SETTING=preserve"}
	want := []string{"PATH=C:\\bin", "USER_SETTING=preserve", "CODEX_CLI_PATH=C:\\cq.exe", windowsHarnessStockCLIEnv + "=C:\\codex.exe"}
	if got := windowsHarnessChildEnv(base, `C:\cq.exe`, `C:\codex.exe`); !reflect.DeepEqual(got, want) {
		t.Fatalf("child env = %#v, want %#v", got, want)
	}
}

func TestWindowsHarnessInstalledPackageResolution(t *testing.T) {
	if os.Getenv("CQ_TEST_INSTALLED_WINDOWS_CODEX_PACKAGE") != "1" {
		t.Skip("requires an installed official Codex Desktop package")
	}
	desktop, stock, err := resolveWindowsHarnessDesktop()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(desktop) != "ChatGPT.exe" || filepath.Base(stock) != "codex.exe" {
		t.Fatalf("unexpected package executable names: %q, %q", filepath.Base(desktop), filepath.Base(stock))
	}
	if !officialWindowsCodexPackage(filepath.Base(filepath.Dir(filepath.Dir(desktop)))) {
		t.Fatal("Desktop executable is outside the official package root")
	}
	if _, err := windowsHarnessDesktopRunning(desktop); err != nil {
		t.Fatalf("inspect current-session Desktop processes: %v", err)
	}
}
