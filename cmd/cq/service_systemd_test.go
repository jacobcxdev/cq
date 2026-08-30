package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
		{"--user", "enable", "--now", systemdProxyUnit, systemdRefreshTimer},
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
	keys := []string{"LoadState", "ActiveState", "SubState", "MainPID", "ExecStart", "FragmentPath", "UnitFileState", "Result", "NextElapseUSecRealtime"}
	var output strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&output, "%s=%s\n", key, values[key])
	}
	return []byte(output.String())
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
