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
)

func TestInstallProxyAgentWritesPlist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

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
