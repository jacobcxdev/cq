//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"

	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestLinuxServiceUsesXDGSystemdUserDirectory(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	t.Setenv("XDG_CONFIG_HOME", config)

	directory, err := linuxSystemdUserDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config, "systemd", "user"); directory != want {
		t.Fatalf("unit directory = %q, want %q", directory, want)
	}
}

func TestLinuxServiceLifecycleBindsSystemdContract(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "cq")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	unitDirectory := filepath.Join(root, "systemd", "user")
	lifecycle := newLinuxServiceLifecycle(
		executable,
		unitDirectory,
		userdirs.Roots{State: filepath.Join(root, "state")},
		func(context.Context, ...string) ([]byte, error) { return nil, nil },
		func(context.Context, string) componentStatus { return componentStatus{} },
	)
	platform, ok := lifecycle.Platform.(*systemdServicePlatform)
	if !ok {
		t.Fatalf("platform = %T", lifecycle.Platform)
	}
	if platform.unitPath(systemdProxyUnit) != filepath.Join(unitDirectory, "cq-proxy.service") || platform.unitPath(systemdRefreshService) != filepath.Join(unitDirectory, "cq-refresh.service") || platform.unitPath(systemdRefreshTimer) != filepath.Join(unitDirectory, "cq-refresh.timer") {
		t.Fatalf("unit paths do not match published contract")
	}
	if lifecycle.Executable != executable || lifecycle.Version != version {
		t.Fatalf("lifecycle = %#v", lifecycle)
	}
	store, ok := lifecycle.Store.(*installstate.Store)
	if !ok || store.Roots.State == "" {
		t.Fatalf("store = %#v", lifecycle.Store)
	}
}

func TestLinuxProxyRuntimeHookBindsExactKernelIdentity(t *testing.T) {
	previous := inspectLinuxProxyRuntimeFn
	previousWorker := captureLinuxRuntimeWorkerFn
	previousHealth := probeLinuxProxyRuntimeHealthFn
	previousPort := linuxProxyRuntimePortFn
	t.Cleanup(func() {
		inspectLinuxProxyRuntimeFn = previous
		captureLinuxRuntimeWorkerFn = previousWorker
		probeLinuxProxyRuntimeHealthFn = previousHealth
		linuxProxyRuntimePortFn = previousPort
	})
	linuxProxyRuntimePortFn = func() (int, error) { return 24567, nil }
	inspectLinuxProxyRuntimeFn = func(_ context.Context, executable string, port int) (proxy.LinuxProxyRuntimeIdentity, error) {
		if executable != "/home/test/bin/cq" || port != 24567 {
			t.Fatalf("inspection inputs = %q %d", executable, port)
		}
		process := proxy.LinuxProcessIdentity{
			PID: 731, ParentPID: 1, StartTime: 100, UID: 501,
			Arguments:  []string{executable, "proxy", "start"},
			CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
			Executable: proxy.LinuxExecutableIdentity{
				Path: executable, Device: 1, Inode: 2, Links: 1, Owner: 501,
				Size: 4, Mode: 0o100755, SHA256: [32]byte{1},
			},
		}
		return proxy.LinuxProxyRuntimeIdentity{
			Process:  process,
			Listener: proxy.LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 7, Process: process},
		}, nil
	}
	captureLinuxRuntimeWorkerFn = func(_ context.Context, supervisor proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
		if supervisor.PID != 731 {
			t.Fatalf("supervisor PID = %d, want 731", supervisor.PID)
		}
		return proxy.LinuxProcessIdentity{
			PID: 732, ParentPID: supervisor.PID, StartTime: 101, UID: supervisor.UID,
			Arguments:  []string{"/home/test/bin/cq", "proxy", "start", "--runtime-role", "worker"},
			CgroupPath: supervisor.CgroupPath,
			Executable: supervisor.Executable,
		}, nil
	}
	probeLinuxProxyRuntimeHealthFn = func(_ context.Context, address string) bool {
		if address != "127.0.0.1:19280" {
			t.Fatalf("health address = %q", address)
		}
		return true
	}

	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if !status.Healthy || status.PID != 731 || status.LiveExecutable != "/home/test/bin/cq" || status.Listener != "127.0.0.1:19280" || status.Error != "" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestLinuxProxyRuntimeInspectorFailsClosedUntilWorkerHealth(t *testing.T) {
	previousInspect := inspectLinuxProxyRuntimeFn
	previousWorker := captureLinuxRuntimeWorkerFn
	previousHealth := probeLinuxProxyRuntimeHealthFn
	previousPort := linuxProxyRuntimePortFn
	t.Cleanup(func() {
		inspectLinuxProxyRuntimeFn = previousInspect
		captureLinuxRuntimeWorkerFn = previousWorker
		probeLinuxProxyRuntimeHealthFn = previousHealth
		linuxProxyRuntimePortFn = previousPort
	})
	linuxProxyRuntimePortFn = func() (int, error) { return 24567, nil }
	process := proxy.LinuxProcessIdentity{
		PID: 731, ParentPID: 1, StartTime: 100, UID: 501,
		Arguments:  []string{"/home/test/bin/cq", "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
		Executable: proxy.LinuxExecutableIdentity{Path: "/home/test/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 501, Size: 4, Mode: 0o100755, SHA256: [32]byte{1}},
	}
	inspectLinuxProxyRuntimeFn = func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error) {
		return proxy.LinuxProxyRuntimeIdentity{Process: process, Listener: proxy.LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 7, Process: process}}, nil
	}
	captureLinuxRuntimeWorkerFn = func(context.Context, proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
		return proxy.LinuxProcessIdentity{PID: 732, ParentPID: process.PID, StartTime: 101, UID: process.UID, Arguments: []string{"worker"}, CgroupPath: process.CgroupPath, Executable: process.Executable}, nil
	}
	probeLinuxProxyRuntimeHealthFn = func(context.Context, string) bool { return false }

	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if status.Healthy || status.Running || status.Error == "" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestLinuxProxyRuntimeInspectorFailsClosedAcrossRuntimeGenerations(t *testing.T) {
	previousInspect := inspectLinuxProxyRuntimeFn
	previousWorker := captureLinuxRuntimeWorkerFn
	previousHealth := probeLinuxProxyRuntimeHealthFn
	previousPort := linuxProxyRuntimePortFn
	t.Cleanup(func() {
		inspectLinuxProxyRuntimeFn = previousInspect
		captureLinuxRuntimeWorkerFn = previousWorker
		probeLinuxProxyRuntimeHealthFn = previousHealth
		linuxProxyRuntimePortFn = previousPort
	})
	linuxProxyRuntimePortFn = func() (int, error) { return 24567, nil }
	process := proxy.LinuxProcessIdentity{
		PID: 731, ParentPID: 1, StartTime: 100, UID: 501,
		Arguments:  []string{"/home/test/bin/cq", "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
		Executable: proxy.LinuxExecutableIdentity{Path: "/home/test/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 501, Size: 4, Mode: 0o100755, SHA256: [32]byte{1}},
	}
	inspectCalls := 0
	inspectLinuxProxyRuntimeFn = func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error) {
		inspectCalls++
		generation := process
		inode := uint64(7)
		if inspectCalls > 1 {
			generation.StartTime++
			inode++
		}
		return proxy.LinuxProxyRuntimeIdentity{Process: generation, Listener: proxy.LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: inode, Process: generation}}, nil
	}
	captureLinuxRuntimeWorkerFn = func(context.Context, proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
		return proxy.LinuxProcessIdentity{PID: 732, ParentPID: process.PID, StartTime: 101, UID: process.UID, Arguments: []string{"worker"}, CgroupPath: process.CgroupPath, Executable: process.Executable}, nil
	}
	probeLinuxProxyRuntimeHealthFn = func(context.Context, string) bool { return true }

	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if status.Healthy || status.Running || status.Error == "" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestLinuxProxyRuntimeInspectorFailsClosedAcrossWorkerGenerations(t *testing.T) {
	previousInspect := inspectLinuxProxyRuntimeFn
	previousWorker := captureLinuxRuntimeWorkerFn
	previousHealth := probeLinuxProxyRuntimeHealthFn
	previousPort := linuxProxyRuntimePortFn
	t.Cleanup(func() {
		inspectLinuxProxyRuntimeFn = previousInspect
		captureLinuxRuntimeWorkerFn = previousWorker
		probeLinuxProxyRuntimeHealthFn = previousHealth
		linuxProxyRuntimePortFn = previousPort
	})
	linuxProxyRuntimePortFn = func() (int, error) { return 24567, nil }
	process := proxy.LinuxProcessIdentity{
		PID: 731, ParentPID: 1, StartTime: 100, UID: 501,
		Arguments:  []string{"/home/test/bin/cq", "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
		Executable: proxy.LinuxExecutableIdentity{Path: "/home/test/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 501, Size: 4, Mode: 0o100755, SHA256: [32]byte{1}},
	}
	inspectLinuxProxyRuntimeFn = func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error) {
		return proxy.LinuxProxyRuntimeIdentity{Process: process, Listener: proxy.LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 7, Process: process}}, nil
	}
	workerCalls := 0
	captureLinuxRuntimeWorkerFn = func(context.Context, proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
		workerCalls++
		return proxy.LinuxProcessIdentity{
			PID: 732, ParentPID: process.PID, StartTime: uint64(100 + workerCalls), UID: process.UID,
			Arguments: []string{"worker"}, CgroupPath: process.CgroupPath, Executable: process.Executable,
		}, nil
	}
	probeLinuxProxyRuntimeHealthFn = func(context.Context, string) bool { return true }

	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if status.Healthy || status.Running || status.Error == "" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestLinuxProxyRuntimeInspectorFailsClosedWithoutWorker(t *testing.T) {
	previousInspect := inspectLinuxProxyRuntimeFn
	previousWorker := captureLinuxRuntimeWorkerFn
	previousPort := linuxProxyRuntimePortFn
	t.Cleanup(func() {
		inspectLinuxProxyRuntimeFn = previousInspect
		captureLinuxRuntimeWorkerFn = previousWorker
		linuxProxyRuntimePortFn = previousPort
	})
	linuxProxyRuntimePortFn = func() (int, error) { return 24567, nil }
	process := proxy.LinuxProcessIdentity{
		PID: 731, ParentPID: 1, StartTime: 100, UID: 501,
		Arguments:  []string{"/home/test/bin/cq", "proxy", "start"},
		CgroupPath: "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service",
		Executable: proxy.LinuxExecutableIdentity{Path: "/home/test/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 501, Size: 4, Mode: 0o100755, SHA256: [32]byte{1}},
	}
	inspectLinuxProxyRuntimeFn = func(context.Context, string, int) (proxy.LinuxProxyRuntimeIdentity, error) {
		return proxy.LinuxProxyRuntimeIdentity{Process: process, Listener: proxy.LinuxListenerIdentity{Address: "127.0.0.1:19280", Inode: 7, Process: process}}, nil
	}
	captureLinuxRuntimeWorkerFn = func(context.Context, proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
		return proxy.LinuxProcessIdentity{}, errors.New("missing worker")
	}

	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if status.Healthy || status.Running || status.Error == "" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestLinuxProxyRuntimeInspectorFailsClosedWithoutConfiguredPort(t *testing.T) {
	previous := linuxProxyRuntimePortFn
	t.Cleanup(func() { linuxProxyRuntimePortFn = previous })
	linuxProxyRuntimePortFn = func() (int, error) { return 0, errors.New("missing") }
	status := linuxProxyRuntimeInspector(context.Background(), "/home/test/bin/cq")
	if status.Healthy || status.Running || status.Error == "" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestLinuxServiceUserDirsRejectInvalidAndCleanAbsoluteBases(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "unused-invalid")
	for _, value := range []string{"relative", " ", "~/config"} {
		t.Setenv("XDG_CONFIG_HOME", value)
		if _, err := linuxSystemdUserDirectory(); err == nil {
			t.Fatal("invalid XDG accepted")
		}
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/nested/..")
	got, err := linuxSystemdUserDirectory()
	if err != nil || got != filepath.Join(dir, "systemd", "user") {
		t.Fatalf("directory %q error %v", got, err)
	}
}

func TestLinuxServiceSelectedFactoryAndPreparationBudget(t *testing.T) {
	originalRun := linuxSelectedSystemctl
	linuxSelectedSystemctl = (&fakeSystemctl{show: map[string][]byte{}}).Run
	t.Cleanup(func() { linuxSelectedSystemctl = originalRun })
	nativeHome := t.TempDir()
	shellHome := t.TempDir()
	t.Setenv("HOME", shellHome)
	old := linuxSelectedHome
	t.Cleanup(func() { linuxSelectedHome = old })
	linuxSelectedHome = func() (string, error) { return nativeHome, nil }
	lifecycle, err := selectedServiceLifecycleFactory(context.Background(), serviceInspect, serviceProxy)
	if err != nil {
		t.Fatal(err)
	}
	p := lifecycle.Platform.(*systemdServicePlatform)
	if p.home != nativeHome || p.inspectSelectedProxy == nil || p.roots.Config == "" {
		t.Fatal("canonical Linux factory did not bind native authority")
	}
	linuxSelectedHome = func() (string, error) {
		time.Sleep(5 * time.Millisecond)
		return "", errors.New("late preparation failure")
	}
	inv := cli.Invocation{Path: "service status", Options: map[string][]string{"timeout": {"1ms"}, "component": {"proxy"}}}
	outcome := handleV2Service(context.Background(), inv, &cli.Session{})
	if outcome.ExitCode != 7 {
		t.Fatalf("preparation timeout exit=%d", outcome.ExitCode)
	}
}
func TestLinuxServiceSelectedRuntimeRejectsUnknownInstalledConfig(t *testing.T) {
	old := linuxProxyRuntimePortFn
	linuxProxyRuntimePortFn = func() (int, error) { t.Fatal("read invoking-shell config"); return 0, nil }
	t.Cleanup(func() { linuxProxyRuntimePortFn = old })
	roots := userdirs.Roots{Config: filepath.Join(t.TempDir(), "missing")}
	status := inspectSelectedLinuxProxy(context.Background(), "/fixture/cq", roots)
	if status.Healthy || status.Error == "" {
		t.Fatal("unknown installed config became healthy")
	}
}

func TestLinuxServiceRefreshHookBoundToInstalledUnit(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "unmarked", "wrong executable", "wrong roots", "wrong home", "missing unit"} {
		t.Run(scenario, func(t *testing.T) {
			_, p, _ := newSelectedSystemdHarness(t)
			actual, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			executable := actual
			if scenario == "wrong executable" {
				executable = p.executable
			}
			defs, err := renderSelectedSystemdDefinitions(executable, p.home, p.roots)
			if err != nil {
				t.Fatal(err)
			}
			if err := atomicWriteSystemdUnit(p.unitPath(systemdRefreshService), defs[systemdRefreshService]); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", p.home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Dir(p.roots.Config))
			t.Setenv("XDG_CACHE_HOME", filepath.Dir(p.roots.Cache))
			t.Setenv("CQ_SERVICE_REFRESH", "1")
			switch scenario {
			case "unmarked":
				t.Setenv("CQ_SERVICE_REFRESH", "")
			case "wrong roots":
				t.Setenv("XDG_CACHE_HOME", t.TempDir())
			case "wrong home":
				t.Setenv("HOME", t.TempDir())
			case "missing unit":
				if err := os.Remove(p.unitPath(systemdRefreshService)); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			sentinel := errors.New("refresh failed")
			err = runLinuxServiceRefresh(func() error {
				calls++
				if scenario == "failure" {
					return sentinel
				}
				return nil
			})
			if scenario == "success" || scenario == "failure" || scenario == "unmarked" {
				if calls != 1 {
					t.Fatalf("refresh calls=%d", calls)
				}
				if scenario == "failure" && !errors.Is(err, sentinel) {
					t.Fatalf("failure lost: %v", err)
				}
				if scenario != "failure" && err != nil {
					t.Fatal(err)
				}
				if scenario == "unmarked" {
					if _, err := os.Stat(filepath.Join(p.roots.State, serviceRefreshCompletionName)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("ordinary refresh wrote receipt")
					}
				} else {
					receipt, err := readServiceRefreshCompletion(executable, p.roots, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					want := 0
					if scenario == "failure" {
						want = 1
					}
					if receipt.ExitCode != want {
						t.Fatalf("completion exit=%d", receipt.ExitCode)
					}
				}
			} else if calls != 0 || err == nil {
				t.Fatalf("unowned service refreshed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestLinuxServiceSelectedFactoryInstalledAuthority(t *testing.T) {
	for _, scenario := range []string{"canonical", "legacy status", "legacy reinstall", "legacy missing record", "legacy mismatched record", "legacy unsafe record", "missing manager", "discovery deadline"} {
		t.Run(scenario, func(t *testing.T) {
			original, installed, runner := newSelectedSystemdHarness(t)
			oldRun, oldHome := linuxSelectedSystemctl, linuxSelectedHome
			t.Cleanup(func() { linuxSelectedSystemctl = oldRun; linuxSelectedHome = oldHome })
			linuxSelectedSystemctl = installed.run
			linuxSelectedHome = func() (string, error) { return installed.home, nil }
			t.Setenv("HOME", "invalid-shell-home")
			t.Setenv("XDG_CONFIG_HOME", "invalid-shell-config")
			cache := filepath.Join(t.TempDir(), "new-cache")
			t.Setenv("XDG_CACHE_HOME", cache)
			if strings.HasPrefix(scenario, "legacy") {
				store := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: installed.roots}
				if err := store.Save(original.Store.(*selectedServiceStore).record); err != nil {
					t.Fatal(err)
				}
				defs, err := renderSystemdServiceDefinitions(installed.executable)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range systemdSelectedUnits(serviceRefresh) {
					if err := atomicWriteSystemdUnit(installed.unitPath(name), defs[name]); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "legacy missing record" {
				if err := os.Remove(filepath.Join(installed.roots.State, "install.json")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "legacy mismatched record" {
				record := original.Store.(*selectedServiceStore).record
				record.Services = []string{systemdProxyUnit}
				if err := (installstate.Store{FS: fsutil.OSFileSystem{}, Roots: installed.roots}).Save(record); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "legacy unsafe record" {
				if err := os.Chmod(filepath.Join(installed.roots.State, "install.json"), 0o666); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing manager" {
				linuxSelectedSystemctl = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("missing manager") }
			}
			if scenario == "discovery deadline" {
				linuxSelectedSystemctl = func(ctx context.Context, args ...string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }
				inv := cli.Invocation{Path: "service status", Options: map[string][]string{"timeout": {"1ms"}, "component": {"token-refresh"}}}
				if out := handleV2Service(context.Background(), inv, &cli.Session{}); out.ExitCode != 7 {
					t.Fatalf("discovery exceeded command budget: %d", out.ExitCode)
				}
				return
			}
			action := serviceInspect
			if scenario == "legacy reinstall" || strings.Contains(scenario, "record") {
				action = serviceInstall
			}
			lifecycle, err := selectedServiceLifecycleFactory(context.Background(), action, serviceRefresh)
			if strings.Contains(scenario, "record") {
				if err == nil || lifecycle != nil {
					t.Fatal("unsafe legacy ownership accepted")
				}
				return
			}
			if scenario == "missing manager" {
				if !errors.Is(err, errServiceUnavailable) {
					t.Fatalf("manager absence=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			p := lifecycle.Platform.(*systemdServicePlatform)
			store := lifecycle.Store.(*installstate.Store)
			locker := lifecycle.MutationLocker.(installer.FileInstallLocker)
			if p.unitDirectory != installed.unitDirectory || store.Roots.State != installed.roots.State || locker.StateRoot != installed.roots.State {
				t.Fatal("installed ownership/lock moved into shell roots")
			}
			switch scenario {
			case "canonical":
				if p.roots != installed.roots {
					t.Fatal("canonical roots replaced")
				}
			case "legacy status":
				if p.roots != (userdirs.Roots{State: installed.roots.State}) {
					t.Fatal("legacy unobserved roots invented")
				}
			case "legacy reinstall":
				if p.roots.Config != installed.roots.Config || p.roots.Cache != filepath.Join(cache, "cq") {
					t.Fatal("reinstall did not bind new explicit cache while preserving installed config")
				}
				defs, err := renderSelectedSystemdDefinitions(p.executable, p.home, p.roots)
				if err != nil {
					t.Fatal(err)
				}
				_, roots, err := validateOwnedSystemdDefinition(systemdRefreshService, defs[systemdRefreshService])
				if err != nil || roots == nil || *roots != p.roots {
					t.Fatalf("new cache not reconstructible: %v", err)
				}
			}
			for _, args := range runner.calls {
				if args[len(args)-1] == systemdProxyUnit {
					t.Fatal("factory queried unselected proxy")
				}
			}
		})
	}
}

func TestLinuxServiceSelectedAuthenticatedRuntime(t *testing.T) {
	for _, scenario := range []string{"healthy", "wrong token", "missing token", "changed supervisor", "changed worker"} {
		t.Run(scenario, func(t *testing.T) {
			_, p, _ := newSelectedSystemdHarness(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer installed-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.URL.Path == proxy.RuntimeRescueStatusPath {
					w.Write([]byte(`{"mode":"normal"}`))
				} else {
					w.Write([]byte(`{"status":"ok"}`))
				}
			}))
			defer server.Close()
			address := strings.TrimPrefix(server.URL, "http://")
			_, portText, _ := net.SplitHostPort(address)
			port, _ := strconv.Atoi(portText)
			token := "installed-token"
			if scenario == "wrong token" {
				token = "wrong"
			}
			if scenario == "missing token" {
				token = ""
			}
			savedToken := token
			if savedToken == "" {
				savedToken = "installed-token"
			}
			if err := proxy.SaveConfigAt(proxy.PathsForRoots(p.roots), &proxy.Config{Port: port, LocalToken: savedToken}); err != nil {
				t.Fatal(err)
			}
			if token == "" {
				path := proxy.PathsForRoots(p.roots).ConfigFile
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "installed-token", "")), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			oldInspect, oldWorker, oldHealth := inspectLinuxProxyRuntimeFn, captureLinuxRuntimeWorkerFn, probeLinuxProxyRuntimeHealthFn
			t.Cleanup(func() {
				inspectLinuxProxyRuntimeFn = oldInspect
				captureLinuxRuntimeWorkerFn = oldWorker
				probeLinuxProxyRuntimeHealthFn = oldHealth
			})
			probeLinuxProxyRuntimeHealthFn = func(context.Context, string) bool {
				t.Fatal("selected inspection used legacy unauthenticated probe")
				return true
			}
			process := proxy.LinuxProcessIdentity{PID: 731, ParentPID: 1, StartTime: 100, UID: 501, Arguments: []string{p.executable, "proxy", "start"}, CgroupPath: "/cq", Executable: proxy.LinuxExecutableIdentity{Path: p.executable, Device: 1, Inode: 2, Links: 1, Owner: 501, Size: 4, Mode: 0o100755, SHA256: [32]byte{1}}}
			inspections, workers := 0, 0
			inspectLinuxProxyRuntimeFn = func(ctx context.Context, exe string, gotPort int) (proxy.LinuxProxyRuntimeIdentity, error) {
				if exe != p.executable || gotPort != port {
					t.Fatal("runtime used shell configuration")
				}
				inspections++
				current := process
				if scenario == "changed supervisor" && inspections == 2 {
					current.StartTime++
				}
				return proxy.LinuxProxyRuntimeIdentity{Process: current, Listener: proxy.LinuxListenerIdentity{Address: address, Inode: 7, Process: current}}, nil
			}
			captureLinuxRuntimeWorkerFn = func(context.Context, proxy.LinuxProcessIdentity) (proxy.LinuxProcessIdentity, error) {
				workers++
				worker := process
				worker.PID = 732
				worker.ParentPID = 731
				worker.StartTime = 101
				if scenario == "changed worker" && workers == 2 {
					worker.StartTime++
				}
				return worker, nil
			}
			status := inspectSelectedLinuxProxy(context.Background(), p.executable, p.roots)
			if status.Healthy != (scenario == "healthy") {
				t.Fatalf("health=%v scenario=%s error=%s", status.Healthy, scenario, status.Error)
			}
			if scenario == "healthy" && (inspections != 2 || workers != 2) {
				t.Fatal("missing before/after native identity checks")
			}
		})
	}
}
