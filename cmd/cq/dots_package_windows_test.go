//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/dots"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestWindowsDotsDefaultRootUsesCurrentUserCQState(t *testing.T) {
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	got, err := dotsDefaultRoot("")
	if err != nil || got != filepath.Join(roots.State, "dots") {
		t.Fatalf("Windows Dots default root: %q, %v", got, err)
	}
}

func TestWindowsDotsInstallForwardsRegisteredPackageWithoutLaunch(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "private dots")
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(canonicalHome, "private dots")
	packageRoot := filepath.Join(home, "OpenAI.Codex_26.1120.5083.0_x64__2p2nqsd0c76g0")
	runner := dotsRunner{
		home:       func() (string, error) { return home, nil },
		executable: func() (string, error) { return filepath.Join(home, "cq.exe"), nil },
		baseURL:    func() (string, error) { return "http://127.0.0.1:19280", nil },
		app: func() (dots.Settings, string, error) {
			return dots.Settings{AppPath: packageRoot}, filepath.Join(packageRoot, "app", "ChatGPT.exe"), nil
		},
		run: func(_ context.Context, controller dots.Controller, request dots.Request) (map[string]any, error) {
			if request.Operation != "install" || request.Install.App != packageRoot || request.Install.Mode != "native" || controller.Root != wantRoot {
				t.Fatalf("Windows install request: %#v %#v", controller, request)
			}
			return map[string]any{"installed": true}, nil
		},
	}
	var output bytes.Buffer
	if err := runner.execute(t.Context(), &CodexDotsCmd{Root: root}, "install", true, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("CLI unexpectedly created state: %v", err)
	}
	var result map[string]bool
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result["installed"] {
		t.Fatalf("Windows install output: %v %#v", err, result)
	}
}

func TestWindowsDotsLaunchForwardsOnlyTheSelectedRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "dots")
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(canonicalHome, "dots")
	runner := dotsRunner{
		home: func() (string, error) { return home, nil },
		launch: func(_ context.Context, selected string, _, _ io.Writer) error {
			if selected != wantRoot {
				t.Fatalf("launcher root = %q", selected)
			}
			return nil
		},
	}
	if err := runner.execute(t.Context(), &CodexDotsCmd{Root: root}, "launch", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsDotsInstallRejectsIsolatedAndForeignPackage(t *testing.T) {
	qualified := filepath.Join(t.TempDir(), "OpenAI.Codex_26.1120.5083.0_x64__2p2nqsd0c76g0")
	resolve := func() (dots.Settings, string, error) {
		return dots.Settings{AppPath: qualified}, filepath.Join(qualified, "app", "ChatGPT.exe"), nil
	}
	for _, options := range []CodexDotsInstallCmd{
		{Mode: "isolated"},
		{Mode: "native", App: filepath.Join(t.TempDir(), "other")},
	} {
		if err := dotsInstallApp(&options, resolve); err == nil {
			t.Fatalf("unsupported Windows install accepted: %#v", options)
		}
	}
}

func TestWindowsDotsRootRejectsRegisteredPackageData(t *testing.T) {
	home := t.TempDir()
	protected, err := dots.ProtectedAppRoots(home)
	if err != nil {
		t.Fatal(err)
	}
	var packageData string
	for _, path := range protected {
		if filepath.Base(path) == "OpenAI.Codex_2p2nqsd0c76g0" {
			packageData = path
			break
		}
	}
	if packageData == "" {
		t.Fatal("registered package data root missing from protected paths")
	}
	runner := dotsRunner{
		home: func() (string, error) { return home, nil },
		run: func(context.Context, dots.Controller, dots.Request) (map[string]any, error) {
			t.Fatal("protected root reached Dots controller")
			return nil, nil
		},
	}
	if err := runner.execute(t.Context(), &CodexDotsCmd{Root: filepath.Join(packageData, "dots")}, "status", true, io.Discard, io.Discard); err == nil {
		t.Fatal("Dots root inside registered package data accepted")
	}
}

func TestRegisteredWindowsDotsPackageReadOnly(t *testing.T) {
	if os.Getenv("CQ_TEST_WINDOWS_DOTS_REGISTERED") != "1" {
		t.Skip("set CQ_TEST_WINDOWS_DOTS_REGISTERED=1 for read-only installed package qualification")
	}
	pin, executable, err := resolveWindowsDotsApp()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(executable) != "ChatGPT.exe" || pin.NativeBindings == nil {
		t.Fatal("registered package did not resolve the Dots renderer")
	}
	if err := dots.CheckWindowsApp(&pin); err != nil {
		t.Fatal(err)
	}
	t.Logf("registered package qualified: version=%s renderer=%s", pin.AppVersion, pin.ModuleAsset)
}
