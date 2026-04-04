//go:build darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallProxyAgentWritesPlistAndVersionMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	oldVersion := version
	version = "test-version"
	defer func() { version = oldVersion }()

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

	match, err := proxyAgentVersionMarkerMatches("test-version")
	if err != nil {
		t.Fatalf("proxyAgentVersionMarkerMatches: %v", err)
	}
	if !match {
		t.Fatalf("expected version marker to match")
	}
	if !strings.Contains(stderr, "installed proxy LaunchAgent") {
		t.Fatalf("stderr = %q, want install notice", stderr)
	}
}

func TestProxyAgentVersionMarkerMatches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := writeProxyAgentVersionMarker("v1.2.3"); err != nil {
		t.Fatalf("writeProxyAgentVersionMarker: %v", err)
	}

	match, err := proxyAgentVersionMarkerMatches("v1.2.3")
	if err != nil {
		t.Fatalf("proxyAgentVersionMarkerMatches: %v", err)
	}
	if !match {
		t.Fatalf("expected version marker to match")
	}

	match, err = proxyAgentVersionMarkerMatches("v9.9.9")
	if err != nil {
		t.Fatalf("proxyAgentVersionMarkerMatches mismatch: %v", err)
	}
	if match {
		t.Fatalf("expected version marker mismatch")
	}
}

func TestEnsureProxyAgentCurrentRestartsWhenVersionDiffers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		t.Fatalf("proxyAgentPlistPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir plist dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("plist"), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	if err := writeProxyAgentVersionMarker("old-version"); err != nil {
		t.Fatalf("seed version marker: %v", err)
	}

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	oldVersion := version
	version = "test-version"
	defer func() { version = oldVersion }()

	stderr := captureStderr(t, func() {
		ensureProxyAgentCurrent(version)
	})

	if len(calls) != 1 {
		t.Fatalf("launchctl calls = %d, want 1", len(calls))
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), proxyAgentLabel)}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Fatalf("launchctl args = %v, want %v", calls[0], want)
	}
	if !strings.Contains(stderr, "restarted proxy LaunchAgent for test-version") {
		t.Fatalf("stderr = %q, want restart notice", stderr)
	}
	match, err := proxyAgentVersionMarkerMatches("test-version")
	if err != nil {
		t.Fatalf("proxyAgentVersionMarkerMatches after restart: %v", err)
	}
	if !match {
		t.Fatalf("expected updated version marker")
	}
}

func TestEnsureProxyAgentCurrentSkipsWhenVersionMatches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		t.Fatalf("proxyAgentPlistPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir plist dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("plist"), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	if err := writeProxyAgentVersionMarker("same-version"); err != nil {
		t.Fatalf("seed version marker: %v", err)
	}

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	called := false
	runProxyLaunchctl = func(args ...string) error {
		called = true
		return nil
	}

	stderr := captureStderr(t, func() {
		ensureProxyAgentCurrent("same-version")
	})

	if called {
		t.Fatalf("expected no launchctl call when version matches")
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestEnsureProxyAgentCurrentReportsRestartFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		t.Fatalf("proxyAgentPlistPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("mkdir plist dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("plist"), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()
	runProxyLaunchctl = func(args ...string) error {
		return errors.New("boom")
	}

	oldVersion := version
	version = "test-version"
	defer func() { version = oldVersion }()

	stderr := captureStderr(t, func() {
		ensureProxyAgentCurrent(version)
	})
	if !strings.Contains(stderr, "proxy auto-restart failed") {
		t.Fatalf("stderr = %q, want auto-restart failure", stderr)
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

