//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
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
	t.Setenv("CODEX_CLI_PATH", "old")
	t.Setenv(windowsHarnessStockCLIEnv, "")
	t.Setenv("USER_SETTING", "preserve")
	if err := os.Unsetenv(windowsHarnessStockCLIEnv); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("launch failed")
	err := withWindowsHarnessEnvironment(`C:\cq.exe`, `C:\codex.exe`, func() error {
		if os.Getenv("CODEX_CLI_PATH") != `C:\cq.exe` || os.Getenv(windowsHarnessStockCLIEnv) != `C:\codex.exe` || os.Getenv("USER_SETTING") != "preserve" {
			t.Fatal("desktop launch environment differs from the scoped overrides")
		}
		return failure
	})
	if _, exists := os.LookupEnv(windowsHarnessStockCLIEnv); exists || os.Getenv("CODEX_CLI_PATH") != "old" || !errors.Is(err, failure) {
		t.Fatal("desktop launch environment or error was not restored correctly")
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
	cliDir := t.TempDir()
	t.Setenv("PATH", cliDir)
	if got := harnessNativeExecutable(); got != stock {
		t.Fatalf("harness executable without codex on PATH = %q, want %q", got, stock)
	}
	pathCLI := filepath.Join(cliDir, "codex.exe")
	if err := os.WriteFile(pathCLI, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := harnessNativeExecutable(); got != pathCLI {
		t.Fatalf("harness executable with codex on PATH = %q, want %q", got, pathCLI)
	}
}
