package main

import (
	"bytes"
	"context"
	"errors"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
)

func TestEffectiveVersionUsesTaggedModule(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{
		Path:    "github.com/jacobcxdev/cq",
		Version: "v0.27.0",
	}}
	got, err := effectiveVersionFrom("", info, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.27.0" {
		t.Fatalf("effectiveVersionFrom() = %q", got)
	}
}

func TestEffectiveVersionUsesLinkedReleaseVersion(t *testing.T) {
	got, err := effectiveVersionFrom("0.27.0", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.27.0" {
		t.Fatalf("effectiveVersionFrom() = %q", got)
	}
}

func TestEffectiveVersionRejectsDevelopmentAndReplacedModules(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
	}{
		{name: "devel", info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/jacobcxdev/cq", Version: "(devel)"}}},
		{name: "empty", info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/jacobcxdev/cq"}}},
		{name: "wrong module", info: &debug.BuildInfo{Main: debug.Module{Path: "example.com/fork", Version: "v0.27.0"}}},
		{name: "replaced", info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/jacobcxdev/cq", Version: "v0.27.0", Replace: &debug.Module{Path: "../cq", Version: "(devel)"}}}},
		{name: "prerelease", info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/jacobcxdev/cq", Version: "v0.27.0-rc.1"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := effectiveVersionFrom("", test.info, true); err == nil {
				t.Fatal("effectiveVersionFrom() succeeded")
			}
		})
	}
}

func TestRunInstallerDefaultsToGoInstall(t *testing.T) {
	action := &fakeInstallerAction{}
	output := &bytes.Buffer{}
	var gotOwner installstate.Owner
	var gotVersion string
	deps := commandDependencies{
		ResolveVersion: func() (string, error) { return "0.27.0", nil },
		Build: func(_ context.Context, owner installstate.Owner, version string) (installerAction, error) {
			gotOwner, gotVersion = owner, version
			return action, nil
		},
		Output: output,
	}

	if err := runInstaller(context.Background(), nil, deps); err != nil {
		t.Fatal(err)
	}
	if action.installs != 1 || action.uninstalls != 0 || gotOwner != installstate.OwnerGo || gotVersion != "0.27.0" {
		t.Fatalf("calls install=%d uninstall=%d owner=%q version=%q", action.installs, action.uninstalls, gotOwner, gotVersion)
	}
	if got := output.String(); !strings.Contains(got, "CQ 0.27.0 installed") {
		t.Fatalf("output = %q", got)
	}
}

func TestRunInstallerUninstalls(t *testing.T) {
	action := &fakeInstallerAction{}
	deps := testCommandDependencies(action)
	if err := runInstaller(context.Background(), []string{"uninstall"}, deps); err != nil {
		t.Fatal(err)
	}
	if action.installs != 0 || action.uninstalls != 1 {
		t.Fatalf("calls install=%d uninstall=%d", action.installs, action.uninstalls)
	}
}

func TestRunInstallerWinGetOwnerRequiresReleaseBuild(t *testing.T) {
	args := []string{"install", "--owner=winget", "--silent"}
	if err := runInstaller(context.Background(), args, testCommandDependencies(&fakeInstallerAction{})); err == nil {
		t.Fatal("development runner accepted WinGet owner")
	}

	action := &fakeInstallerAction{}
	deps := testCommandDependencies(action)
	deps.AllowWinGet = true
	var gotOwner installstate.Owner
	deps.Build = func(_ context.Context, owner installstate.Owner, _ string) (installerAction, error) {
		gotOwner = owner
		return action, nil
	}
	if err := runInstaller(context.Background(), args, deps); err != nil {
		t.Fatal(err)
	}
	if gotOwner != installstate.OwnerWinGet || action.installs != 1 {
		t.Fatalf("owner=%q installs=%d", gotOwner, action.installs)
	}
}

func TestRunInstallerRejectsUnsupportedOwners(t *testing.T) {
	for _, owner := range []string{"manual", "homebrew", "invalid"} {
		if err := runInstaller(context.Background(), []string{"--owner=" + owner}, testCommandDependencies(&fakeInstallerAction{})); err == nil {
			t.Fatalf("owner %q accepted", owner)
		}
	}
}

func TestRunInstallerSilentAndHelp(t *testing.T) {
	action := &fakeInstallerAction{}
	deps := testCommandDependencies(action)
	if err := runInstaller(context.Background(), []string{"--silent"}, deps); err != nil {
		t.Fatal(err)
	}
	if deps.Output.(*bytes.Buffer).Len() != 0 {
		t.Fatalf("silent output = %q", deps.Output.(*bytes.Buffer).String())
	}

	helpOutput := &bytes.Buffer{}
	deps.Output = helpOutput
	deps.ResolveVersion = func() (string, error) { return "", errors.New("must not resolve") }
	if err := runInstaller(context.Background(), []string{"--help"}, deps); err != nil {
		t.Fatal(err)
	}
	if got := helpOutput.String(); !strings.Contains(got, "cq-install [install|uninstall]") || strings.Contains(got, "--owner") {
		t.Fatalf("help output = %q", got)
	}
}

func TestRunInstallerPropagatesActionFailure(t *testing.T) {
	want := errors.New("operation failed")
	for _, uninstall := range []bool{false, true} {
		action := &fakeInstallerAction{err: want}
		args := []string(nil)
		if uninstall {
			args = []string{"uninstall"}
		}
		err := runInstaller(context.Background(), args, testCommandDependencies(action))
		if !errors.Is(err, want) {
			t.Fatalf("runInstaller(%v) error = %v", args, err)
		}
	}
}

func TestRunInstallerDoesNotEchoUnknownArguments(t *testing.T) {
	secret := "token-super-secret"
	err := runInstaller(context.Background(), []string{"--access-token=" + secret}, testCommandDependencies(&fakeInstallerAction{}))
	if err == nil {
		t.Fatal("unknown argument accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed argument: %v", err)
	}
}

func TestCommandLifecycleUsesExactServiceCommands(t *testing.T) {
	var calls [][]string
	lifecycle := commandLifecycle{
		Executable: "/go/bin/cq",
		Owner:      installstate.OwnerGo,
		Run: func(_ context.Context, executable string, args ...string) error {
			calls = append(calls, append([]string{executable}, args...))
			return nil
		},
	}
	ctx := context.Background()
	if err := lifecycle.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Install(ctx, installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Uninstall(ctx, installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/go/bin/cq", "service", "uninstall", "--owner=go"},
		{"/go/bin/cq", "service", "install", "--owner=go"},
		{"/go/bin/cq", "service", "status", "--json"},
		{"/go/bin/cq", "service", "uninstall", "--owner=go"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %#v", calls)
	}
	for index := range want {
		if strings.Join(calls[index], "\x00") != strings.Join(want[index], "\x00") {
			t.Fatalf("call %d = %#v, want %#v", index, calls[index], want[index])
		}
	}
}

func TestCommandVersionRunnerNormalisesReleaseVersion(t *testing.T) {
	runner := commandVersionRunner{Run: func(context.Context, string) (string, error) {
		return "v0.27.0\n", nil
	}}
	got, err := runner.Version(context.Background(), "/go/bin/cq")
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.27.0" {
		t.Fatalf("Version() = %q", got)
	}
}

func testCommandDependencies(action installerAction) commandDependencies {
	return commandDependencies{
		ResolveVersion: func() (string, error) { return "0.27.0", nil },
		Build: func(context.Context, installstate.Owner, string) (installerAction, error) {
			return action, nil
		},
		Output: &bytes.Buffer{},
	}
}

type fakeInstallerAction struct {
	installs   int
	uninstalls int
	err        error
}

func (action *fakeInstallerAction) Install(context.Context) error {
	action.installs++
	return action.err
}

func (action *fakeInstallerAction) Uninstall(context.Context) error {
	action.uninstalls++
	return action.err
}
