//go:build darwin

package main

import (
	"bytes"
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
	if !strings.Contains(plist, "<string>24567</string>") {
		t.Fatalf("plist = %q, want configured port", plist)
	}
	if !strings.Contains(plist, "<string>"+filepath.Join(dir, "Library", "Logs", "cq", "proxy.log")+"</string>") {
		t.Fatalf("plist = %q, want proxy log path", plist)
	}
	if !strings.Contains(stderr, "installed proxy LaunchAgent") {
		t.Fatalf("stderr = %q, want install notice", stderr)
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
