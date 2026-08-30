package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
)

func TestRunServiceInstallDefaultsToManualOwner(t *testing.T) {
	lifecycle, _, store := newServiceHarness(t)

	if err := runServiceWithLifecycle([]string{"install"}, lifecycle, io.Discard); err != nil {
		t.Fatalf("runServiceWithLifecycle() error = %v", err)
	}
	record, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record.Owner != installstate.OwnerManual {
		t.Fatalf("owner = %q, want manual", record.Owner)
	}
}

func TestRunServiceInstallAcceptsHiddenPackageOwner(t *testing.T) {
	lifecycle, _, store := newServiceHarness(t)

	if err := runServiceWithLifecycle([]string{"install", "--owner=homebrew"}, lifecycle, io.Discard); err != nil {
		t.Fatalf("runServiceWithLifecycle() error = %v", err)
	}
	record, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record.Owner != installstate.OwnerHomebrew {
		t.Fatalf("owner = %q, want homebrew", record.Owner)
	}
}

func TestRunServiceStatusWritesStableJSON(t *testing.T) {
	lifecycle, _, _ := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}

	if err := runServiceWithLifecycle([]string{"status", "--json"}, lifecycle, output); err != nil {
		t.Fatalf("runServiceWithLifecycle() error = %v", err)
	}
	var got serviceStatus
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("status JSON error = %v: %s", err, output.Bytes())
	}
	if got.SchemaVersion != serviceStatusSchemaVersion || got.Owner != installstate.OwnerGo || !got.Proxy.Healthy || !got.Refresh.Healthy {
		t.Fatalf("status = %#v", got)
	}
}

func TestRunServiceStatusWritesHumanSummary(t *testing.T) {
	lifecycle, _, _ := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}

	if err := runServiceWithLifecycle([]string{"status"}, lifecycle, output); err != nil {
		t.Fatalf("runServiceWithLifecycle() error = %v", err)
	}
	want := "CQ services: healthy\nProxy: healthy (test.proxy)\nRefresh: healthy (test.refresh)\n"
	if output.String() != want {
		t.Fatalf("status output = %q, want %q", output.String(), want)
	}
}

func TestRunServiceRejectsInvalidArgumentsBeforeMutation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing action"},
		{name: "unknown action", args: []string{"start"}},
		{name: "unknown owner", args: []string{"install", "--owner=package"}},
		{name: "owner on status", args: []string{"status", "--owner=go"}},
		{name: "JSON on install", args: []string{"install", "--json"}},
		{name: "extra argument", args: []string{"restart", "extra"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, platform, _ := newServiceHarness(t)
			if err := runServiceWithLifecycle(test.args, lifecycle, io.Discard); err == nil {
				t.Fatalf("runServiceWithLifecycle(%v) error = nil", test.args)
			}
			if len(platform.calls) != 0 {
				t.Fatalf("platform calls = %v", platform.calls)
			}
		})
	}
}

func TestRunServiceRestartAndUninstall(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerWinGet); err != nil {
		t.Fatal(err)
	}
	platform.calls = nil

	if err := runServiceWithLifecycle([]string{"restart"}, lifecycle, io.Discard); err != nil {
		t.Fatalf("restart error = %v", err)
	}
	if err := runServiceWithLifecycle([]string{"uninstall", "--owner=winget"}, lifecycle, io.Discard); err != nil {
		t.Fatalf("uninstall error = %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, installstate.ErrNotInstalled) {
		t.Fatalf("state after uninstall error = %v", err)
	}
}

func TestServiceHelpIsPureAndHidesOwnerFlag(t *testing.T) {
	for _, args := range [][]string{
		{"service", "--help"},
		{"service", "install", "--help"},
		{"service", "restart", "--help"},
		{"service", "status", "--help"},
		{"service", "uninstall", "--help"},
	} {
		output := &bytes.Buffer{}
		handled, exitCode, err := runPureGlobalInspection(args, output, io.Discard)
		if !handled || exitCode != 0 || err != nil {
			t.Fatalf("runPureGlobalInspection(%v) = %t, %d, %v", args, handled, exitCode, err)
		}
		if strings.Contains(output.String(), "--owner") {
			t.Fatalf("service help exposed package owner flag:\n%s", output.String())
		}
	}
}

func TestOrdinaryCheckDoesNotInstallRefreshAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent mutation exists only on macOS")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "cq")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--json", "check", "gemini")
	command.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cq check gemini: %v\n%s", err, output)
	}
	agent := filepath.Join(home, "Library", "LaunchAgents", "dev.jacobcx.cq.refresh.plist")
	if _, err := os.Stat(agent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary check created refresh agent %s: %v", agent, err)
	}
}
