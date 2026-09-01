package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
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
	wantCalls := []string{"preflight", "inspect", "snapshot", "install-proxy", "install-refresh", "inspect"}
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
	wantCalls := []string{"preflight", "inspect", "snapshot", "install-proxy", "install-refresh", "inspect"}
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
	wantCalls := []string{"preflight", "inspect", "snapshot", "install-proxy", "install-refresh", "restore"}
	if !reflect.DeepEqual(platform.calls, wantCalls) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, wantCalls)
	}
}

func TestServiceInstallPreservesPreexistingProxyOnRefreshFailure(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	platform.proxyRegistered = true
	platform.proxyRunning = true
	platform.proxyDefinition = "original"
	platform.installRefreshErr = errors.New("refresh registration failed")

	err := lifecycle.Install(context.Background(), installstate.OwnerGo)
	if !errors.Is(err, platform.installRefreshErr) {
		t.Fatalf("Install() error = %v, want refresh error", err)
	}
	if !platform.proxyRegistered || !platform.proxyRunning {
		t.Fatalf("pre-existing proxy removed: registered/running = %t/%t", platform.proxyRegistered, platform.proxyRunning)
	}
	if platform.proxyDefinition != "original" {
		t.Fatalf("pre-existing proxy definition = %q", platform.proxyDefinition)
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
		BinaryDigest:  strings.Repeat("0", 64),
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

func TestServiceRestartWaitsForProxyBeforeRefresh(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	platform.proxyRegistered = true
	platform.proxyRunning = true
	platform.proxyHealthy = false
	platform.refreshRegistered = true
	lifecycle.StatusAttempts = 3
	lifecycle.Wait = func(context.Context, time.Duration) error {
		platform.proxyHealthy = true
		return nil
	}

	if err := lifecycle.Restart(context.Background()); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	want := []string{"restart-proxy", "inspect", "inspect", "restart-refresh", "inspect"}
	if !reflect.DeepEqual(platform.calls, want) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, want)
	}
	if platform.refreshRuns != 1 {
		t.Fatalf("refresh runs = %d, want 1", platform.refreshRuns)
	}
}

func TestServiceRestartDoesNotRunRefreshWhenProxyIsUnhealthy(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)
	platform.proxyRegistered = true
	platform.proxyRunning = true
	platform.proxyHealthy = false
	platform.refreshRegistered = true

	err := lifecycle.Restart(context.Background())
	if !errors.Is(err, ErrServiceUnhealthy) {
		t.Fatalf("Restart() error = %v, want ErrServiceUnhealthy", err)
	}
	want := []string{"restart-proxy", "inspect"}
	if !reflect.DeepEqual(platform.calls, want) {
		t.Fatalf("platform calls = %v, want %v", platform.calls, want)
	}
	if platform.refreshRuns != 0 {
		t.Fatalf("refresh runs = %d, want 0", platform.refreshRuns)
	}
}

func TestServiceSnapshotRestoresExactDefinitionsAndStoppedState(t *testing.T) {
	lifecycle, platform, store := newServiceHarness(t)
	if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatal(err)
	}
	platform.proxyDefinition = "custom-proxy-definition"
	platform.refreshDefinition = "custom-refresh-definition"
	platform.proxyRunning = false
	snapshotPath := filepath.Join(store.Roots.State, "services-snapshot.json")

	if err := lifecycle.Snapshot(context.Background(), installstate.OwnerGo, snapshotPath); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	platform.proxyDefinition = "candidate-proxy-definition"
	platform.refreshDefinition = "candidate-refresh-definition"
	platform.proxyRunning = true
	if err := lifecycle.Restore(context.Background(), installstate.OwnerGo, snapshotPath); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if platform.proxyDefinition != "custom-proxy-definition" || platform.refreshDefinition != "custom-refresh-definition" || platform.proxyRunning {
		t.Fatalf("restored service state = proxy %q refresh %q running %t", platform.proxyDefinition, platform.refreshDefinition, platform.proxyRunning)
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
	want := []string{"preflight", "inspect", "remove-refresh", "remove-proxy", "inspect"}
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
	want := []string{"preflight", "inspect", "remove-refresh", "remove-proxy"}
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

func TestServiceUninstallWithoutStateOnlySucceedsWhenServicesAreAbsent(t *testing.T) {
	lifecycle, platform, _ := newServiceHarness(t)

	if err := lifecycle.Uninstall(context.Background(), installstate.OwnerGo); err != nil {
		t.Fatalf("absent uninstall error = %v", err)
	}
	if !reflect.DeepEqual(platform.calls, []string{"inspect"}) {
		t.Fatalf("absent uninstall calls = %v", platform.calls)
	}

	platform.calls = nil
	platform.proxyRegistered = true
	platform.proxyRunning = true
	err := lifecycle.Uninstall(context.Background(), installstate.OwnerGo)
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("unowned uninstall error = %v", err)
	}
	if !reflect.DeepEqual(platform.calls, []string{"inspect"}) {
		t.Fatalf("unowned uninstall mutated services: %v", platform.calls)
	}
}

func TestServiceUninstallRequiresRecordedExecutableDigestAndServiceIDs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*serviceLifecycle, installstate.Store)
	}{
		{
			name: "executable",
			mutate: func(lifecycle *serviceLifecycle, _ installstate.Store) {
				lifecycle.Executable = filepath.Join(filepath.Dir(lifecycle.Executable), "other-"+serviceExecutableName())
			},
		},
		{
			name: "digest",
			mutate: func(lifecycle *serviceLifecycle, _ installstate.Store) {
				lifecycle.DigestExecutable = func(string) (string, error) { return strings.Repeat("1", 64), nil }
			},
		},
		{
			name: "service IDs",
			mutate: func(_ *serviceLifecycle, store installstate.Store) {
				record, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				record.Services = []string{"foreign.proxy", "foreign.refresh"}
				if err := store.Save(record); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, platform, store := newServiceHarness(t)
			if err := lifecycle.Install(context.Background(), installstate.OwnerGo); err != nil {
				t.Fatal(err)
			}
			platform.calls = nil
			test.mutate(lifecycle, store)

			err := lifecycle.Uninstall(context.Background(), installstate.OwnerGo)
			if !errors.Is(err, installstate.ErrOwnershipConflict) {
				t.Fatalf("Uninstall() error = %v", err)
			}
			for _, call := range platform.calls {
				if strings.HasPrefix(call, "remove-") {
					t.Fatalf("uninstall mutated services: %v", platform.calls)
				}
			}
		})
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
	stateRoot := testServiceStateRoot(t)
	store := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: stateRoot}}
	executable := filepath.Join(t.TempDir(), "bin", serviceExecutableName())
	platform := &fakeServicePlatform{executable: executable, proxyHealthy: true, refreshHealthy: true}
	lifecycle := &serviceLifecycle{
		Platform:         platform,
		Store:            store,
		Executable:       executable,
		Version:          "0.27.0",
		StatusAttempts:   1,
		StatusInterval:   time.Millisecond,
		Wait:             func(context.Context, time.Duration) error { return nil },
		DigestExecutable: func(string) (string, error) { return strings.Repeat("0", 64), nil },
		MutationLocker:   installer.FileInstallLocker{FS: fsutil.OSFileSystem{}, StateRoot: stateRoot},
	}
	return lifecycle, platform, store
}

func testServiceStateRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return filepath.Join(t.TempDir(), "state")
	}
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	localRoot := filepath.Dir(roots.State)
	stateExisted := pathExistsForServiceTest(t, roots.State)
	localRootExisted := pathExistsForServiceTest(t, localRoot)
	if err := fsutil.EnsureSecureDirectory(fsutil.OSFileSystem{}, roots.State); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !stateExisted {
			if err := os.Remove(roots.State); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove Windows service test state directory: %v", err)
			}
		}
		if !localRootExisted {
			if err := os.Remove(localRoot); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove Windows service test root: %v", err)
			}
		}
	})
	root, err := os.MkdirTemp(roots.State, "service-state-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove Windows service state test root: %v", err)
		}
	})
	return root
}

func pathExistsForServiceTest(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatal(err)
	return false
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
	proxyDefinition   string
	refreshDefinition string

	installProxyErr   error
	installRefreshErr error
	restartProxyErr   error
	restartRefreshErr error
	removeProxyErr    error
	removeRefreshErr  error
	inspectErr        error
}

func (platform *fakeServicePlatform) PrepareRollback(context.Context) (serviceRestore, error) {
	snapshot, err := platform.Snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		return platform.Restore(ctx, snapshot)
	}, nil
}

func (platform *fakeServicePlatform) Snapshot(context.Context) (servicePlatformSnapshot, error) {
	platform.calls = append(platform.calls, "snapshot")
	return servicePlatformSnapshot{Manager: "fake", Components: []serviceComponentSnapshot{
		{ID: "proxy", Definition: []byte(platform.proxyDefinition), Exists: platform.proxyRegistered, Running: platform.proxyRunning},
		{ID: "refresh", Definition: []byte(platform.refreshDefinition), Exists: platform.refreshRegistered},
	}}, nil
}

func (platform *fakeServicePlatform) Restore(_ context.Context, snapshot servicePlatformSnapshot) error {
	platform.calls = append(platform.calls, "restore")
	if snapshot.Manager != "fake" || len(snapshot.Components) != 2 || snapshot.Components[0].ID != "proxy" || snapshot.Components[1].ID != "refresh" {
		return errors.New("invalid fake snapshot")
	}
	platform.proxyRegistered = snapshot.Components[0].Exists
	platform.proxyRunning = snapshot.Components[0].Running
	platform.refreshRegistered = snapshot.Components[1].Exists
	platform.proxyDefinition = string(snapshot.Components[0].Definition)
	platform.refreshDefinition = string(snapshot.Components[1].Definition)
	return nil
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
	platform.proxyDefinition = "candidate"
	return nil
}

func (platform *fakeServicePlatform) InstallRefresh(context.Context, string) error {
	platform.calls = append(platform.calls, "install-refresh")
	if platform.installRefreshErr != nil {
		return platform.installRefreshErr
	}
	platform.refreshRegistered = true
	platform.refreshDefinition = "candidate"
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
