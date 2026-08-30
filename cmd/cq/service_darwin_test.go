//go:build darwin

package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestDarwinServiceInstallsExactProxyAndRefreshLaunchAgents(t *testing.T) {
	platform, runner := newDarwinServiceHarness(t)
	executable := platform.executable

	if err := platform.Preflight(context.Background(), executable); err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	if err := platform.InstallProxy(context.Background(), executable); err != nil {
		t.Fatalf("InstallProxy() error = %v", err)
	}
	if err := platform.InstallRefresh(context.Background(), executable); err != nil {
		t.Fatalf("InstallRefresh() error = %v", err)
	}

	proxyDefinition := readDarwinDefinition(t, platform.plistPath(proxyAgentLabel))
	if proxyDefinition.Label != proxyAgentLabel || !reflect.DeepEqual(proxyDefinition.ProgramArguments, []string{executable, "proxy", "start"}) || !proxyDefinition.RunAtLoad || !proxyDefinition.KeepAlive {
		t.Fatalf("proxy definition = %#v", proxyDefinition)
	}
	if proxyDefinition.StandardErrorPath != filepath.Join(platform.roots.Logs, "proxy.log") {
		t.Fatalf("proxy log = %q", proxyDefinition.StandardErrorPath)
	}
	refreshDefinition := readDarwinDefinition(t, platform.plistPath(agentLabel))
	if refreshDefinition.Label != agentLabel || !reflect.DeepEqual(refreshDefinition.ProgramArguments, []string{executable, "refresh"}) || !refreshDefinition.RunAtLoad || refreshDefinition.StartInterval != 1800 || refreshDefinition.KeepAlive {
		t.Fatalf("refresh definition = %#v", refreshDefinition)
	}
	if refreshDefinition.StandardErrorPath != filepath.Join(platform.roots.Logs, "refresh.log") {
		t.Fatalf("refresh log = %q", refreshDefinition.StandardErrorPath)
	}

	wantCalls := [][]string{
		{"print", "gui/501/" + homebrewProxyAgentLabel},
		{"print", "gui/501/" + proxyAgentLabel},
		{"print", "gui/501/" + agentLabel},
		{"bootout", "gui/501/" + proxyAgentLabel},
		{"bootstrap", "gui/501", platform.plistPath(proxyAgentLabel)},
		{"kickstart", "-k", "gui/501/" + proxyAgentLabel},
		{"bootout", "gui/501/" + agentLabel},
		{"bootstrap", "gui/501", platform.plistPath(agentLabel)},
		{"kickstart", "-k", "gui/501/" + agentLabel},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("launchctl calls = %#v\nwant = %#v", runner.calls, wantCalls)
	}
	assertNoDarwinTemporaryFiles(t, filepath.Dir(platform.plistPath(proxyAgentLabel)))
}

func TestDarwinServiceEscapesLaunchAgentValues(t *testing.T) {
	platform, _ := newDarwinServiceHarness(t)
	platform.executable = filepath.Join(platform.home, "bin & tools", "cq")
	platform.roots.Logs = filepath.Join(platform.home, "Logs & State")

	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatalf("InstallRefresh() error = %v", err)
	}
	data, err := os.ReadFile(platform.plistPath(agentLabel))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "bin & tools") || strings.Contains(string(data), "Logs & State") {
		t.Fatalf("plist contains unescaped ampersand:\n%s", data)
	}
	definition := readDarwinDefinition(t, platform.plistPath(agentLabel))
	if definition.ProgramArguments[0] != platform.executable || definition.StandardErrorPath != filepath.Join(platform.roots.Logs, "refresh.log") {
		t.Fatalf("decoded definition = %#v", definition)
	}
}

func TestDarwinServicePreflightRejectsLegacyHomebrewJob(t *testing.T) {
	platform, runner := newDarwinServiceHarness(t)
	runner.loaded[homebrewProxyAgentLabel] = true

	err := platform.Preflight(context.Background(), platform.executable)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Preflight() error = %v, want ownership conflict", err)
	}
	if len(runner.calls) != 1 || runner.calls[0][1] != "gui/501/"+homebrewProxyAgentLabel {
		t.Fatalf("launchctl calls = %v", runner.calls)
	}
}

func TestDarwinServicePreflightRejectsDifferentExecutable(t *testing.T) {
	platform, _ := newDarwinServiceHarness(t)
	other := filepath.Join(platform.home, "other", "cq")
	data, err := renderDarwinLaunchAgent(darwinLaunchAgentDefinition{
		Label:             proxyAgentLabel,
		ProgramArguments:  []string{other, "proxy", "start"},
		RunAtLoad:         true,
		KeepAlive:         true,
		StandardErrorPath: filepath.Join(platform.roots.Logs, "proxy.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteDarwinLaunchAgent(platform.plistPath(proxyAgentLabel), data); err != nil {
		t.Fatal(err)
	}

	err = platform.Preflight(context.Background(), platform.executable)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Preflight() error = %v, want ownership conflict", err)
	}
}

func TestDarwinServiceRestoresPreviousDefinitionWhenBootstrapFails(t *testing.T) {
	platform, runner := newDarwinServiceHarness(t)
	oldExecutable := filepath.Join(platform.home, "old", "cq")
	oldData, err := renderDarwinLaunchAgent(darwinLaunchAgentDefinition{
		Label:             proxyAgentLabel,
		ProgramArguments:  []string{oldExecutable, "proxy", "start"},
		RunAtLoad:         true,
		KeepAlive:         true,
		StandardErrorPath: filepath.Join(platform.roots.Logs, "proxy.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := platform.plistPath(proxyAgentLabel)
	if err := atomicWriteDarwinLaunchAgent(path, oldData); err != nil {
		t.Fatal(err)
	}
	runner.loaded[proxyAgentLabel] = true
	runner.failOnce["bootstrap\x00gui/501\x00"+path] = errors.New("bootstrap failed")

	err = platform.InstallProxy(context.Background(), platform.executable)
	if err == nil || !strings.Contains(err.Error(), "bootstrap failed") {
		t.Fatalf("InstallProxy() error = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(oldData) {
		t.Fatalf("restored plist differs\ngot:\n%s\nwant:\n%s", got, oldData)
	}
	if !runner.loaded[proxyAgentLabel] {
		t.Fatal("previous proxy job was not restored")
	}
}

func TestDarwinServiceRestartsAndRemovesBothJobs(t *testing.T) {
	platform, runner := newDarwinServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil

	if err := platform.RestartRefresh(context.Background()); err != nil {
		t.Fatalf("RestartRefresh() error = %v", err)
	}
	if err := platform.RestartProxy(context.Background()); err != nil {
		t.Fatalf("RestartProxy() error = %v", err)
	}
	if err := platform.RemoveRefresh(context.Background()); err != nil {
		t.Fatalf("RemoveRefresh() error = %v", err)
	}
	if err := platform.RemoveProxy(context.Background()); err != nil {
		t.Fatalf("RemoveProxy() error = %v", err)
	}
	want := [][]string{
		{"kickstart", "-k", "gui/501/" + agentLabel},
		{"kickstart", "-k", "gui/501/" + proxyAgentLabel},
		{"bootout", "gui/501/" + agentLabel},
		{"bootout", "gui/501/" + proxyAgentLabel},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("launchctl calls = %#v, want %#v", runner.calls, want)
	}
	for _, label := range []string{proxyAgentLabel, agentLabel} {
		if _, err := os.Stat(platform.plistPath(label)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plist %s remains: %v", label, err)
		}
	}
}

func TestDarwinServiceInspectCombinesDefinitionsAndRuntime(t *testing.T) {
	platform, _ := newDarwinServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !status.Proxy.Registered || !status.Proxy.Running || !status.Proxy.Healthy || status.Proxy.ConfiguredExecutable != platform.executable || status.Proxy.LiveExecutable != platform.executable || status.Proxy.PID != 4312 || status.Proxy.Listener != "127.0.0.1:19280" {
		t.Fatalf("proxy status = %#v", status.Proxy)
	}
	if !status.Refresh.Registered || status.Refresh.Running || !status.Refresh.Healthy || status.Refresh.ConfiguredExecutable != platform.executable || status.Refresh.LastResult != "success" {
		t.Fatalf("refresh status = %#v", status.Refresh)
	}
}

func newDarwinServiceHarness(t *testing.T) (*darwinServicePlatform, *fakeDarwinLaunchctl) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	logs := filepath.Join(home, "Library", "Logs", "cq")
	executable := filepath.Join(home, "bin", "cq")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDarwinLaunchctl{
		loaded:   map[string]bool{},
		runs:     map[string]int{},
		failOnce: map[string]error{},
	}
	platform := &darwinServicePlatform{
		home:       home,
		roots:      userdirs.Roots{Logs: logs},
		uid:        501,
		executable: executable,
		run:        runner.Run,
		inspectProxy: func(context.Context, string) componentStatus {
			return componentStatus{
				ID:                   proxyAgentLabel,
				Manager:              "launchd",
				Registered:           true,
				Running:              true,
				ConfiguredExecutable: executable,
				LiveExecutable:       executable,
				PID:                  4312,
				Listener:             "127.0.0.1:19280",
				Healthy:              true,
			}
		},
	}
	return platform, runner
}

type fakeDarwinLaunchctl struct {
	calls    [][]string
	loaded   map[string]bool
	runs     map[string]int
	failOnce map[string]error
}

func (runner *fakeDarwinLaunchctl) Run(_ context.Context, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string(nil), args...))
	key := strings.Join(args, "\x00")
	if err := runner.failOnce[key]; err != nil {
		delete(runner.failOnce, key)
		return nil, err
	}
	switch args[0] {
	case "print":
		label := filepath.Base(args[1])
		if !runner.loaded[label] {
			return nil, darwinLaunchctlExitError(113)
		}
		if label == agentLabel {
			return []byte(fmt.Sprintf("%s = {\n\tstate = exited\n\truns = %d\n\tlast exit code = 0\n}\n", args[1], runner.runs[label])), nil
		}
		return []byte(fmt.Sprintf("%s = {\n\tstate = running\n\truns = %d\n\tpid = 4312\n\tlast exit code = 0\n}\n", args[1], runner.runs[label])), nil
	case "bootout":
		label := filepath.Base(args[1])
		if !runner.loaded[label] {
			return nil, darwinLaunchctlExitError(113)
		}
		runner.loaded[label] = false
		return nil, nil
	case "bootstrap":
		label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
		runner.loaded[label] = true
		return nil, nil
	case "kickstart":
		label := filepath.Base(args[2])
		if !runner.loaded[label] {
			return nil, darwinLaunchctlExitError(113)
		}
		runner.runs[label]++
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected launchctl command %q", args[0])
	}
}

type darwinLaunchctlExitError int

func (err darwinLaunchctlExitError) Error() string { return fmt.Sprintf("exit status %d", err) }
func (err darwinLaunchctlExitError) ExitCode() int { return int(err) }

func readDarwinDefinition(t *testing.T, path string) darwinLaunchAgentDefinition {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDarwinPlistXML(data); err != nil {
		t.Fatalf("invalid plist XML: %v\n%s", err, data)
	}
	definition, err := parseDarwinLaunchAgent(data)
	if err != nil {
		t.Fatalf("parse plist: %v\n%s", err, data)
	}
	return definition
}

func validateDarwinPlistXML(data []byte) error {
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		if _, err := decoder.Token(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func assertNoDarwinTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary LaunchAgent remains: %s", entry.Name())
		}
	}
}

var _ servicePlatform = (*darwinServicePlatform)(nil)
