//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestDarwinProxyStartWiresTerminationHandling(t *testing.T) {
	if reflect.ValueOf(runProxyOwnedRuntimeFn).Pointer() != reflect.ValueOf(runDarwinProxyOwnedRuntime).Pointer() {
		t.Fatal("macOS owned runtime bypasses termination handling")
	}
	if reflect.ValueOf(runProxyAdoptedRuntimeFn).Pointer() != reflect.ValueOf(runDarwinProxyAdoptedRuntime).Pointer() {
		t.Fatal("macOS adopted runtime bypasses termination handling")
	}
}

func TestDarwinOwnedRuntimeCancelsOnTermination(t *testing.T) {
	original := runDarwinUnixProxyOwnedRuntime
	started := make(chan struct{})
	runDarwinUnixProxyOwnedRuntime = func(ctx context.Context, _ int, _ func(context.Context, net.Listener, http.Handler) error) (bool, error) {
		close(started)
		<-ctx.Done()
		return true, ctx.Err()
	}
	t.Cleanup(func() { runDarwinUnixProxyOwnedRuntime = original })
	done := make(chan error, 1)
	go func() {
		_, err := runDarwinProxyOwnedRuntime(context.Background(), 0, nil)
		done <- err
	}()
	<-started
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("terminated macOS runtime = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("macOS runtime ignored termination")
	}
}

func TestDarwinAdoptedRuntimeCancelsOnTermination(t *testing.T) {
	original := runDarwinUnixProxyAdoptedRuntime
	started := make(chan struct{})
	runDarwinUnixProxyAdoptedRuntime = func(ctx context.Context, _ net.Listener, _ func(context.Context, net.Listener, http.Handler) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { runDarwinUnixProxyAdoptedRuntime = original })
	done := make(chan error, 1)
	go func() { done <- runDarwinProxyAdoptedRuntime(context.Background(), nil, nil) }()
	<-started
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("terminated adopted macOS runtime = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adopted macOS runtime ignored termination")
	}
}

func TestInstallProxyAgentWritesPlist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	if err := proxy.SaveConfig(&proxy.Config{Port: 24567, LocalToken: "test-token"}); err != nil {
		t.Fatal(err)
	}

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "print" {
			return launchctlTestExitError(113)
		}
		return nil
	}

	stderr := captureStderr(t, func() {
		if err := installProxyAgent(); err != nil {
			t.Fatalf("installProxyAgent: %v", err)
		}
	})

	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), proxyAgentLabel)
	expectedPlistPath := filepath.Join(dir, "Library", "LaunchAgents", proxyAgentLabel+".plist")
	wantCalls := [][]string{
		{"print", target},
		{"bootout", target},
		{"print", target},
		{"bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), expectedPlistPath},
		{"kickstart", "-k", target},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("launchctl calls = %v, want %v", calls, wantCalls)
	}

	plistPath, err := proxyAgentPlistPath()
	if err != nil {
		t.Fatalf("proxyAgentPlistPath: %v", err)
	}
	plistData, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	plist := string(plistData)
	if !strings.Contains(plist, "<string>proxy</string>") || !strings.Contains(plist, "<string>start</string>") {
		t.Fatalf("plist = %q, want proxy start arguments", plist)
	}
	if strings.Contains(plist, "<key>Sockets</key>") {
		t.Fatalf("plist = %q, want runtime supervisor to own listener", plist)
	}
	if !strings.Contains(plist, "<string>"+filepath.Join(dir, "Library", "Logs", "cq", "proxy.log")+"</string>") {
		t.Fatalf("plist = %q, want proxy log path", plist)
	}
	if !strings.Contains(stderr, "installed proxy LaunchAgent") {
		t.Fatalf("stderr = %q, want install notice", stderr)
	}
}

func TestProxyAgentLogPathUsesResolvedLogsRoot(t *testing.T) {
	logs := filepath.Join(string(filepath.Separator), "cq", "logs")
	if got, want := proxyAgentLogPath(logs), filepath.Join(logs, "proxy.log"); got != want {
		t.Fatalf("proxy log path = %q, want %q", got, want)
	}
}

func TestInstallProxyAgentRejectsHomebrewExecutable(t *testing.T) {
	oldExecutable := currentExecutable
	oldRunner := runProxyLaunchctl
	defer func() {
		currentExecutable = oldExecutable
		runProxyLaunchctl = oldRunner
	}()
	currentExecutable = func() (string, error) {
		return "/opt/homebrew/Cellar/cq/0.23.6/bin/cq", nil
	}
	runProxyLaunchctl = func(...string) error {
		t.Fatal("launchctl called for Homebrew-managed install")
		return nil
	}

	err := installProxyAgent()
	if err == nil || !strings.Contains(err.Error(), "brew services start cq") {
		t.Fatalf("installProxyAgent error = %v, want Homebrew start guidance", err)
	}
}

func TestUninstallProxyAgentRejectsHomebrewExecutable(t *testing.T) {
	oldExecutable := currentExecutable
	oldRunner := runProxyLaunchctl
	defer func() {
		currentExecutable = oldExecutable
		runProxyLaunchctl = oldRunner
	}()
	currentExecutable = func() (string, error) {
		return "/usr/local/Cellar/cq/0.23.6/bin/cq", nil
	}
	runProxyLaunchctl = func(...string) error {
		t.Fatal("launchctl called for Homebrew-managed uninstall")
		return nil
	}

	err := uninstallProxyAgent()
	if err == nil || !strings.Contains(err.Error(), "brew services stop cq") {
		t.Fatalf("uninstallProxyAgent error = %v, want Homebrew stop guidance", err)
	}
}

func TestRestartProxyAgentRunsKickstart(t *testing.T) {
	t.Helper()

	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	if err := restartProxyAgent(); err != nil {
		t.Fatalf("restartProxyAgent: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("launchctl calls = %d, want 1", len(calls))
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), proxyAgentLabel)}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Fatalf("launchctl args = %v, want %v", calls[0], want)
	}
}

func TestRestartProxyAgentFallsBackToHomebrewLabel(t *testing.T) {
	oldRunner := runProxyLaunchctl
	defer func() { runProxyLaunchctl = oldRunner }()

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if strings.HasSuffix(args[len(args)-1], "/"+proxyAgentLabel) {
			return launchctlTestExitError(113)
		}
		return nil
	}

	if err := restartProxyAgent(); err != nil {
		t.Fatalf("restartProxyAgent: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("launchctl calls = %d, want 2", len(calls))
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), homebrewProxyAgentLabel)}
	if strings.Join(calls[1], "|") != strings.Join(want, "|") {
		t.Fatalf("fallback launchctl args = %v, want %v", calls[1], want)
	}
}

func TestRestartProxyAgentTargetsHomebrewLabelWhenManagedByHomebrew(t *testing.T) {
	oldExecutable := currentExecutable
	oldRunner := runProxyLaunchctl
	defer func() {
		currentExecutable = oldExecutable
		runProxyLaunchctl = oldRunner
	}()
	currentExecutable = func() (string, error) {
		return "/opt/homebrew/Cellar/cq/0.26.1/bin/cq", nil
	}

	var calls [][]string
	runProxyLaunchctl = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	if err := restartProxyAgent(); err != nil {
		t.Fatalf("restartProxyAgent: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("launchctl calls = %d, want 1", len(calls))
	}
	want := []string{"kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), homebrewProxyAgentLabel)}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Fatalf("launchctl args = %v, want %v", calls[0], want)
	}
}

func TestDarwinProxyInspectionBoundaryHasNoLiveCollectorsInCU1(t *testing.T) {
	target := darwinProxyInspectionTarget()
	if target.Inspector == nil || target.Desired == nil || target.Service == nil || target.Listener == nil || target.Process == nil || target.Runtime == nil || target.DataPlane == nil {
		t.Fatalf("Darwin inspection target omitted collector: %+v", target)
	}
}

func TestDarwinProxyInspectionUsesExistingConfigWithoutResilienceState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := proxy.SaveConfig(&proxy.Config{Port: 24567, LocalToken: "test-token"}); err != nil {
		t.Fatal(err)
	}

	facts := collectDarwinProxyInspectionFacts(context.Background(), "")
	if facts.desired.Status != proxy.FactKnown || facts.desired.Value == nil {
		t.Fatalf("desired fact = %+v, want known", facts.desired)
	}
	if got, want := facts.desired.Value.Listener, "127.0.0.1:24567"; got != want {
		t.Fatalf("desired listener = %q, want %q", got, want)
	}
}

func TestDarwinProxyInspectionUsesConfiguredPortWhenServiceOmitsPort(t *testing.T) {
	if got := darwinProxyInspectionListenerPort(19280, 0); got != 19280 {
		t.Fatalf("listener port = %d, want 19280", got)
	}
	if got := darwinProxyInspectionListenerPort(19280, 29280); got != 29280 {
		t.Fatalf("explicit listener port = %d, want 29280", got)
	}
}

type launchctlTestExitError int

func (e launchctlTestExitError) Error() string { return fmt.Sprintf("exit status %d", e) }
func (e launchctlTestExitError) ExitCode() int { return int(e) }

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return buf.String()
}

func TestDarwinServiceSelectedProxyHealth(t *testing.T) {
	for _, scenario := range []string{"healthy", "wrong-process", "wrong-listener", "unauthorised", "failed-health", "rescue", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			p, _ := newSelectedDarwinHarness(t)
			if err := proxy.SaveConfigAt(proxy.PathsForRoots(p.roots), &proxy.Config{Port: 24567, LocalToken: "selected-test-token"}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			calls := 0
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "/bin/ps" {
					if scenario == "wrong-process" {
						return []byte("/other/cq\n"), nil
					}
					return []byte(p.executable + "\n"), nil
				}
				if scenario == "wrong-listener" {
					return []byte("p999\n"), nil
				}
				return []byte("p4312\n"), nil
			}
			doer := testDoer(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != "127.0.0.1:24567" || request.Header.Get("Authorization") != "Bearer selected-test-token" {
					t.Fatal("health used wrong installed authority")
				}
				code, body := 200, `{"mode":"normal"}`
				if request.URL.Path == "/health" {
					body = `{"status":"ok"}`
					if scenario == "failed-health" {
						body = `{"status":"degraded"}`
					}
				}
				if scenario == "unauthorised" {
					code = 401
				}
				if scenario == "redirect" {
					code = 302
				}
				if scenario == "rescue" {
					body = `{"mode":"rescue"}`
				}
				var reader io.ReadCloser = io.NopCloser(strings.NewReader(body))
				if code != 200 {
					reader = &darwinServiceForbiddenBody{t: t}
				}
				return &http.Response{StatusCode: code, Body: reader}, nil
			})
			status := inspectDarwinSelectedProxyRuntimeWith(context.Background(), p.executable, p.roots, 4312, run, doer)
			if status.Healthy != (scenario == "healthy") {
				t.Fatalf("health=%+v", status)
			}
			if (scenario == "wrong-process" || scenario == "wrong-listener") && calls != 0 {
				t.Fatal("credentials sent before owned listener proof")
			}
			if scenario == "healthy" && calls != 2 {
				t.Fatalf("HTTP calls=%d", calls)
			}
		})
	}
}

type darwinServiceForbiddenBody struct{ t *testing.T }

func (b *darwinServiceForbiddenBody) Read([]byte) (int, error) {
	b.t.Error("read unused authentication error body")
	return 0, io.EOF
}
func (b *darwinServiceForbiddenBody) Close() error { return nil }
