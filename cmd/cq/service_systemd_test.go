package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/userdirs"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/installstate"
)

func TestSystemdServiceDefinitionsMatchGoldenFiles(t *testing.T) {
	executable := "/home/test/bin/cq"
	definitions, err := renderSystemdServiceDefinitions(executable)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range definitions {
		want, err := os.ReadFile(filepath.Join("testdata", "systemd", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s differs\ngot:\n%s\nwant:\n%s", name, got, want)
		}
	}
}

func TestSystemdServiceEscapesExecutableWithoutShell(t *testing.T) {
	executable := "/home/test/bin & tools/%cq"
	definitions, err := renderSystemdServiceDefinitions(executable)
	if err != nil {
		t.Fatal(err)
	}
	proxyUnit := string(definitions[systemdProxyUnit])
	if strings.Contains(proxyUnit, "ExecStart="+executable) || !strings.Contains(proxyUnit, `ExecStart="/home/test/bin & tools/%%cq" proxy start`) {
		t.Fatalf("proxy unit did not encode executable:\n%s", proxyUnit)
	}
	if strings.Contains(proxyUnit, "/bin/sh") || strings.Contains(proxyUnit, "${") {
		t.Fatalf("proxy unit uses shell indirection:\n%s", proxyUnit)
	}
}

func TestSystemdServiceSupportsCustomRefreshInterval(t *testing.T) {
	definitions, err := renderSystemdServiceDefinitionsWithInterval("/home/test/bin/cq", 75)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(definitions[systemdRefreshTimer]), "OnUnitActiveSec=75s") {
		t.Fatalf("refresh timer =\n%s", definitions[systemdRefreshTimer])
	}
}

func TestSystemdInstallProxyStartsUnitBeforeReturning(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)

	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatalf("InstallProxy() error = %v", err)
	}
	want := [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", systemdProxyUnit},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("systemctl calls = %#v\nwant = %#v", runner.calls, want)
	}
}

func TestSystemdServiceInstallUsesUserManagerInOrder(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	if err := platform.Preflight(context.Background(), platform.executable); err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatalf("InstallProxy() error = %v", err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatalf("InstallRefresh() error = %v", err)
	}

	wantCalls := [][]string{
		{"--user", "show-environment"},
		{"--user", "show", systemdProxyUnit, "--no-pager", "--property=" + strings.Join(systemdShowProperties, ",")},
		{"--user", "show", systemdRefreshService, "--no-pager", "--property=" + strings.Join(systemdShowProperties, ",")},
		{"--user", "show", systemdRefreshTimer, "--no-pager", "--property=" + strings.Join(systemdShowProperties, ",")},
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", systemdProxyUnit},
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", systemdRefreshTimer},
		{"--user", "start", systemdRefreshService},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("systemctl calls = %#v\nwant = %#v", runner.calls, wantCalls)
	}
	for _, name := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
		data, err := os.ReadFile(platform.unitPath(name))
		if err != nil {
			t.Fatal(err)
		}
		if name != systemdRefreshTimer && !strings.Contains(string(data), platform.executable) {
			t.Fatalf("%s omits executable", name)
		}
		if name == systemdRefreshTimer && !strings.Contains(string(data), "Unit="+systemdRefreshService) {
			t.Fatalf("%s omits refresh service", name)
		}
	}
	assertNoSystemdTemporaryFiles(t, platform.unitDirectory)
}

func TestSystemdServiceRestorePreservesRuntimeEnablement(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	definitions, err := renderSystemdServiceDefinitions(platform.executable)
	if err != nil {
		t.Fatal(err)
	}
	for name, definition := range definitions {
		if err := atomicWriteSystemdUnit(platform.unitPath(name), definition); err != nil {
			t.Fatal(err)
		}
	}
	runner.show[systemdProxyUnit] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "MainPID": "731", "UnitFileState": "enabled-runtime", "Result": "success",
	})
	runner.show[systemdRefreshService] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "MainPID": "0", "UnitFileState": "static", "Result": "success",
	})
	runner.show[systemdRefreshTimer] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "MainPID": "0", "UnitFileState": "disabled", "Result": "success",
	})
	snapshot, err := platform.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if err := platform.Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if !hasSystemdCall(runner.calls, []string{"--user", "enable", "--runtime", systemdProxyUnit}) {
		t.Fatalf("restore calls = %#v, want runtime enable", runner.calls)
	}
	if hasSystemdCall(runner.calls, []string{"--user", "enable", systemdProxyUnit}) {
		t.Fatalf("restore calls = %#v, persistent enable changed prior state", runner.calls)
	}
}

func TestSystemdServicePreflightRejectsUnavailableManagerBeforeWrites(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	runner.fail["--user\x00show-environment"] = errors.New("Failed to connect to bus")

	err := platform.Preflight(context.Background(), platform.executable)
	if err == nil || !strings.Contains(err.Error(), "user systemd manager") {
		t.Fatalf("Preflight() error = %v", err)
	}
	if entries, readErr := os.ReadDir(platform.unitDirectory); readErr == nil && len(entries) != 0 {
		t.Fatalf("unit files written before preflight: %v", entries)
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
}

func TestSystemdServicePreflightRejectsDifferentExecutable(t *testing.T) {
	platform, _ := newSystemdServiceHarness(t)
	definitions, err := renderSystemdServiceDefinitions("/home/other/bin/cq")
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteSystemdUnit(platform.unitPath(systemdProxyUnit), definitions[systemdProxyUnit]); err != nil {
		t.Fatal(err)
	}

	err = platform.Preflight(context.Background(), platform.executable)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Preflight() error = %v, want ownership conflict", err)
	}
}

func TestSystemdServicePreflightRejectsForeignLoadedFragment(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	runner.show[systemdProxyUnit] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "MainPID": "0", "FragmentPath": "/usr/lib/systemd/user/cq-proxy.service", "Result": "success",
	})

	err := platform.Preflight(context.Background(), platform.executable)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Preflight() error = %v, want ownership conflict", err)
	}
}

func TestSystemdServicePreflightRejectsModifiedRefreshTimer(t *testing.T) {
	platform, _ := newSystemdServiceHarness(t)
	if err := atomicWriteSystemdUnit(platform.unitPath(systemdRefreshTimer), []byte("[Timer]\nOnUnitActiveSec=1s\n")); err != nil {
		t.Fatal(err)
	}

	err := platform.Preflight(context.Background(), platform.executable)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Preflight() error = %v, want ownership conflict", err)
	}
}

func TestSystemdServiceRemovesUnitsIdempotently(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	if err := platform.Preflight(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil

	if err := platform.RemoveRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RemoveProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RemoveRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := platform.RemoveProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
		if _, err := os.Stat(platform.unitPath(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unit %s remains: %v", name, err)
		}
	}
}

func TestSystemdServiceInspectCombinesManagerAndRuntime(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	if err := platform.Preflight(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.show[systemdProxyUnit] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "MainPID": "731", "Result": "success",
	})
	runner.show[systemdRefreshService] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "MainPID": "0", "Result": "success",
	})
	runner.show[systemdRefreshTimer] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "waiting", "MainPID": "0", "Result": "success",
	})

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Proxy.Registered || !status.Proxy.Running || !status.Proxy.Healthy || status.Proxy.PID != 731 || status.Proxy.ConfiguredExecutable != platform.executable || status.Proxy.LiveExecutable != platform.executable || status.Proxy.Listener != "127.0.0.1:19280" {
		t.Fatalf("proxy status = %#v", status.Proxy)
	}
	if !status.Refresh.Registered || status.Refresh.Running || !status.Refresh.Healthy || status.Refresh.LastResult != "success" || status.Refresh.ConfiguredExecutable != platform.executable {
		t.Fatalf("refresh status = %#v", status.Refresh)
	}
}

func TestSystemdServiceInspectKeepsManagerPIDAuthoritative(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	if err := platform.Preflight(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.show[systemdProxyUnit] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "MainPID": "731", "Result": "success",
	})
	runner.show[systemdRefreshService] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "MainPID": "0", "Result": "success",
	})
	runner.show[systemdRefreshTimer] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "waiting", "MainPID": "0", "Result": "success",
	})
	platform.inspectProxy = func(_ context.Context, executable string) componentStatus {
		return componentStatus{Running: true, Healthy: true, PID: 999, LiveExecutable: executable, Listener: "127.0.0.1:19280"}
	}

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Proxy.PID != 731 || status.Proxy.Healthy || !strings.Contains(status.Proxy.Error, "PID") {
		t.Fatalf("proxy status = %#v", status.Proxy)
	}
}

func TestSystemdServiceInspectAcceptsAbsentUnitsWithoutMainPID(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	absent := []byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\n")
	for _, unit := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
		runner.show[unit] = absent
	}

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Proxy.Registered || status.Proxy.Running || status.Proxy.Healthy || status.Proxy.PID != 0 {
		t.Fatalf("proxy status = %#v", status.Proxy)
	}
	if status.Refresh.Registered || status.Refresh.Running || status.Refresh.Healthy {
		t.Fatalf("refresh status = %#v", status.Refresh)
	}
}

func TestSystemdServiceInspectAcceptsWaitingTimerWithoutMainPID(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	if err := platform.InstallRefresh(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.show[systemdProxyUnit] = systemdShow(map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "MainPID": "731", "Result": "success",
	})
	runner.show[systemdRefreshService] = []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nResult=success\n")
	runner.show[systemdRefreshTimer] = []byte("LoadState=loaded\nActiveState=active\nSubState=waiting\nResult=success\n")

	status, err := platform.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Proxy.Healthy || !status.Refresh.Healthy {
		t.Fatalf("status = %#v", status)
	}
}

func TestSystemdServiceInspectRejectsLoadedUnitWithoutMainPID(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.show[systemdProxyUnit] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n")

	_, err := platform.Inspect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "omitted MainPID") {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func TestSystemdServiceInspectRejectsRunningProxyWithZeroMainPID(t *testing.T) {
	platform, runner := newSystemdServiceHarness(t)
	if err := platform.InstallProxy(context.Background(), platform.executable); err != nil {
		t.Fatal(err)
	}
	runner.show[systemdProxyUnit] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=0\nResult=success\n")

	_, err := platform.Inspect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid MainPID") {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func newSystemdServiceHarness(t *testing.T) (*systemdServicePlatform, *fakeSystemctl) {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "cq")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeSystemctl{fail: map[string]error{}, show: map[string][]byte{}}
	platform := &systemdServicePlatform{
		unitDirectory: filepath.Join(root, "config", "systemd", "user"),
		executable:    executable,
		run:           runner.Run,
		inspectProxy: func(_ context.Context, executable string) componentStatus {
			return componentStatus{
				ID:             systemdProxyUnit,
				Manager:        "systemd-user",
				Running:        true,
				LiveExecutable: executable,
				PID:            731,
				Listener:       "127.0.0.1:19280",
				Healthy:        true,
			}
		},
	}
	return platform, runner
}

type fakeSystemctl struct {
	calls [][]string
	fail  map[string]error
	show  map[string][]byte
}

func (runner *fakeSystemctl) Run(_ context.Context, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string(nil), args...))
	if err := runner.fail[strings.Join(args, "\x00")]; err != nil {
		return nil, err
	}
	if len(args) >= 3 && args[0] == "--user" && args[1] == "show" {
		if output := runner.show[args[2]]; output != nil {
			return output, nil
		}
		return systemdShow(map[string]string{"LoadState": "not-found", "ActiveState": "inactive", "SubState": "dead", "MainPID": "0", "Result": "success"}), nil
	}
	return nil, nil
}

func systemdShow(values map[string]string) []byte {
	keys := []string{"LoadState", "ActiveState", "SubState", "MainPID", "ExecStart", "FragmentPath", "UnitFileState", "Result", "NextElapseUSecRealtime", "DropInPaths", "NeedDaemonReload", "ExecMainStartTimestamp", "ExecMainExitTimestamp", "ExecMainCode", "ExecMainStatus"}
	var output strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&output, "%s=%s\n", key, values[key])
	}
	return []byte(output.String())
}

func hasSystemdCall(calls [][]string, want []string) bool {
	for _, call := range calls {
		if reflect.DeepEqual(call, want) {
			return true
		}
	}
	return false
}

func assertNoSystemdTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary systemd unit remains: %s", entry.Name())
		}
	}
}

func init() {
	registerV2Fixture("linux-stop-refresh", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		p, runner := newSystemdServiceHarness(t)
		definitions, _ := renderSystemdServiceDefinitions(p.executable)
		for name, data := range definitions {
			if err := atomicWriteSystemdUnit(p.unitPath(name), data); err != nil {
				t.Fatal(err)
			}
			state, sub := "static", "running"
			if name != systemdRefreshService {
				state = "enabled"
			}
			if name == systemdRefreshTimer {
				sub = "waiting"
			}
			runner.show[name] = systemdShow(map[string]string{"LoadState": "loaded", "ActiveState": "active", "SubState": sub, "MainPID": "731", "UnitFileState": state, "Result": "success", "FragmentPath": p.unitPath(name)})
		}
		p.run = func(ctx context.Context, args ...string) ([]byte, error) {
			for _, arg := range args {
				if arg == systemdProxyUnit {
					f.Call("proxy-manager")
				}
			}
			if len(args) > 2 && args[1] == "disable" {
				f.Call("refresh-disable-timer")
				properties, _ := parseSystemdShow(runner.show[systemdRefreshTimer])
				properties["ActiveState"] = "inactive"
				properties["UnitFileState"] = "disabled"
				runner.show[systemdRefreshTimer] = systemdShow(properties)
			}
			if len(args) > 2 && args[1] == "stop" && args[2] == systemdRefreshService {
				f.Call("refresh-stop-job")
				properties, _ := parseSystemdShow(runner.show[systemdRefreshService])
				properties["ActiveState"] = "inactive"
				runner.show[systemdRefreshService] = systemdShow(properties)
			}
			return runner.Run(ctx, args...)
		}
		lifecycle := &serviceLifecycle{Platform: p, Store: &selectedServiceStore{exists: true, record: installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerManual, Version: "fixture", Executable: p.executable, BinaryDigest: strings.Repeat("a", 64), Services: []string{systemdProxyUnit, systemdRefreshTimer}}}, Version: "fixture", Executable: p.executable, StatusAttempts: 1, MutationLocker: &selectedServiceLock{}}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Service(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ServiceWithPreparation(ctx, inv, s, func(context.Context) (*serviceLifecycle, error) { return lifecycle, nil })
			}, ok
		}
		return f
	})
}
func TestCLIV2LinuxServiceStop(t *testing.T) {
	runV2Case(t, v2Case{Name: "Linux timer and job stop together", Scenario: "linux-stop-refresh", Args: []string{"service", "stop", "--component", "token-refresh", "--json"}, Exit: 0, Command: "service stop", WantJSON: `{"component":"token-refresh"}`, Forbid: []string{"proxy-manager"}, Calls: map[string]int{"refresh-disable-timer": 1, "refresh-stop-job": 1}})
}
func TestSystemdServiceSelectedStopOrder(t *testing.T) {
	_, p, r := newSelectedSystemdHarness(t)
	if err := p.StopRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 || !reflect.DeepEqual(r.calls[0], []string{"--user", "disable", "--now", systemdRefreshTimer}) || r.calls[1][1] != "show" || r.calls[1][2] != systemdRefreshTimer || !reflect.DeepEqual(r.calls[2], []string{"--user", "stop", systemdRefreshService}) {
		t.Fatalf("timer not disabled/stopped before job: %v", r.calls)
	}
}

// All manager state below is in memory; definitions live only in t.TempDir.
func newSelectedSystemdHarness(t *testing.T) (*serviceLifecycle, *systemdServicePlatform, *fakeSystemctl) {
	t.Helper()
	p, r := newSystemdServiceHarness(t)
	p.home = filepath.Dir(filepath.Dir(p.executable))
	config := filepath.Join(p.home, "config", "cq")
	state := filepath.Join(config, "state")
	p.roots = userdirs.Roots{Config: config, State: state, Runtime: state, Cache: filepath.Join(p.home, "cache", "cq"), Logs: filepath.Join(state, "logs")}
	p.inspectSelectedProxy = func(ctx context.Context, exe string, roots userdirs.Roots) componentStatus {
		if roots != p.roots {
			t.Fatalf("runtime roots=%+v want=%+v", roots, p.roots)
		}
		return p.inspectProxy(ctx, exe)
	}
	defs, err := renderSelectedSystemdDefinitions(p.executable, p.home, p.roots)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range defs {
		if err := atomicWriteSystemdUnit(p.unitPath(name), data); err != nil {
			t.Fatal(err)
		}
		active, sub, policy := "active", "running", "enabled"
		if name == systemdRefreshService {
			active, sub, policy = "inactive", "dead", "static"
		}
		if name == systemdRefreshTimer {
			sub = "waiting"
		}
		props := map[string]string{"LoadState": "loaded", "ActiveState": active, "SubState": sub, "MainPID": "731", "UnitFileState": policy, "Result": "success", "FragmentPath": p.unitPath(name), "NeedDaemonReload": "no"}
		if name == systemdRefreshService {
			setSystemdCompletion(props, time.Now().Add(-time.Minute), 0)
		}
		r.show[name] = systemdShow(props)
	}
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := r.Run(ctx, args...)
		if err != nil {
			return out, err
		}
		action := args[1]
		if action == "show" || action == "show-environment" {
			return out, nil
		}
		if action == "daemon-reload" {
			for _, name := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
				if _, err := os.Stat(p.unitPath(name)); errors.Is(err, os.ErrNotExist) {
					delete(r.show, name)
					continue
				}
				props, _ := parseSystemdShow(r.show[name])
				if props == nil {
					props = map[string]string{}
				}
				props["LoadState"] = "loaded"
				props["FragmentPath"] = p.unitPath(name)
				props["NeedDaemonReload"] = "no"
				if props["UnitFileState"] == "" {
					props["UnitFileState"] = "disabled"
					if name == systemdRefreshService {
						props["UnitFileState"] = "static"
					}
				}
				r.show[name] = systemdShow(props)
			}
			return nil, nil
		}
		name := args[len(args)-1]
		props, _ := parseSystemdShow(r.show[name])
		if props == nil {
			props = map[string]string{}
		}
		now := false
		for _, arg := range args {
			if arg == "--now" {
				now = true
			}
		}
		switch action {
		case "disable":
			runtimeFlag := false
			for _, arg := range args {
				runtimeFlag = runtimeFlag || arg == "--runtime"
			}
			if props["UnitFileState"] != "enabled-runtime" || runtimeFlag {
				props["UnitFileState"] = "disabled"
			}
			if now {
				props["ActiveState"] = "inactive"
				props["MainPID"] = "0"
			}
		case "enable":
			props["UnitFileState"] = "enabled"
			for _, arg := range args {
				if arg == "--runtime" {
					props["UnitFileState"] = "enabled-runtime"
				}
			}
			if now {
				props["ActiveState"] = "active"
				props["MainPID"] = "731"
			}
		case "stop":
			props["ActiveState"] = "inactive"
			props["MainPID"] = "0"
		case "start", "restart":
			if name == systemdRefreshService {
				props["ActiveState"] = "inactive"
				for _, arg := range args {
					if arg == "--no-block" {
						props["ActiveState"] = "activating"
					}
				}
				setSystemdCompletion(props, time.Now(), 0)
			} else {
				props["ActiveState"] = "active"
				props["MainPID"] = "731"
			}
		}
		r.show[name] = systemdShow(props)
		return nil, nil
	}
	store := &selectedServiceStore{exists: true, record: installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerManual, Version: "fixture", Executable: p.executable, BinaryDigest: strings.Repeat("a", 64), Services: []string{systemdProxyUnit, systemdRefreshTimer}}}
	l := &serviceLifecycle{Platform: p, Store: store, Executable: p.executable, Version: "fixture", StatusAttempts: 1, MutationLocker: &selectedServiceLock{}, DigestExecutable: func(string) (string, error) { return strings.Repeat("a", 64), nil }}
	return l, p, r
}
func setSystemdCompletion(props map[string]string, at time.Time, exit int) {
	const layout = "Mon 2006-01-02 15:04:05.000000 MST"
	props["ExecMainStartTimestamp"] = at.Add(-time.Second).UTC().Format(layout)
	props["ExecMainExitTimestamp"] = at.UTC().Format(layout)
	props["ExecMainCode"] = "1"
	props["ExecMainStatus"] = fmt.Sprint(exit)
	props["Result"] = "success"
	if exit != 0 {
		props["Result"] = "exit-code"
	}
}
func TestSystemdServiceSelectedLifecycleMatrix(t *testing.T) {
	for _, action := range []serviceAction{serviceInstall, serviceStart, serviceStop, serviceRestart, serviceInspect, serviceUninstall} {
		for _, selection := range []serviceSelection{serviceProxy, serviceRefresh, serviceAll} {
			t.Run(string(action)+"/"+string(selection), func(t *testing.T) {
				l, p, r := newSelectedSystemdHarness(t)
				if action == serviceStart {
					for _, name := range []string{systemdProxyUnit, systemdRefreshTimer} {
						props, _ := parseSystemdShow(r.show[name])
						props["UnitFileState"] = "disabled"
						props["ActiveState"] = "inactive"
						r.show[name] = systemdShow(props)
					}
				}
				before := map[string][]byte{}
				beforeState := map[string][]byte{}
				selected := systemdSelectedUnits(selection)
				for _, name := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
					before[name], _ = os.ReadFile(p.unitPath(name))
					beforeState[name] = append([]byte(nil), r.show[name]...)
				}
				result, err := l.Selected(context.Background(), action, selection, false)
				if err != nil {
					t.Fatalf("Selected: %v result=%+v", err, result)
				}
				for _, name := range []string{systemdProxyUnit, systemdRefreshService, systemdRefreshTimer} {
					isSelected := false
					for _, s := range selected {
						isSelected = isSelected || s == name
					}
					if isSelected {
						continue
					}
					data, err := os.ReadFile(p.unitPath(name))
					if err != nil || string(data) != string(before[name]) || string(r.show[name]) != string(beforeState[name]) {
						t.Fatalf("unselected %s changed", name)
					}
					for _, call := range r.calls {
						for _, arg := range call {
							if arg == name {
								t.Fatalf("unselected manager target: %v", call)
							}
						}
					}
				}
				if action == serviceUninstall {
					store := l.Store.(*selectedServiceStore)
					if selection == serviceProxy && !store.record.HasService(systemdRefreshTimer) {
						t.Fatal("refresh ownership removed")
					}
					if selection == serviceRefresh && !store.record.HasService(systemdProxyUnit) {
						t.Fatal("proxy ownership removed")
					}
				}
			})
		}
	}
}
func TestSystemdServiceSelectedDisabledRestart(t *testing.T) {
	for _, selection := range []serviceSelection{serviceProxy, serviceRefresh} {
		t.Run(string(selection), func(t *testing.T) {
			l, p, r := newSelectedSystemdHarness(t)
			if _, err := l.Selected(context.Background(), serviceStop, selection, false); err != nil {
				t.Fatal(err)
			}
			before, err := p.InspectSelected(context.Background(), selection)
			if err != nil {
				t.Fatal(err)
			}
			r.calls = nil
			result, err := l.Selected(context.Background(), serviceRestart, selection, false)
			if err != nil {
				t.Fatal(err)
			}
			c := result.Status.component(selection)
			if c.Observed.Enabled == nil || *c.Observed.Enabled {
				t.Fatal("restart enabled policy")
			}
			for _, call := range r.calls {
				if call[1] == "enable" {
					t.Fatalf("restart enabled unit: %v", call)
				}
			}
			if selection == serviceRefresh {
				if c.Observed.LastRunAt == nil || !c.Observed.LastRunAt.After(*before.Refresh.Observed.LastRunAt) {
					t.Fatal("restart did not observe new completion")
				}
				dto := projectV2ServiceComponent(selection, c, time.Now())
				if dto.Healthy == nil || *dto.Healthy {
					t.Fatalf("disabled refresh healthy=%v", dto.Healthy)
				}
			}
		})
	}
}
func TestSystemdServiceSelectedIdempotence(t *testing.T) {
	l, _, r := newSelectedSystemdHarness(t)
	if _, err := l.Selected(context.Background(), serviceStart, serviceAll, false); err != nil {
		t.Fatal(err)
	}
	for _, call := range r.calls {
		if call[1] == "start" || call[1] == "enable" {
			t.Fatalf("healthy start mutated: %v", call)
		}
	}
	if _, err := l.Selected(context.Background(), serviceStop, serviceAll, false); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if _, err := l.Selected(context.Background(), serviceStop, serviceAll, false); err != nil {
		t.Fatal(err)
	}
	for _, call := range r.calls {
		if call[1] == "stop" || call[1] == "disable" {
			t.Fatalf("stopped stop mutated: %v", call)
		}
	}
}
func TestSystemdServiceSelectedRollback(t *testing.T) {
	for _, selection := range []serviceSelection{serviceProxy, serviceRefresh, serviceAll} {
		t.Run(string(selection), func(t *testing.T) {
			l, p, r := newSelectedSystemdHarness(t)
			// Non-default native policy must survive selected restoration exactly.
			props, _ := parseSystemdShow(r.show[systemdProxyUnit])
			props["UnitFileState"] = "enabled-runtime"
			r.show[systemdProxyUnit] = systemdShow(props)
			props, _ = parseSystemdShow(r.show[systemdRefreshService])
			props["ActiveState"] = "active"
			r.show[systemdRefreshService] = systemdShow(props)
			before, err := p.SnapshotSelected(context.Background(), selection)
			if err != nil {
				t.Fatal(err)
			}
			unselected := map[string][]byte{}
			if selection == serviceProxy {
				unselected[systemdRefreshService] = append([]byte(nil), r.show[systemdRefreshService]...)
				unselected[systemdRefreshTimer] = append([]byte(nil), r.show[systemdRefreshTimer]...)
			} else if selection == serviceRefresh {
				unselected[systemdProxyUnit] = append([]byte(nil), r.show[systemdProxyUnit]...)
			}
			run := p.run
			failed := false
			p.run = func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := run(ctx, args...)
				if !failed && args[1] == "daemon-reload" {
					failed = true
					return nil, errors.New("reload failed after write")
				}
				return out, err
			}
			result, err := l.Selected(context.Background(), serviceInstall, selection, false)
			if err == nil || result.Rollback != "restored" {
				t.Fatalf("rollback=%s err=%v", result.Rollback, err)
			}
			after, err := p.SnapshotSelected(context.Background(), selection)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("snapshot restoration mismatch before=%+v after=%+v err=%v", before, after, err)
			}
			for name, want := range unselected {
				if string(r.show[name]) != string(want) {
					t.Fatalf("unselected %s state changed", name)
				}
			}
		})
	}
}
func TestSystemdServiceSelectedPreflightSafety(t *testing.T) {
	for _, mode := range []os.FileMode{0o666, 0o777, 0o755 | os.ModeSetuid, 0o755 | os.ModeSetgid} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			p, r := newSystemdServiceHarness(t)
			if err := os.Chmod(p.executable, mode); err != nil {
				t.Fatal(err)
			}
			if err := p.PreflightSelected(context.Background(), p.executable, serviceRefresh); err == nil {
				t.Fatal("unsafe executable accepted")
			}
			if len(r.calls) != 0 {
				t.Fatalf("manager launched before full executable validation: %v", r.calls)
			}
		})
	}
	for _, kind := range []string{"symlink", "hardlink", "directory", "fifo", "writable", "modified", "dropin", "foreign-fragment"} {
		t.Run(kind, func(t *testing.T) {
			_, p, r := newSelectedSystemdHarness(t)
			path := p.unitPath(systemdRefreshService)
			switch kind {
			case "symlink", "hardlink":
				data, _ := os.ReadFile(path)
				other := filepath.Join(p.home, "other")
				if err := os.WriteFile(other, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(other, path)
				} else {
					err = os.Link(other, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if runtime.GOOS == "windows" {
					t.Skip("Unix FIFO")
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := exec.Command("mkfifo", path).Run(); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "modified":
				if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/true\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				props, _ := parseSystemdShow(r.show[systemdRefreshService])
				if kind == "dropin" {
					props["DropInPaths"] = "/tmp/foreign.conf"
				} else {
					props["FragmentPath"] = "/usr/lib/systemd/user/cq-refresh.service"
				}
				r.show[systemdRefreshService] = systemdShow(props)
			}
			started := time.Now()
			err := p.PreflightSelected(context.Background(), p.executable, serviceRefresh)
			if !errors.Is(err, installstate.ErrOwnershipConflict) {
				t.Fatalf("err=%v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("unsafe input blocked")
			}
			for _, call := range r.calls {
				for _, arg := range call {
					if arg == systemdProxyUnit {
						t.Fatal("unselected proxy inspected")
					}
				}
			}
		})
	}
}
func TestSystemdServiceSelectedUnavailableAndCancellation(t *testing.T) {
	_, p, r := newSelectedSystemdHarness(t)
	r.fail["--user\x00show-environment"] = errors.New("no user manager")
	if err := p.PreflightSelected(context.Background(), p.executable, serviceProxy); !errors.Is(err, errServiceUnavailable) {
		t.Fatalf("unavailable err=%v", err)
	}
	for _, operation := range []string{"install", "remove", "restore", "start", "restart", "preflight"} {
		t.Run(operation, func(t *testing.T) {
			_, p, r := newSelectedSystemdHarness(t)
			before, err := p.SnapshotSelected(context.Background(), serviceRefresh)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), selectedServiceContextKey{}, true))
			run := p.run
			p.run = func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := run(ctx, args...)
				cancel()
				return out, err
			}
			switch operation {
			case "install":
				err = p.InstallRefresh(ctx, p.executable)
			case "remove":
				err = p.RemoveRefresh(ctx)
			case "restore":
				err = p.RestoreSelected(ctx, serviceRefresh, before)
			case "start":
				err = p.StartRefresh(ctx)
			case "restart":
				err = p.RestartRefresh(ctx)
			case "preflight":
				err = p.PreflightSelected(ctx, p.executable, serviceRefresh)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v", err)
			}
			for _, c := range before.Components {
				data, readErr := os.ReadFile(p.unitPath(c.ID))
				if readErr != nil || string(data) != string(c.Definition) {
					t.Fatal("definition changed after cancelled manager call")
				}
			}
			// Exactly one boundary is reached before cancellation; later native mutations stop.
			if len(r.calls) != 3 {
				t.Fatalf("calls=%v", r.calls)
			} // Two initial snapshot reads plus one operation call.
		})
	}
}
func TestSystemdServiceSelectedCompletion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, test := range []struct {
		name    string
		age     time.Duration
		exit    int
		mutate  func(map[string]string)
		state   string
		healthy *bool
	}{
		{name: "idle", age: time.Minute, state: "idle", healthy: serviceBool(true)},
		{name: "boundary", age: 35 * time.Minute, state: "idle", healthy: serviceBool(true)},
		{name: "stale", age: 35*time.Minute + time.Microsecond, state: "idle", healthy: serviceBool(false)},
		{name: "failed", age: time.Minute, exit: 2, state: "failed", healthy: serviceBool(false)},
		{name: "never completed", mutate: func(p map[string]string) { p["ExecMainExitTimestamp"] = "" }, state: "indeterminate"},
		{name: "default status is not completion", age: time.Minute, mutate: func(p map[string]string) { p["ExecMainCode"] = "0" }, state: "failed", healthy: serviceBool(false)},
		{name: "current run", age: time.Minute, mutate: func(p map[string]string) {
			p["ExecMainStartTimestamp"] = now.UTC().Format("Mon 2006-01-02 15:04:05.000000 MST")
		}, state: "failed", healthy: serviceBool(false)},
		{name: "invalid timestamp", age: time.Minute, mutate: func(p map[string]string) { p["ExecMainExitTimestamp"] = "yesterday" }, state: "failed", healthy: serviceBool(false)},
		{name: "signal", age: time.Minute, mutate: func(p map[string]string) { p["ExecMainCode"] = "2"; p["ExecMainStatus"] = "15"; p["Result"] = "signal" }, state: "failed", healthy: serviceBool(false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, p, r := newSelectedSystemdHarness(t)
			props, _ := parseSystemdShow(r.show[systemdRefreshService])
			setSystemdCompletion(props, now.Add(-test.age), test.exit)
			if test.mutate != nil {
				test.mutate(props)
			}
			r.show[systemdRefreshService] = systemdShow(props)
			status, err := p.InspectSelected(context.Background(), serviceRefresh)
			if err != nil {
				t.Fatal(err)
			}
			dto := projectV2ServiceComponent(serviceRefresh, status.Refresh, now)
			if dto.State != test.state || !reflect.DeepEqual(dto.Healthy, test.healthy) {
				t.Fatalf("state=%s healthy=%v want %s %v", dto.State, dto.Healthy, test.state, test.healthy)
			}
		})
	}
}
func TestSystemdServiceSelectedRootsAndLegacy(t *testing.T) {
	l, p, r := newSelectedSystemdHarness(t)
	t.Setenv("HOME", "/shell/spoof")
	t.Setenv("XDG_CONFIG_HOME", "/shell/config")
	t.Setenv("XDG_CACHE_HOME", "/shell/cache")
	status, err := p.InspectSelected(context.Background(), serviceAll)
	if err != nil {
		t.Fatal(err)
	}
	if *status.Proxy.Observed.Roots != p.roots || *status.Refresh.Observed.Roots != p.roots {
		t.Fatal("installed roots replaced by shell")
	}
	legacy, _ := renderSystemdServiceDefinitions(p.executable)
	if err := atomicWriteSystemdUnit(p.unitPath(systemdRefreshService), legacy[systemdRefreshService]); err != nil {
		t.Fatal(err)
	}
	status, err = p.InspectSelected(context.Background(), serviceRefresh)
	if err != nil {
		t.Fatal(err)
	}
	if status.Refresh.Observed.Roots != nil || projectV2ServiceComponent(serviceRefresh, status.Refresh, time.Now()).State != "indeterminate" {
		t.Fatal("legacy roots guessed")
	}
	if _, err := l.Selected(context.Background(), serviceInspect, serviceRefresh, true); !errors.Is(err, ErrServiceUnhealthy) {
		t.Fatalf("strict legacy=%v", err)
	}
	if _, err := l.Selected(context.Background(), serviceInstall, serviceRefresh, false); err != nil {
		t.Fatalf("explicit reinstall=%v", err)
	}
	// Selected inspection and failed preflight cannot poison a later legacy call.
	if err := p.PreflightSelected(context.Background(), "/missing", serviceRefresh); err == nil {
		t.Fatal("bad preflight succeeded")
	}
	r.calls = nil
	if err := p.InstallRefresh(context.Background(), p.executable); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.unitPath(systemdRefreshService))
	if string(data) != string(legacy[systemdRefreshService]) {
		t.Fatal("selected context leaked into legacy install")
	}
}
func TestSystemdServiceSelectedEscapedRoots(t *testing.T) {
	_, p, _ := newSelectedSystemdHarness(t)
	home := filepath.Join(p.home, `space & % "quote" \ slash`)
	config := filepath.Join(home, "cfg", "cq")
	state := filepath.Join(config, "state")
	roots := userdirs.Roots{Config: config, State: state, Runtime: state, Cache: filepath.Join(home, "cache", "cq"), Logs: filepath.Join(state, "logs")}
	defs, err := renderSelectedSystemdDefinitions(p.executable, home, roots)
	if err != nil {
		t.Fatal(err)
	}
	gotHome, gotRoots, err := systemdDefinitionRoots(defs[systemdRefreshService])
	if err != nil || gotHome != home || gotRoots == nil || *gotRoots != roots {
		t.Fatalf("root escaping: %q %+v %v", gotHome, gotRoots, err)
	}
	if _, _, err := validateOwnedSystemdDefinition(systemdRefreshService, defs[systemdRefreshService]); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdServiceSelectedAbsentAndInvalidNativeState(t *testing.T) {
	for _, selection := range []serviceSelection{serviceProxy, serviceRefresh, serviceAll} {
		p, _ := newSystemdServiceHarness(t)
		status, err := p.InspectSelected(context.Background(), selection)
		if err != nil {
			t.Fatal(err)
		}
		for _, component := range selection.components() {
			c := status.component(component)
			if c.Observed == nil || c.Registered || c.Observed.Enabled == nil || *c.Observed.Enabled {
				t.Fatalf("absence not observed: %+v", c)
			}
		}
	}
	for _, scenario := range []string{"partial pair", "masked", "pid mismatch", "missing pid", "missing dropin evidence", "stale manager definition", "unknown timer state", "timer stopped", "bad show"} {
		t.Run(scenario, func(t *testing.T) {
			_, p, r := newSelectedSystemdHarness(t)
			selection := serviceProxy
			props, _ := parseSystemdShow(r.show[systemdProxyUnit])
			wantError := true
			switch scenario {
			case "partial pair":
				selection = serviceRefresh
				if err := os.Remove(p.unitPath(systemdRefreshTimer)); err != nil {
					t.Fatal(err)
				}
				delete(r.show, systemdRefreshTimer)
			case "masked":
				props["UnitFileState"] = "masked"
			case "pid mismatch":
				props["MainPID"] = "999"
				wantError = false
			case "missing pid":
				props["MainPID"] = ""
			case "stale manager definition":
				props["NeedDaemonReload"] = "yes"
			case "missing dropin evidence":
			case "bad show":
			case "unknown timer state":
				selection = serviceRefresh
				values, _ := parseSystemdShow(r.show[systemdRefreshTimer])
				values["UnitFileState"] = "indirect"
				r.show[systemdRefreshTimer] = systemdShow(values)
			case "timer stopped":
				selection = serviceRefresh
				values, _ := parseSystemdShow(r.show[systemdRefreshTimer])
				values["ActiveState"] = "inactive"
				r.show[systemdRefreshTimer] = systemdShow(values)
				wantError = false
			}
			r.show[systemdProxyUnit] = systemdShow(props)
			if scenario == "missing dropin evidence" {
				r.show[systemdProxyUnit] = []byte(strings.ReplaceAll(string(r.show[systemdProxyUnit]), "DropInPaths=\n", ""))
			}
			if scenario == "bad show" {
				r.show[systemdProxyUnit] = []byte("not-properties")
			}
			status, err := p.InspectSelected(context.Background(), selection)
			if wantError && err == nil {
				t.Fatal("invalid native state accepted")
			}
			if !wantError {
				if err != nil {
					t.Fatal(err)
				}
				dto := projectV2ServiceComponent(selection, status.component(selection), time.Now())
				if dto.Healthy == nil || *dto.Healthy {
					t.Fatal("invalid running authority reported healthy")
				}
			}
		})
	}
}
func TestSystemdServiceSelectedRestartRejectsStaleCompletion(t *testing.T) {
	l, p, _ := newSelectedSystemdHarness(t)
	if _, err := l.Selected(context.Background(), serviceStop, serviceRefresh, false); err != nil {
		t.Fatal(err)
	}
	run := p.run
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[1] == "restart" {
			return nil, nil
		}
		return run(ctx, args...)
	}
	result, err := l.Selected(context.Background(), serviceRestart, serviceRefresh, false)
	if !errors.Is(err, ErrServiceUnhealthy) || result.Rollback != "restored" {
		t.Fatalf("stale restart accepted: %v rollback=%s", err, result.Rollback)
	}
}
func TestSystemdServiceSelectedSnapshotValidationBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"unselected", "foreign executable", "bad timer", "oversize", "absent state"} {
		t.Run(scenario, func(t *testing.T) {
			_, p, r := newSelectedSystemdHarness(t)
			snapshot, err := p.SnapshotSelected(context.Background(), serviceRefresh)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unselected":
				snapshot.Components[0].ID = systemdProxyUnit
			case "foreign executable":
				snapshot.Components[0].Definition = []byte("[Service]\nExecStart=/bin/false\n")
			case "bad timer":
				snapshot.Components[1].Definition = []byte("[Timer]\nUnit=other.service\n")
			case "oversize":
				snapshot.Components[0].Definition = make([]byte, maxSystemdUnitBytes+1)
			case "absent state":
				snapshot.Components[0].Exists = false
			}
			r.calls = nil
			if err := p.RestoreSelected(context.Background(), serviceRefresh, snapshot); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
			if len(r.calls) > 0 {
				t.Fatalf("invalid snapshot launched manager: %v", r.calls)
			}
		})
	}
}
func TestSystemdServiceSelectedInstallRollbackToAbsence(t *testing.T) {
	l, p, r := newSelectedSystemdHarness(t)
	for _, name := range systemdSelectedUnits(serviceRefresh) {
		if err := os.Remove(p.unitPath(name)); err != nil {
			t.Fatal(err)
		}
		delete(r.show, name)
	}
	l.Store.(*selectedServiceStore).record.Services = []string{systemdProxyUnit}
	beforeProxy, _ := os.ReadFile(p.unitPath(systemdProxyUnit))
	run := p.run
	failed := false
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := run(ctx, args...)
		if !failed && args[1] == "start" {
			failed = true
			return nil, errors.New("job failed")
		}
		return out, err
	}
	result, err := l.Selected(context.Background(), serviceInstall, serviceRefresh, false)
	if err == nil || result.Rollback != "restored" {
		t.Fatalf("install rollback=%s err=%v", result.Rollback, err)
	}
	for _, name := range systemdSelectedUnits(serviceRefresh) {
		if _, err := os.Stat(p.unitPath(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("new selected definition survived rollback")
		}
	}
	afterProxy, _ := os.ReadFile(p.unitPath(systemdProxyUnit))
	if string(beforeProxy) != string(afterProxy) {
		t.Fatal("unselected proxy changed")
	}
}
func TestSystemdServiceSelectedLegacyInterval(t *testing.T) {
	_, p, _ := newSelectedSystemdHarness(t)
	defs, err := renderSystemdServiceDefinitionsWithInterval(p.executable, 75)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateOwnedSystemdDefinition(systemdRefreshTimer, defs[systemdRefreshTimer]); err != nil {
		t.Fatal(err)
	}
}
func TestSystemdServiceSelectedCancelledBeforeWrites(t *testing.T) {
	_, p, r := newSelectedSystemdHarness(t)
	snapshot, err := p.SnapshotSelected(context.Background(), serviceRefresh)
	if err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), selectedServiceContextKey{}, true))
	cancel()
	for _, action := range []func() error{func() error { return p.InstallRefresh(ctx, p.executable) }, func() error { return p.RemoveRefresh(ctx) }, func() error { return p.RestoreSelected(ctx, serviceRefresh, snapshot) }} {
		if err := action(); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	}
	if len(r.calls) > 0 {
		t.Fatalf("cancelled operation launched manager: %v", r.calls)
	}
	for _, c := range snapshot.Components {
		data, err := os.ReadFile(p.unitPath(c.ID))
		if err != nil || string(data) != string(c.Definition) {
			t.Fatal("cancelled operation rewrote definitions")
		}
	}
}

func TestSystemdServiceSelectedRuntimeEnablement(t *testing.T) {
	for _, selection := range []serviceSelection{serviceProxy, serviceRefresh} {
		t.Run(string(selection), func(t *testing.T) {
			l, p, r := newSelectedSystemdHarness(t)
			name := systemdProxyUnit
			if selection == serviceRefresh {
				name = systemdRefreshTimer
			}
			props, _ := parseSystemdShow(r.show[name])
			props["UnitFileState"] = "enabled-runtime"
			r.show[name] = systemdShow(props)
			if _, err := l.Selected(context.Background(), serviceStop, selection, false); err != nil {
				t.Fatal(err)
			}
			if !hasSystemdCall(r.calls, []string{"--user", "disable", "--runtime", "--now", name}) {
				t.Fatal("runtime enablement not cleared")
			}
			state, err := p.InspectSelected(context.Background(), selection)
			if err != nil || *state.component(selection).Observed.Enabled {
				t.Fatalf("runtime still enabled: %v", err)
			}
		})
	}
	_, p, r := newSelectedSystemdHarness(t)
	props, _ := parseSystemdShow(r.show[systemdProxyUnit])
	props["UnitFileState"] = "linked-runtime"
	r.show[systemdProxyUnit] = systemdShow(props)
	if _, err := p.InspectSelected(context.Background(), serviceProxy); err == nil {
		t.Fatal("linked-runtime ownership accepted")
	}
	for _, call := range r.calls {
		if call[1] != "show" {
			t.Fatalf("foreign linked state mutated: %v", call)
		}
	}
}

func TestSystemdServiceSelectedRollbackBeforeManagerReload(t *testing.T) {
	l, p, r := newSelectedSystemdHarness(t)
	for _, name := range systemdSelectedUnits(serviceRefresh) {
		if err := os.Remove(p.unitPath(name)); err != nil {
			t.Fatal(err)
		}
		delete(r.show, name)
	}
	l.Store.(*selectedServiceStore).record.Services = []string{systemdProxyUnit}
	run := p.run
	failed := false
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if !failed && args[1] == "daemon-reload" {
			failed = true
			return nil, errors.New("reload unavailable before loading new definitions")
		}
		if (args[1] == "disable" || args[1] == "stop") && r.show[args[len(args)-1]] == nil {
			return nil, errors.New("Unit not loaded")
		}
		return run(ctx, args...)
	}
	result, err := l.Selected(context.Background(), serviceInstall, serviceRefresh, false)
	if err == nil || result.Rollback != "restored" {
		t.Fatalf("rollback=%s error=%v", result.Rollback, err)
	}
	for _, name := range systemdSelectedUnits(serviceRefresh) {
		if _, err := os.Stat(p.unitPath(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unloaded definition survived rollback")
		}
	}
}

func TestSystemdServiceSelectedRetainedCompletion(t *testing.T) {
	for _, scenario := range []string{"running", "native history collected"} {
		t.Run(scenario, func(t *testing.T) {
			_, p, r := newSelectedSystemdHarness(t)
			past := time.Now().Add(-time.Minute)
			if err := recordServiceRefresh(p.executable, p.roots, func() error { return nil }, func() time.Time { return past }); err != nil {
				t.Fatal(err)
			}
			props, _ := parseSystemdShow(r.show[systemdRefreshService])
			props["ExecMainExitTimestamp"] = ""
			props["ExecMainCode"] = "0"
			props["ExecMainStatus"] = "0"
			if scenario == "running" {
				props["ActiveState"] = "activating"
				props["ExecMainStartTimestamp"] = time.Now().UTC().Format("Mon 2006-01-02 15:04:05.000000 MST")
			}
			r.show[systemdRefreshService] = systemdShow(props)
			status, err := p.InspectSelected(context.Background(), serviceRefresh)
			if err != nil {
				t.Fatal(err)
			}
			if status.Refresh.Observed.LastRunAt == nil || !status.Refresh.Observed.LastRunAt.Equal(past) {
				t.Fatal("previous completed run was lost")
			}
			dto := projectV2ServiceComponent(serviceRefresh, status.Refresh, time.Now())
			if dto.Healthy == nil || !*dto.Healthy {
				t.Fatalf("prior completion did not retain health: %+v", dto)
			}
		})
	}
}

func TestSystemdServiceSelectedReceiptFailurePrecedence(t *testing.T) {
	for _, scenario := range []string{"new native failure", "new native signal", "failure without timestamp", "malformed receipt", "null exit", "foreign receipt roots", "duplicate exit", "duplicate identical root", "first run"} {
		t.Run(scenario, func(t *testing.T) {
			_, p, r := newSelectedSystemdHarness(t)
			past := time.Now().Add(-2 * time.Minute)
			if scenario != "first run" {
				if err := recordServiceRefresh(p.executable, p.roots, func() error { return nil }, func() time.Time { return past }); err != nil {
					t.Fatal(err)
				}
			}
			props, _ := parseSystemdShow(r.show[systemdRefreshService])
			path := filepath.Join(p.roots.State, serviceRefreshCompletionName)
			switch scenario {
			case "new native failure":
				setSystemdCompletion(props, time.Now().Add(-time.Minute), 7)
			case "new native signal":
				setSystemdCompletion(props, time.Now().Add(-time.Minute), 15)
				props["ExecMainCode"] = "2"
				props["Result"] = "signal"
			case "failure without timestamp":
				props["ExecMainExitTimestamp"] = ""
				props["Result"] = "exit-code"
			case "first run":
				props["ExecMainExitTimestamp"] = ""
				props["ExecMainCode"] = "0"
				props["ActiveState"] = "activating"
			default:
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "malformed receipt" {
					data = []byte("not json")
				}
				if scenario == "null exit" {
					data = []byte(strings.Replace(string(data), `"exit_code":0`, `"exit_code":null`, 1))
				}
				if scenario == "foreign receipt roots" {
					data = []byte(strings.ReplaceAll(string(data), p.roots.State, "/other/state"))
				}
				if scenario == "duplicate exit" {
					data = []byte(strings.Replace(string(data), `"exit_code":0`, `"exit_code":0,"exit_code":0`, 1))
				}
				if scenario == "duplicate identical root" {
					root, err := json.Marshal(p.roots.Config)
					if err != nil {
						t.Fatal(err)
					}
					field := `"Config":` + string(root)
					data = []byte(strings.Replace(string(data), field, field+","+field, 1))
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r.show[systemdRefreshService] = systemdShow(props)
			status, err := p.InspectSelected(context.Background(), serviceRefresh)
			if err != nil {
				t.Fatal(err)
			}
			dto := projectV2ServiceComponent(serviceRefresh, status.Refresh, time.Now())
			if scenario == "first run" {
				if dto.Healthy != nil || dto.LastRunAt != nil {
					t.Fatal("first run reused success")
				}
			} else if dto.Healthy == nil || *dto.Healthy {
				t.Fatalf("failure hidden by retained success: %+v", dto)
			}
			if scenario == "new native failure" && (dto.LastExitCode == nil || *dto.LastExitCode != 7) {
				t.Fatal("native failure exit lost")
			}
			if scenario == "new native signal" && (dto.LastExitCode == nil || *dto.LastExitCode != 143) {
				t.Fatal("native signal exit lost")
			}
		})
	}
}
