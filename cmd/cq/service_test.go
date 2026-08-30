package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestServiceInstallPersistsOnlyHealthyComponents(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)

	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if !platform.proxyRegistered || !platform.refreshRegistered {
		t.Fatalf("registered proxy/refresh = %t/%t", platform.proxyRegistered, platform.refreshRegistered)
	}
	if !platform.proxyRunning || platform.refreshRuns != 1 {
		t.Fatalf("proxy running/refresh runs = %t/%d", platform.proxyRunning, platform.refreshRuns)
	}
	record, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	wantServices := []string{"test.proxy", "test.refresh"}
	if record.Owner != installstate.OwnerGo || record.Version != "0.27.0" || record.Executable != lifecycle.Executable || !reflect.DeepEqual(record.Services, wantServices) {
		t.Fatalf("install record = %#v, want owner/version/path/services", record)
	}
	wantCalls := []string{"preflight", "inspect", "install-proxy", "install-refresh", "inspect"}
	if !reflect.DeepEqual(platform.calls, wantCalls) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, wantCalls)
	}
}

func TestServiceInstallIsIdempotent(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	platform.calls = nil

	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatalf("Install() repeated error = %v", err)
	}
	if platform.refreshRuns != 2 {
		t.Fatalf("refresh runs = %d, want 2", platform.refreshRuns)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	wantCalls := []string{"preflight", "inspect", "install-proxy", "install-refresh", "inspect"}
	if !reflect.DeepEqual(platform.calls, wantCalls) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, wantCalls)
	}
}

func TestServiceInstallRollsBackNewProxyWhenRefreshFails(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	platform.installRefreshErr = errors.New("refresh registration failed")

	err := lifecycle.Install(context.Background(), installstate.OwnerGo)
	if !errors.Is(err, platform.installRefreshErr) {
		t.Fatalf("Install() error = %v, want refresh error", err)
	}
	if platform.proxyRegistered || platform.refreshRegistered {
		t.Fatalf("registered proxy/refresh after rollback = %t/%t", platform.proxyRegistered, platform.refreshRegistered)
	}
	if _, err := store.Load(); !errors.Is(err, installstate.ErrNotInstalled) {
		t.Fatalf("Load() error = %v, want not installed", err)
	}
	wantCalls := []string{"preflight", "inspect", "install-proxy", "install-refresh", "remove-refresh", "remove-proxy"}
	if !reflect.DeepEqual(platform.calls, wantCalls) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, wantCalls)
	}
}

func TestServiceInstallPreservesPreexistingProxyOnRefreshFailure(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	platform.proxyRegistered = true
	platform.proxyRunning = true
	platform.installRefreshErr = errors.New("refresh registration failed")

	err := lifecycle.Install(context.Background(), installstate.OwnerGo)
	if !errors.Is(err, platform.installRefreshErr) {
		t.Fatalf("Install() error = %v, want refresh error", err)
	}
	if !platform.proxyRegistered || !platform.proxyRunning {
		t.Fatalf("pre-existing proxy removed: registered/running = %t/%t", platform.proxyRegistered, platform.proxyRunning)
	}
	for _, call := range platform.calls {
		if call == "remove-proxy" {
			t.Fatalf("pre-existing proxy rollback call = %v", platform.calls)
		}
	}
}

func TestServiceInstallRollsBackWhenStatusStaysUnhealthy(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	platform.proxyHealthy = false
	lifecycle.StatusAttempts = 2

	err := lifecycle.Install(context.Background(), installstate.OwnerGo)
	if !errors.Is(err, ErrServiceUnhealthy) {
		t.Fatalf("Install() error = %v, want ErrServiceUnhealthy", err)
	}
	if platform.proxyRegistered || platform.refreshRegistered {
		t.Fatalf("registered proxy/refresh after rollback = %t/%t", platform.proxyRegistered, platform.refreshRegistered)
	}
	if _, err := store.Load(); !errors.Is(err, installstate.ErrNotInstalled) {
		t.Fatalf("Load() error = %v, want not installed", err)
	}
	if platform.inspectCalls != 3 {
		t.Fatalf("inspect calls = %d, want initial plus two polls", platform.inspectCalls)
	}
}

func TestServiceInstallRejectsOwnershipConflictBeforeMutation(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	record := installstate.Record{
		SchemaVersion: installstate.CurrentSchemaVersion,
		Owner:         installstate.OwnerGo,
		Version:       lifecycle.Version,
		Executable:    lifecycle.Executable,
		Services:      []string{"test.proxy", "test.refresh"},
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	err := lifecycle.Install(context.Background(), installstate.OwnerHomebrew)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Install() error = %v, want ownership conflict", err)
	}
	if len(platform.calls) != 0 {
		t.Fatalf("platform mutated during conflict: %v", platform.calls)
	}
}

func TestServiceRestartRunsRefreshThenProxy(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	platform.proxyRegistered = true
	platform.proxyRunning = true
	platform.refreshRegistered = true

	if err := lifecycle.Restart(context.Background()); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	want := []string{"restart-refresh", "restart-proxy", "inspect"}
	if !reflect.DeepEqual(platform.calls, want) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, want)
	}
	if platform.refreshRuns != 1 {
		t.Fatalf("refresh runs = %d, want 1", platform.refreshRuns)
	}
}

func TestServiceUninstallRemovesRefreshBeforeProxyAndState(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	platform.calls = nil

	if err := lifecycle.Uninstall(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	want := []string{"remove-refresh", "remove-proxy", "inspect"}
	if !reflect.DeepEqual(platform.calls, want) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, want)
	}
	if _, err := store.Load(); !errors.Is(err, installstate.ErrNotInstalled) {
		t.Fatalf("Load() error = %v, want not installed", err)
	}
}

func TestServiceUninstallKeepsStateWhenRemovalFails(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	platform.calls = nil
	platform.removeRefreshErr = errors.New("refresh remove failed")
	platform.removeProxyErr = errors.New("proxy remove failed")

	err := lifecycle.Uninstall(context.Background(), installstate.OwnerGo)
	if !errors.Is(err, platform.removeRefreshErr) || !errors.Is(err, platform.removeProxyErr) {
		t.Fatalf("Uninstall() error = %v, want both removal errors", err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("state removed after failed uninstall: %v", err)
	}
	want := []string{"remove-refresh", "remove-proxy"}
	if !reflect.DeepEqual(platform.calls, want) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, want)
	}
}

func TestServiceUninstallRejectsDifferentOwner(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	platform.calls = nil

	err := lifecycle.Uninstall(context.Background(), installstate.OwnerWinGet)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Uninstall() error = %v, want ownership conflict", err)
	}
	if len(platform.calls) != 0 {
		t.Fatalf("platform calls during owner conflict = %v", platform.calls)
	}
}

func TestServiceStatusJSONSchemaIsStable(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	platform.proxyRegistered = true
	platform.proxyRunning = true
	platform.refreshRegistered = true
	status, err := lifecycle.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"executable":"` + lifecycle.Executable + `","proxy":{"id":"test.proxy","manager":"test","registered":true,"running":true,"configured_executable":"` + lifecycle.Executable + `","live_executable":"` + lifecycle.Executable + `","pid":41,"listener":"127.0.0.1:19280","healthy":true},"refresh":{"id":"test.refresh","manager":"test","registered":true,"running":false,"configured_executable":"` + lifecycle.Executable + `","healthy":true,"last_result":"success"}}`
	if string(data) != want {
		t.Fatalf("status JSON = %s\nwant        = %s", data, want)
	}
}

func newServiceHarness(t *testing.T) (*serviceLifecycle, *fakeServicePlatform, installstate.Store) {
	t.Helper()
	stateRoot := filepath.Join(t.TempDir(), "state")
	store := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: stateRoot}}
	executable := filepath.Join(t.TempDir(), "bin", serviceExecutableName())
	platform := &fakeServicePlatform{executable: executable, proxyHealthy: true, refreshHealthy: true}
	lifecycle := &serviceLifecycle{
		Platform:       platform,
		Store:          store,
		Executable:     executable,
		Version:        "0.27.0",
		StatusAttempts: 1,
		StatusInterval: time.Millisecond,
		Wait:           func(context.Context, time.Duration) error { return nil },
	}
	return lifecycle, platform, store
}

func serviceExecutableName() string {
	if filepath.Separator == '\\' {
		return "cq.exe"
	}
	return "cq"
}

type fakeServicePlatform struct {
	executable string
	calls      []string

	proxyRegistered   bool
	proxyRunning      bool
	proxyHealthy      bool
	refreshRegistered bool
	refreshHealthy    bool
	refreshRuns       int
	inspectCalls      int

	installProxyErr   error
	installRefreshErr error
	restartProxyErr   error
	restartRefreshErr error
	removeProxyErr    error
	removeRefreshErr  error
	inspectErr        error
}

func (platform *fakeServicePlatform) Preflight(_ context.Context, executable string) error {
	platform.calls = append(platform.calls, "preflight")
	if executable != platform.executable {
		return errors.New("unexpected executable")
	}
	return nil
}

func (platform *fakeServicePlatform) InstallProxy(context.Context, string) error {
	platform.calls = append(platform.calls, "install-proxy")
	if platform.installProxyErr != nil {
		return platform.installProxyErr
	}
	platform.proxyRegistered = true
	platform.proxyRunning = true
	return nil
}

func (platform *fakeServicePlatform) InstallRefresh(context.Context, string) error {
	platform.calls = append(platform.calls, "install-refresh")
	if platform.installRefreshErr != nil {
		return platform.installRefreshErr
	}
	platform.refreshRegistered = true
	platform.refreshRuns++
	return nil
}

func (platform *fakeServicePlatform) RestartProxy(context.Context) error {
	platform.calls = append(platform.calls, "restart-proxy")
	if platform.restartProxyErr != nil {
		return platform.restartProxyErr
	}
	platform.proxyRunning = platform.proxyRegistered
	return nil
}

func (platform *fakeServicePlatform) RestartRefresh(context.Context) error {
	platform.calls = append(platform.calls, "restart-refresh")
	if platform.restartRefreshErr != nil {
		return platform.restartRefreshErr
	}
	if platform.refreshRegistered {
		platform.refreshRuns++
	}
	return nil
}

func (platform *fakeServicePlatform) RemoveProxy(context.Context) error {
	platform.calls = append(platform.calls, "remove-proxy")
	if platform.removeProxyErr != nil {
		return platform.removeProxyErr
	}
	platform.proxyRegistered = false
	platform.proxyRunning = false
	return nil
}

func (platform *fakeServicePlatform) RemoveRefresh(context.Context) error {
	platform.calls = append(platform.calls, "remove-refresh")
	if platform.removeRefreshErr != nil {
		return platform.removeRefreshErr
	}
	platform.refreshRegistered = false
	return nil
}

func (platform *fakeServicePlatform) Inspect(context.Context) (serviceStatus, error) {
	platform.calls = append(platform.calls, "inspect")
	platform.inspectCalls++
	if platform.inspectErr != nil {
		return serviceStatus{}, platform.inspectErr
	}
	status := serviceStatus{
		SchemaVersion: serviceStatusSchemaVersion,
		Executable:    platform.executable,
		Proxy: componentStatus{
			ID:                   "test.proxy",
			Manager:              "test",
			Registered:           platform.proxyRegistered,
			Running:              platform.proxyRunning,
			ConfiguredExecutable: registeredValue(platform.proxyRegistered, platform.executable),
			LiveExecutable:       registeredValue(platform.proxyRunning, platform.executable),
			PID:                  registeredInt(platform.proxyRunning, 41),
			Listener:             registeredValue(platform.proxyRunning, "127.0.0.1:19280"),
			Healthy:              platform.proxyRegistered && platform.proxyRunning && platform.proxyHealthy,
		},
		Refresh: componentStatus{
			ID:                   "test.refresh",
			Manager:              "test",
			Registered:           platform.refreshRegistered,
			ConfiguredExecutable: registeredValue(platform.refreshRegistered, platform.executable),
			Healthy:              platform.refreshRegistered && platform.refreshHealthy,
			LastResult:           registeredValue(platform.refreshRegistered, "success"),
		},
	}
	return status, nil
}

func registeredValue(registered bool, value string) string {
	if registered {
		return value
	}
	return ""
}

func registeredInt(registered bool, value int) int {
	if registered {
		return value
	}
	return 0
}

var _ servicePlatform = (*fakeServicePlatform)(nil)

func TestServiceErrorMessagesDoNotExposeEnvironment(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	secret := "cq-test-secret-value"
	t.Setenv("CQ_SECRET_TEST_VALUE", secret)
	platform.installProxyErr = errors.New("registration failed")

	err := lifecycle.Install(context.Background(), installstate.OwnerGo)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("Install() error = %v", err)
	}
}
