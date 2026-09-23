//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
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

func TestServiceStatusHealthyForAcceptsSymlinkedExecutable(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "cq")
	if err := os.WriteFile(target, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "linked-cq")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	status := serviceStatus{
		Proxy: componentStatus{
			Registered:           true,
			Running:              true,
			Healthy:              true,
			ConfiguredExecutable: link,
			LiveExecutable:       target,
		},
		Refresh: componentStatus{
			Registered:           true,
			Healthy:              true,
			ConfiguredExecutable: link,
		},
	}

	if !status.healthyFor(link) {
		t.Fatal("healthy Cask symlink and runtime target rejected")
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
		disabled: map[string]bool{},
		runs:     map[string]int{},
		failOnce: map[string]error{},
	}
	platform := &darwinServicePlatform{
		home:         home,
		roots:        userdirs.Roots{Logs: logs},
		uid:          501,
		executable:   executable,
		run:          runner.Run,
		processAlive: func(int) bool { return false },
		inspectSelectedProxy: func(context.Context, string, userdirs.Roots, int) componentStatus {
			return componentStatus{Healthy: true, LiveExecutable: executable, Listener: "127.0.0.1:19280"}
		},
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
	disabled map[string]bool
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
	case "print-disabled":
		output := "disabled services = {\n"
		for label, disabled := range runner.disabled {
			output += fmt.Sprintf("\t%q => %t\n", label, disabled)
		}
		return []byte(output + "}\n"), nil
	case "load":
		label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
		runner.loaded[label] = true
		return nil, nil
	case "enable", "disable":
		runner.disabled[filepath.Base(args[1])] = args[0] == "disable"
		return nil, nil
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
		if runner.disabled[label] {
			return nil, fmt.Errorf("service disabled")
		}
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

func init() {
	registerV2Fixture("mac-stop", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		p, r := newDarwinServiceHarness(t)
		if err := p.InstallProxy(context.Background(), p.executable); err != nil {
			t.Fatal(err)
		}
		if err := p.InstallRefresh(context.Background(), p.executable); err != nil {
			t.Fatal(err)
		}
		refresh, _ := os.ReadFile(p.plistPath(agentLabel))
		r.calls = nil
		l, _, store := newSelectedServiceHarness(t)
		l.Platform, l.Executable = p, p.executable
		store.record.Executable = p.executable
		store.record.Services = []string{proxyAgentLabel, agentLabel}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Service(path)
			return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
				out := handleV2ServiceWithPreparation(ctx, inv, session, func(context.Context) (*serviceLifecycle, error) { return l, nil })
				for _, call := range r.calls {
					if strings.Contains(strings.Join(call, " "), agentLabel) {
						f.Call("refresh-manager")
					}
					if call[0] == "disable" {
						f.Call("proxy-disable")
					}
					if call[0] == "bootout" {
						f.Call("proxy-stop")
					}
				}
				after, _ := os.ReadFile(p.plistPath(agentLabel))
				if !reflect.DeepEqual(refresh, after) || !r.loaded[agentLabel] {
					t.Error("unselected refresh changed")
				}
				return out
			}, ok
		}
		return f
	})
}
func TestCLIV2MacServiceStop(t *testing.T) {
	runV2Case(t, v2Case{Name: "macOS persistent proxy stop", Scenario: "mac-stop", Args: []string{"service", "stop", "--component", "proxy", "--json"}, Exit: 0, Command: "service stop", WantJSON: `{"component":"proxy"}`, Forbid: []string{"refresh-manager"}, Calls: map[string]int{"proxy-disable": 1, "proxy-stop": 1}})
}

func newSelectedDarwinHarness(t *testing.T) (*darwinServicePlatform, *fakeDarwinLaunchctl) {
	t.Helper()
	p, r := newDarwinServiceHarness(t)
	home, err := filepath.EvalSymlinks(p.home)
	if err != nil {
		t.Fatal(err)
	}
	p.home = home
	p.executable = filepath.Join(home, "bin", "cq")
	p.roots, err = (userdirs.Resolver{UserHomeDir: func() (string, error) { return home, nil }}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	p.inspectSelectedProxy = func(_ context.Context, exe string, _ userdirs.Roots, _ int) componentStatus {
		return componentStatus{Healthy: true, LiveExecutable: exe, Listener: "127.0.0.1:19280"}
	}
	for _, install := range []func(context.Context, string) error{p.InstallProxy, p.InstallRefresh} {
		if err := install(context.WithValue(context.Background(), selectedServiceContextKey{}, true), p.executable); err != nil {
			t.Fatal(err)
		}
	}
	p.runRefreshOnce = func(_ context.Context, d darwinLaunchAgentDefinition) error {
		return recordDarwinServiceRefresh(d, func() error { return nil }, time.Now)
	}
	r.calls = nil
	return p, r
}
func TestDarwinServiceSelectedStartStop(t *testing.T) {
	for _, component := range []serviceSelection{serviceProxy, serviceRefresh} {
		for _, loaded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_loaded_%t", component, loaded), func(t *testing.T) {
				p, r := newSelectedDarwinHarness(t)
				label := darwinSelectedLabels(component)[0]
				r.loaded[label] = loaded
				r.disabled[label] = true
				start, stop := p.StartProxy, p.StopProxy
				if component == serviceRefresh {
					start, stop = p.StartRefresh, p.StopRefresh
				}
				if err := start(context.Background()); err != nil {
					t.Fatal(err)
				}
				want := [][]string{{"enable", p.target(label)}, {"print", p.target(label)}}
				if !loaded {
					want = append(want, []string{"bootstrap", p.domain(), p.plistPath(label)})
				}
				want = append(want, []string{"kickstart", "-k", p.target(label)})
				if !reflect.DeepEqual(r.calls, want) {
					t.Fatalf("start calls=%v want=%v", r.calls, want)
				}
				r.calls = nil
				if err := stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				want = [][]string{{"print", p.target(label)}, {"disable", p.target(label)}, {"print-disabled", p.domain()}, {"bootout", p.target(label)}}
				if !reflect.DeepEqual(r.calls, want) || !r.disabled[label] || r.loaded[label] {
					t.Fatalf("stop calls=%v disabled=%t loaded=%t", r.calls, r.disabled[label], r.loaded[label])
				}
				r.calls = nil
				if err := stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				if r.loaded[label] || !r.disabled[label] {
					t.Fatal("stop lost durable policy")
				}
			})
		}
	}
}
func TestDarwinServiceSelectedSnapshotRestore(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			p, r := newSelectedDarwinHarness(t)
			r.disabled[proxyAgentLabel] = !enabled
			r.loaded[proxyAgentLabel] = enabled
			before, err := p.SnapshotSelected(context.Background(), serviceProxy)
			if err != nil {
				t.Fatal(err)
			}
			refresh, _ := os.ReadFile(p.plistPath(agentLabel))
			refreshRuns := r.runs[agentLabel]
			if err := p.StopProxy(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := p.RestoreSelected(context.Background(), serviceProxy, before); err != nil {
				t.Fatal(err)
			}
			after, err := p.SnapshotSelected(context.Background(), serviceProxy)
			if err != nil || !sameServicePlatformSnapshot(before, after) {
				t.Fatalf("snapshot mismatch %v: %+v %+v", err, before, after)
			}
			now, _ := os.ReadFile(p.plistPath(agentLabel))
			if !bytes.Equal(now, refresh) || refreshRuns != r.runs[agentLabel] {
				t.Fatal("unselected refresh changed")
			}
			for _, call := range r.calls {
				if strings.Contains(strings.Join(call, " "), agentLabel) {
					t.Fatalf("unselected call %v", call)
				}
			}
		})
	}
	p, r := newSelectedDarwinHarness(t)
	before, _ := p.SnapshotSelected(context.Background(), serviceProxy)
	before.Components[0].ID = agentLabel
	r.calls = nil
	if p.RestoreSelected(context.Background(), serviceProxy, before) == nil || len(r.calls) != 0 {
		t.Fatal("invalid restore mutated manager")
	}
}
func TestDarwinServiceSelectedAbsenceAndAuthority(t *testing.T) {
	p, r := newSelectedDarwinHarness(t)
	if err := os.Remove(p.plistPath(proxyAgentLabel)); err != nil {
		t.Fatal(err)
	}
	r.loaded[proxyAgentLabel] = false
	status, err := p.InspectSelected(context.Background(), serviceProxy)
	if err != nil || status.Proxy.Observed == nil || status.Proxy.Registered || *status.Proxy.Observed.Enabled || *status.Proxy.Observed.Healthy {
		t.Fatalf("absence=%+v err=%v", status, err)
	}
	if !errors.Is(p.StartProxy(context.Background()), installstate.ErrNotInstalled) {
		t.Fatal("missing plist start succeeded")
	}
	r.loaded[proxyAgentLabel] = true
	if !errors.Is(p.PreflightSelected(context.Background(), p.executable, serviceProxy), installstate.ErrOwnershipConflict) {
		t.Fatal("unowned loaded job accepted")
	}
	p, r = newSelectedDarwinHarness(t)
	r.loaded[homebrewProxyAgentLabel] = true
	if !errors.Is(p.PreflightSelected(context.Background(), p.executable, serviceProxy), installstate.ErrOwnershipConflict) {
		t.Fatal("legacy job accepted")
	}
	r.calls = nil
	if err := p.PreflightSelected(context.Background(), p.executable, serviceRefresh); err != nil {
		t.Fatal(err)
	}
	for _, call := range r.calls {
		if strings.Contains(strings.Join(call, " "), proxyAgentLabel) || strings.Contains(strings.Join(call, " "), homebrewProxyAgentLabel) {
			t.Fatalf("unselected proxy call %v", call)
		}
	}
	p, _ = newSelectedDarwinHarness(t)
	path := p.plistPath(proxyAgentLabel)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(p.PreflightSelected(context.Background(), p.executable, serviceProxy), installstate.ErrOwnershipConflict) {
		t.Fatal("unsafe plist accepted")
	}
}
func TestDarwinServiceSelectedRollbackAfterSecondStepFailure(t *testing.T) {
	for _, operation := range []string{"start", "stop"} {
		t.Run(operation, func(t *testing.T) {
			p, r := newSelectedDarwinHarness(t)
			l, _, store := newSelectedServiceHarness(t)
			l.Platform = p
			l.Executable = p.executable
			store.record.Executable = p.executable
			store.record.Services = []string{proxyAgentLabel, agentLabel}
			if operation == "start" {
				r.disabled[proxyAgentLabel] = true
				r.loaded[proxyAgentLabel] = false
				r.failOnce["bootstrap\x00"+p.domain()+"\x00"+p.plistPath(proxyAgentLabel)] = errors.New("injected bootstrap failure")
			} else {
				r.failOnce["bootout\x00"+p.target(proxyAgentLabel)] = errors.New("injected bootout failure")
			}
			before, _ := p.SnapshotSelected(context.Background(), serviceProxy)
			r.calls = nil
			exit, data, _ := runSelectedService(t, context.Background(), l, "service", operation, "--component", "proxy", "--json")
			if exit == 0 || data.Rollback != "restored" {
				t.Fatalf("exit=%d data=%+v", exit, data)
			}
			after, err := p.SnapshotSelected(context.Background(), serviceProxy)
			if err != nil || !sameServicePlatformSnapshot(before, after) {
				t.Fatalf("rollback differs: %v", err)
			}
			for _, call := range r.calls {
				if strings.Contains(strings.Join(call, " "), agentLabel) {
					t.Fatalf("unselected call %v", call)
				}
			}
		})
	}
}
func TestDarwinServiceDisabledRefreshRestart(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		t.Run(fmt.Sprint(fresh), func(t *testing.T) {
			p, r := newSelectedDarwinHarness(t)
			r.disabled[agentLabel] = true
			r.loaded[agentLabel] = false
			d := readDarwinDefinition(t, p.plistPath(agentLabel))
			past := time.Now().Add(-time.Hour)
			if err := recordDarwinServiceRefresh(d, func() error { return nil }, func() time.Time { return past }); err != nil {
				t.Fatal(err)
			}
			if !fresh {
				p.runRefreshOnce = func(context.Context, darwinLaunchAgentDefinition) error { return nil }
			}
			l, _, store := newSelectedServiceHarness(t)
			l.Platform = p
			l.Executable = p.executable
			store.record.Executable = p.executable
			store.record.Services = []string{proxyAgentLabel, agentLabel}
			exit, data, _ := runSelectedService(t, context.Background(), l, "service", "restart", "--component", "token-refresh", "--json")
			if (exit == 0) != fresh || !r.disabled[agentLabel] || r.loaded[agentLabel] {
				t.Fatalf("fresh=%t exit=%d data=%+v", fresh, exit, data)
			}
			if fresh && (data.Components[0].Healthy == nil || *data.Components[0].Healthy || data.Components[0].LastRunAt == nil) {
				t.Fatalf("disabled completion=%+v", data)
			}
			for _, call := range r.calls {
				if call[0] == "enable" || call[0] == "bootstrap" || call[0] == "kickstart" {
					t.Fatalf("disabled scheduler activated: %v", call)
				}
			}
		})
	}
}
func TestDarwinServiceSelectedObservedRoots(t *testing.T) {
	p, _ := newSelectedDarwinHarness(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	status, err := p.InspectSelected(context.Background(), serviceProxy)
	if err != nil || status.Proxy.Observed.Roots == nil || *status.Proxy.Observed.Roots != p.roots {
		t.Fatalf("roots=%+v err=%v", status.Proxy.Observed, err)
	}
	d := readDarwinDefinition(t, p.plistPath(proxyAgentLabel))
	d.EnvironmentVariables = nil
	data, _ := renderDarwinLaunchAgent(d)
	if err := atomicWriteDarwinLaunchAgent(p.plistPath(proxyAgentLabel), data); err != nil {
		t.Fatal(err)
	}
	status, err = p.InspectSelected(context.Background(), serviceProxy)
	if err != nil || status.Proxy.Observed.Roots != nil {
		t.Fatalf("old plist invented roots: %+v %v", status, err)
	}
}
func TestDarwinServiceSelectedStopDeadline(t *testing.T) {
	p, r := newSelectedDarwinHarness(t)
	p.processAlive = func(int) bool { return true }
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := p.StopProxy(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop error=%v", err)
	}
	if !r.disabled[proxyAgentLabel] || r.loaded[proxyAgentLabel] {
		t.Fatal("stop did not persist disable before process wait")
	}
}

func TestDarwinServiceSelectedContextDoesNotLeak(t *testing.T) {
	p, r := newSelectedDarwinHarness(t)
	if _, err := p.InspectSelected(context.Background(), serviceRefresh); err != nil {
		t.Fatal(err)
	}
	if err := p.PreflightSelected(context.Background(), "/missing/cq", serviceRefresh); err == nil {
		t.Fatal("expected preflight failure")
	}
	r.calls = nil
	if err := p.RestartRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, [][]string{{"kickstart", "-k", p.target(agentLabel)}}) {
		t.Fatalf("legacy restart gained selected calls: %v", r.calls)
	}
	r.calls = nil
	if err := p.InstallRefresh(context.Background(), p.executable); err != nil {
		t.Fatal(err)
	}
	d := readDarwinDefinition(t, p.plistPath(agentLabel))
	if len(d.EnvironmentVariables) != 0 {
		t.Fatal("legacy install gained selected environment")
	}
	for _, call := range r.calls {
		if call[0] == "enable" || call[0] == "print-disabled" {
			t.Fatalf("legacy install gained selected calls: %v", call)
		}
	}
	snapshot, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinServiceSelectedFactoryUsesNativeHome(t *testing.T) {
	shellHome, nativeHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", shellHome)
	original := darwinSelectedHome
	darwinSelectedHome = func() (string, error) { return nativeHome, nil }
	t.Cleanup(func() { darwinSelectedHome = original })
	selected, err := selectedServiceLifecycleFactory(context.Background(), serviceInspect, serviceProxy)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := defaultDarwinServiceLifecycle("")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Platform.(*darwinServicePlatform).home != nativeHome || legacy.Platform.(*darwinServicePlatform).home != shellHome {
		t.Fatal("canonical/legacy home authority changed")
	}
}

func TestDarwinServiceDisabledProxyRestart(t *testing.T) {
	for _, scenario := range []string{"success", "no-op", "wrong-domain", "wrong-executable", "policy-changed", "kickstart-failed"} {
		t.Run(scenario, func(t *testing.T) {
			p, r := newSelectedDarwinHarness(t)
			r.disabled[proxyAgentLabel] = true
			r.loaded[proxyAgentLabel] = false
			run := p.run
			p.run = func(ctx context.Context, args ...string) ([]byte, error) {
				if args[0] == "load" {
					if scenario == "no-op" || scenario == "wrong-domain" {
						r.calls = append(r.calls, args)
						return nil, nil
					}
					output, err := run(ctx, args...)
					if scenario == "policy-changed" {
						r.disabled[proxyAgentLabel] = false
					}
					return output, err
				}
				return run(ctx, args...)
			}
			if scenario == "wrong-executable" {
				p.inspectSelectedProxy = func(context.Context, string, userdirs.Roots, int) componentStatus {
					return componentStatus{Healthy: false, LiveExecutable: "/wrong/cq"}
				}
			}
			if scenario == "kickstart-failed" {
				r.failOnce["kickstart\x00-k\x00"+p.target(proxyAgentLabel)] = errors.New("kickstart failed")
			}
			l, _, store := newSelectedServiceHarness(t)
			l.Platform = p
			l.Executable = p.executable
			store.record.Executable = p.executable
			store.record.Services = []string{proxyAgentLabel, agentLabel}
			exit, data, _ := runSelectedService(t, context.Background(), l, "service", "restart", "--component", "proxy", "--json")
			if (exit == 0) != (scenario == "success") || !r.disabled[proxyAgentLabel] {
				t.Fatalf("exit=%d data=%+v disabled=%t", exit, data, r.disabled[proxyAgentLabel])
			}
			if scenario == "success" {
				if !r.loaded[proxyAgentLabel] || data.Components[0].Healthy == nil || !*data.Components[0].Healthy || *data.Components[0].Enabled {
					t.Fatalf("disabled-running status=%+v", data)
				}
			} else if r.loaded[proxyAgentLabel] || data.Rollback != "restored" {
				t.Fatalf("failed restart not restored: %+v", data)
			}
			for _, call := range r.calls {
				if call[0] == "enable" || strings.Contains(strings.Join(call, " "), "-w") || strings.Contains(strings.Join(call, " "), agentLabel) {
					t.Fatalf("forbidden call %v", call)
				}
			}
		})
	}
}
func TestDarwinServiceDisabledRunningSnapshotRestore(t *testing.T) {
	p, r := newSelectedDarwinHarness(t)
	r.disabled[proxyAgentLabel] = true
	r.loaded[proxyAgentLabel] = true
	before, err := p.SnapshotSelected(context.Background(), serviceProxy)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StopProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreSelected(context.Background(), serviceProxy, before); err != nil {
		t.Fatal(err)
	}
	after, err := p.SnapshotSelected(context.Background(), serviceProxy)
	if err != nil || !sameServicePlatformSnapshot(before, after) {
		t.Fatalf("restore=%+v err=%v", after, err)
	}
	for _, call := range r.calls {
		if call[0] == "enable" {
			t.Fatal("restore enabled automatic launch")
		}
	}
}

func TestCLIV2MacServiceLifecycleMatrix(t *testing.T) {
	for _, action := range []string{"install", "start", "stop", "restart", "status", "uninstall"} {
		for _, selection := range []serviceSelection{serviceAll, serviceProxy, serviceRefresh} {
			t.Run(action+"/"+string(selection), func(t *testing.T) {
				p, r := newSelectedDarwinHarness(t)
				d := readDarwinDefinition(t, p.plistPath(agentLabel))
				past := time.Now().Add(-time.Minute)
				if err := recordDarwinServiceRefresh(d, func() error { return nil }, func() time.Time { return past }); err != nil {
					t.Fatal(err)
				}
				if action == "start" {
					for _, label := range darwinSelectedLabels(selection) {
						r.disabled[label] = true
						r.loaded[label] = false
					}
				}
				original := p.run
				p.run = func(ctx context.Context, args ...string) ([]byte, error) {
					out, err := original(ctx, args...)
					if err == nil && args[0] == "kickstart" && filepath.Base(args[2]) == agentLabel {
						current := readDarwinDefinition(t, p.plistPath(agentLabel))
						err = recordDarwinServiceRefresh(current, func() error { return nil }, time.Now)
					}
					return out, err
				}
				unselected := ""
				if selection == serviceProxy {
					unselected = agentLabel
				}
				if selection == serviceRefresh {
					unselected = proxyAgentLabel
				}
				var bytesBefore []byte
				var loadedBefore, disabledBefore bool
				if unselected != "" {
					bytesBefore, _ = os.ReadFile(p.plistPath(unselected))
					loadedBefore = r.loaded[unselected]
					disabledBefore = r.disabled[unselected]
				}
				l, _, store := newSelectedServiceHarness(t)
				l.Platform = p
				l.Executable = p.executable
				store.record.Executable = p.executable
				store.record.Services = []string{proxyAgentLabel, agentLabel}
				exit, data, _ := runSelectedService(t, context.Background(), l, "service", action, "--component", string(selection), "--json")
				if exit != 0 {
					t.Fatalf("exit=%d data=%+v", exit, data)
				}
				if len(data.Components) != len(selection.components()) {
					t.Fatalf("components=%+v", data)
				}
				if unselected != "" {
					after, _ := os.ReadFile(p.plistPath(unselected))
					if !bytes.Equal(bytesBefore, after) || loadedBefore != r.loaded[unselected] || disabledBefore != r.disabled[unselected] {
						t.Fatal("unselected service changed")
					}
					for _, call := range r.calls {
						if strings.Contains(strings.Join(call, " "), unselected) {
							t.Fatalf("unselected manager call: %v", call)
						}
					}
				}
			})
		}
	}
}
func TestDarwinServiceRejectsUnobservedDisabledState(t *testing.T) {
	for _, output := range []string{"", "not disabled services", "disabled services = {\n\"dev.jacobcx.cq.proxy\" => maybe\n}", "disabled services = {\n\"dev.jacobcx.cq.proxy\" => true\n\"dev.jacobcx.cq.proxy\" => false\n}"} {
		t.Run(fmt.Sprintf("%q", output), func(t *testing.T) {
			p, _ := newSelectedDarwinHarness(t)
			run := p.run
			p.run = func(ctx context.Context, args ...string) ([]byte, error) {
				if args[0] == "print-disabled" {
					return []byte(output), nil
				}
				return run(ctx, args...)
			}
			if _, err := p.InspectSelected(context.Background(), serviceProxy); !errors.Is(err, errServiceUnavailable) {
				t.Fatalf("unobserved policy accepted: %v", err)
			}
		})
	}
}

func TestDarwinServiceSelectedInstallRespectsCleanupDeadline(t *testing.T) {
	p, _ := newSelectedDarwinHarness(t)
	replacement := filepath.Join(p.home, "replacement-cq")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), selectedServiceContextKey{}, true))
	defer cancel()
	run := p.run
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			cancel()
			return nil, ctx.Err()
		}
		return run(ctx, args...)
	}
	if err := p.InstallProxy(ctx, replacement); !errors.Is(err, context.Canceled) {
		t.Fatalf("install=%v", err)
	}
	d := readDarwinDefinition(t, p.plistPath(proxyAgentLabel))
	if d.ProgramArguments[0] != replacement {
		t.Fatal("selected adapter rewrote definition during expired cleanup budget")
	}
}

func TestDarwinServiceSelectedInstallRollbackOwner(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			p, r := newSelectedDarwinHarness(t)
			d := readDarwinDefinition(t, p.plistPath(proxyAgentLabel))
			d.RunAtLoad = false
			before, _ := renderDarwinLaunchAgent(d)
			if err := atomicWriteDarwinLaunchAgent(p.plistPath(proxyAgentLabel), before); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			afterCancellation := 0
			run := p.run
			failed := false
			p.run = func(ctx context.Context, args ...string) ([]byte, error) {
				if ctx.Err() != nil {
					afterCancellation++
				}
				if args[0] == "bootstrap" && !failed {
					failed = true
					if cancelled {
						cancel()
						return nil, ctx.Err()
					}
					return nil, errors.New("injected failure")
				}
				return run(ctx, args...)
			}
			l, _, store := newSelectedServiceHarness(t)
			l.Platform = p
			l.Executable = p.executable
			store.record.Executable = p.executable
			store.record.Services = []string{proxyAgentLabel, agentLabel}
			exit, data, _ := runSelectedService(t, ctx, l, "service", "install", "--component", "proxy", "--json")
			after, err := os.ReadFile(p.plistPath(proxyAgentLabel))
			if err != nil {
				t.Fatal(err)
			}
			if cancelled {
				if exit != 130 || data.Rollback != "failed" || afterCancellation != 0 || bytes.Equal(before, after) || data.Components[0].Healthy != nil {
					t.Fatalf("cancelled exit=%d data=%+v latecalls=%d", exit, data, afterCancellation)
				}
			} else {
				if exit == 0 || data.Rollback != "restored" || !bytes.Equal(before, after) || r.disabled[proxyAgentLabel] || !r.loaded[proxyAgentLabel] {
					t.Fatalf("rollback exit=%d data=%+v", exit, data)
				}
			}
		})
	}
}

func TestDarwinServiceSelectedUninstallCancellation(t *testing.T) {
	for _, component := range []serviceSelection{serviceProxy, serviceRefresh} {
		t.Run(string(component), func(t *testing.T) {
			p, _ := newSelectedDarwinHarness(t)
			label := darwinSelectedLabels(component)[0]
			path := p.plistPath(label)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lateCalls := 0
			run := p.run
			p.run = func(ctx context.Context, args ...string) ([]byte, error) {
				if ctx.Err() != nil {
					lateCalls++
				}
				if args[0] == "bootout" && args[1] == p.target(label) {
					cancel()
					return nil, ctx.Err()
				}
				return run(ctx, args...)
			}
			l, _, store := newSelectedServiceHarness(t)
			l.Platform, l.Executable = p, p.executable
			store.record.Executable = p.executable
			store.record.Services = []string{proxyAgentLabel, agentLabel}
			exit, data, _ := runSelectedService(t, ctx, l, "service", "uninstall", "--component", string(component), "--json")
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("cancelled uninstall changed definition: %v", err)
			}
			if exit != 130 || data.Rollback != "failed" || lateCalls != 0 || data.Components[0].Healthy != nil {
				t.Fatalf("exit=%d data=%+v latecalls=%d", exit, data, lateCalls)
			}
		})
	}
}

func TestDarwinServiceLegacyUninstallCancellation(t *testing.T) {
	for _, label := range []string{proxyAgentLabel, agentLabel} {
		t.Run(label, func(t *testing.T) {
			p, _ := newSelectedDarwinHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.run = func(ctx context.Context, args ...string) ([]byte, error) { cancel(); return nil, ctx.Err() }
			remove := p.RemoveProxy
			if label == agentLabel {
				remove = p.RemoveRefresh
			}
			if err := remove(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("remove=%v", err)
			}
			if _, err := os.Stat(p.plistPath(label)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("legacy definition remains: %v", err)
			}
		})
	}
}

func TestDarwinServiceFIFOInputs(t *testing.T) {
	if kind := os.Getenv("CQ_TEST_SERVICE_FIFO_KIND"); kind != "" {
		path := os.Getenv("CQ_TEST_SERVICE_FIFO_PATH")
		var err error
		if kind == "log" {
			var file *os.File
			file, err = openDarwinRefreshLog(path)
			if file != nil {
				file.Close()
			}
		} else {
			p := &darwinServicePlatform{home: os.Getenv("CQ_TEST_SERVICE_FIFO_HOME")}
			_, _, err = p.ownedDefinition(proxyAgentLabel)
		}
		if err == nil {
			t.Fatal("FIFO accepted as a regular file")
		}
		return
	}
	for _, kind := range []string{"log", "plist"} {
		t.Run(kind, func(t *testing.T) {
			p, _ := newSelectedDarwinHarness(t)
			path := filepath.Join(p.roots.Logs, "fifo.log")
			if kind == "plist" {
				path = p.plistPath(proxyAgentLabel)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			// Isolate the potentially blocking open so a regression is killed and
			// reaped, rather than leaving a blocked goroutine in the test process.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDarwinServiceFIFOInputs$")
			command.Env = append(os.Environ(), "CQ_TEST_SERVICE_FIFO_KIND="+kind, "CQ_TEST_SERVICE_FIFO_PATH="+path, "CQ_TEST_SERVICE_FIFO_HOME="+p.home)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("FIFO open blocked until timeout: %v", ctx.Err())
			}
			if err != nil {
				t.Fatalf("FIFO rejection failed: %v: %s", err, output)
			}
		})
	}
}

func TestDarwinServiceFreshInstallExecutableAuthority(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o707, 0o700 | os.ModeSetuid, 0o700 | os.ModeSetgid} {
		for _, component := range []serviceSelection{serviceProxy, serviceRefresh} {
			t.Run(fmt.Sprintf("%s/%s", mode, component), func(t *testing.T) {
				p, r := newSelectedDarwinHarness(t)
				for _, label := range []string{proxyAgentLabel, agentLabel} {
					if err := os.Remove(p.plistPath(label)); err != nil {
						t.Fatal(err)
					}
					r.loaded[label] = false
				}
				if err := os.Chmod(p.executable, mode); err != nil {
					t.Fatal(err)
				}
				l, _, store := newSelectedServiceHarness(t)
				l.Platform, l.Executable = p, p.executable
				store.record.Executable = p.executable
				store.record.Services = []string{proxyAgentLabel, agentLabel}
				exit, _, _ := runSelectedService(t, context.Background(), l, "service", "install", "--component", string(component), "--json")
				if exit == 0 {
					t.Error("unsafe executable installation succeeded")
				}
				for _, call := range r.calls {
					if call[0] != "print" && call[0] != "print-disabled" {
						t.Errorf("unsafe install mutated manager: %v", call)
					}
				}
				for _, label := range []string{proxyAgentLabel, agentLabel} {
					if _, err := os.Stat(p.plistPath(label)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("unsafe install published definition: %s: %v", label, err)
					}
				}
				// The same adapter still applies the frozen legacy validator and
				// install semantics to an ordinary context; only the fake manager runs.
				if err := p.Preflight(context.Background(), p.executable); err != nil {
					t.Fatalf("legacy preflight changed: %v", err)
				}
				install := p.InstallProxy
				if component == serviceRefresh {
					install = p.InstallRefresh
				}
				if err := install(context.Background(), p.executable); err != nil {
					t.Fatalf("legacy install changed: %v", err)
				}
				label := darwinSelectedLabels(component)[0]
				if !r.loaded[label] {
					t.Fatal("legacy installation did not load fake service")
				}
			})
		}
	}
}
