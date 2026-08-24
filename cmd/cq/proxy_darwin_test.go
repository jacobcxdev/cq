//go:build darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestInstallProxyAgentWritesPlist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	if err := proxy.SaveConfig(&proxy.Config{Port: 24567, LocalToken: "test-token"}); err != nil {
		t.Fatal(err)
	}

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	stderr := captureStderr(t, func() {
		if err := installProxyAgent(); err != nil {
			t.Fatalf("installProxyAgent: %v", err)
		}
	})

	if len(calls) != 2 {
		t.Fatalf("launchctl calls = %d, want 2", len(calls))
	}
	if got, want := strings.Join(calls[0], "|"), strings.Join([]string{"unload", filepath.Join(dir, "Library", "LaunchAgents", proxyAgentLabel+".plist")}, "|"); got != want {
		t.Fatalf("first launchctl call = %v, want unload of plist", calls[0])
	}
	if got, want := strings.Join(calls[1], "|"), strings.Join([]string{"load", filepath.Join(dir, "Library", "LaunchAgents", proxyAgentLabel+".plist")}, "|"); got != want {
		t.Fatalf("second launchctl call = %v, want load of plist", calls[1])
	}

	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		t.Fatalf("proxyAgentPlistPath: %v", err)
	}
	plistData, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	plist := string(plistData)
	if !strings.Contains(plist, "<string>proxy</string>") || !strings.Contains(plist, "<string>start</string>") {
		t.Fatalf("plist = %q, want proxy start arguments", plist)
	}
	if strings.Contains(plist, "<key>Sockets</key>") {
		t.Fatalf("plist = %q, want runtime supervisor to own listener", plist)
	}
	if !strings.Contains(plist, "<string>"+filepath.Join(dir, "Library", "Logs", "cq", "proxy.log")+"</string>") {
		t.Fatalf("plist = %q, want proxy log path", plist)
	}
	if !strings.Contains(stderr, "installed proxy LaunchAgent") {
		t.Fatalf("stderr = %q, want install notice", stderr)
	}
}

func TestInstallProxyAgentRejectsHomebrewExecutable(t *testing.T) {
	oldExecutable := currentExecutable
	oldRunner := runProxyLaunchctl
	defer func() {
		currentExecutable = oldExecutable
		runProxyLaunchctl = oldRunner
	}()
	currentExecutable = func() (string, error) {
		return "/opt/homebrew/Cellar/cq/0.23.6/bin/cq", nil
	}
	runProxyLaunchctl = func(...string) error {
		t.Fatal("launchctl called for Homebrew-managed install")
		return nil
	}

	err := installProxyAgent()
	if err == nil || !strings.Contains(err.Error(), "brew services start cq") {
		t.Fatalf("installProxyAgent error = %v, want Homebrew start guidance", err)
	}
}

func TestUninstallProxyAgentRejectsHomebrewExecutable(t *testing.T) {
	oldExecutable := currentExecutable
	oldRunner := runProxyLaunchctl
	defer func() {
		currentExecutable = oldExecutable
		runProxyLaunchctl = oldRunner
	}()
	currentExecutable = func() (string, error) {
		return "/usr/local/Cellar/cq/0.23.6/bin/cq", nil
	}
	runProxyLaunchctl = func(...string) error {
		t.Fatal("launchctl called for Homebrew-managed uninstall")
		return nil
	}

	err := uninstallProxyAgent()
	if err == nil || !strings.Contains(err.Error(), "brew services stop cq") {
		t.Fatalf("uninstallProxyAgent error = %v, want Homebrew stop guidance", err)
	}
}

func TestRestartProxyAgentRunsKickstart(t *testing.T) {
	t.Helper()

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	if err := restartProxyAgent(); err != nil {
		t.Fatalf("restartProxyAgent: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("launchctl calls = %d, want 1", len(calls))
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), proxyAgentLabel)}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Fatalf("launchctl args = %v, want %v", calls[0], want)
	}
}

func TestRestartProxyAgentFallsBackToHomebrewLabel(t *testing.T) {
	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if strings.HasSuffix(args[len(args)-1], "/"+proxyAgentLabel) {
			return launchctlTestExitError(113)
		}
		return nil
	}

	if err := restartProxyAgent(); err != nil {
		t.Fatalf("restartProxyAgent: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("launchctl calls = %d, want 2", len(calls))
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), homebrewProxyAgentLabel)}
	if strings.Join(calls[1], "|") != strings.Join(want, "|") {
		t.Fatalf("fallback launchctl args = %v, want %v", calls[1], want)
	}
}

func TestDarwinProxyInspectionBoundaryHasNoLiveCollectorsInCU1(t *testing.T) {
	target := darwinProxyInspectionTarget()
	if target.Inspector == nil || target.Desired == nil || target.Service == nil || target.Listener == nil || target.Process == nil || target.Runtime == nil || target.DataPlane == nil {
		t.Fatalf("Darwin inspection target omitted collector: %+v", target)
	}
}

func TestDarwinProxyInspectionUsesExistingConfigWithoutResilienceState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := proxy.SaveConfig(&proxy.Config{Port: 24567, LocalToken: "test-token"}); err != nil {
		t.Fatal(err)
	}

	facts := collectDarwinProxyInspectionFacts(context.Background(), "")
	if facts.desired.Status != proxy.FactKnown || facts.desired.Value == nil {
		t.Fatalf("desired fact = %+v, want known", facts.desired)
	}
	if got, want := facts.desired.Value.Listener, "127.0.0.1:24567"; got != want {
		t.Fatalf("desired listener = %q, want %q", got, want)
	}
}

func TestDarwinProxyInspectionUsesConfiguredPortWhenServiceOmitsPort(t *testing.T) {
	if got := darwinProxyInspectionListenerPort(19280, 0); got != 19280 {
		t.Fatalf("listener port = %d, want 19280", got)
	}
	if got := darwinProxyInspectionListenerPort(19280, 29280); got != 29280 {
		t.Fatalf("explicit listener port = %d, want 29280", got)
	}
}

type launchctlTestExitError int

func (e launchctlTestExitError) Error() string { return fmt.Sprintf("exit status %d", e) }
func (e launchctlTestExitError) ExitCode() int { return int(e) }

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return buf.String()
}
