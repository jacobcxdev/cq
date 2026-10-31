package dots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeDesktopWatchdog struct{ kills int }

func (f *fakeDesktopWatchdog) Kill() error { f.kills++; return nil }

func desktopFixture(t *testing.T) (*Settings, Controller, desktopDependencies, *fakeDesktopWatchdog) {
	t.Helper()
	root := t.TempDir()
	watchdog := &fakeDesktopWatchdog{}
	s := &Settings{RuntimeRoot: filepath.Join(root, "runtime"), AppPath: filepath.Join(root, "Original.app"), IntegrationMode: "native"}
	c := Controller{Root: root, Executable: filepath.Join(root, "cq"), BaseURL: "http://127.0.0.1:19280"}
	deps := desktopDependencies{
		stage: func(*Settings, string) (string, error) { return helperExecutable(s), nil },
		spawn: func(*Settings, string, string, []string, []string) (desktopWatchdog, error) { return watchdog, nil },
		start: func(context.Context, Controller) (map[string]any, error) {
			return map[string]any{"native_catalog_ready": true}, nil
		},
		repair:  func(context.Context, Controller) error { return errors.New("no owned app") },
		open:    func(context.Context, string) error { return nil },
		recover: func(context.Context, *Settings) error { return nil },
		wait:    func(context.Context, time.Duration) error { return nil },
	}
	return s, c, deps, watchdog
}

func TestDesktopLaunchOrdersDetachedRecoveryBeforeBoundedStartAndNativeOpen(t *testing.T) {
	s, c, deps, watchdog := desktopFixture(t)
	var calls []string
	t.Setenv("OPENAI_BASE_URL", "http://wrong.invalid")
	deps.stage = func(settings *Settings, source string) (string, error) {
		calls = append(calls, "stage")
		if settings != s || source != c.Executable {
			t.Fatal("stage arguments changed")
		}
		return helperExecutable(s), nil
	}
	deps.spawn = func(settings *Settings, executable, role string, args, env []string) (desktopWatchdog, error) {
		calls = append(calls, "spawn")
		if settings != s || executable != helperExecutable(s) || role != "desktop-recover" || len(args) != 0 {
			t.Fatal("wrong detached watchdog identity")
		}
		if environmentValue(env, "OPENAI_BASE_URL") != c.BaseURL || environmentValue(env, "CQ_DOTS_CQ_PATH") != c.Executable {
			t.Fatal("CQ launch environment was not explicit")
		}
		return watchdog, nil
	}
	deps.start = func(ctx context.Context, controller Controller) (map[string]any, error) {
		calls = append(calls, "start")
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > desktopStartTimeout || time.Until(deadline) < desktopStartTimeout-time.Second {
			t.Fatal("startup lacks its full readiness budget")
		}
		if desktopStartTimeout <= nativeStartupTimeout || desktopRecoveryDelay <= desktopStartTimeout {
			t.Fatal("launcher or watchdog cuts native startup short")
		}
		return map[string]any{"native_catalog_ready": true}, nil
	}
	deps.open = func(ctx context.Context, app string) error {
		calls = append(calls, "open")
		if ctx.Err() != nil || app != s.AppPath {
			t.Fatal("native open arguments changed")
		}
		return nil
	}
	deps.recover = func(context.Context, *Settings) error {
		t.Fatal("successful launch attempted fallback recovery")
		return nil
	}
	if err := runDesktopLaunch(context.Background(), s, c, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "stage,spawn,start,open" || watchdog.kills != 1 {
		t.Fatalf("lifecycle %v kills=%d", calls, watchdog.kills)
	}
}

func TestDesktopStartupCancellationUsesIndependentRecoveryAndKeepsFailedWatchdog(t *testing.T) {
	for _, recoveryFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "watchdog-survives"}[recoveryFails], func(t *testing.T) {
			s, c, deps, watchdog := desktopFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recovered := 0
			deps.start = func(start context.Context, _ Controller) (map[string]any, error) { cancel(); return nil, start.Err() }
			deps.open = func(context.Context, string) error { t.Fatal("cancelled startup activated app directly"); return nil }
			deps.recover = func(recovery context.Context, settings *Settings) error {
				recovered++
				if recovery.Err() != nil || settings != s {
					t.Fatal("parent cancellation leaked into recovery")
				}
				deadline, ok := recovery.Deadline()
				if !ok || time.Until(deadline) > 15*time.Second {
					t.Fatal("recovery lacks deadline")
				}
				if recoveryFails {
					return errors.New("synthetic recovery failure")
				}
				return nil
			}
			if err := runDesktopLaunch(ctx, s, c, deps); err == nil {
				t.Fatal("cancelled startup reported success")
			}
			wantKills := 1
			if recoveryFails {
				wantKills = 0
			}
			if recovered != 1 || watchdog.kills != wantKills {
				t.Fatalf("recovery=%d watchdog kills=%d", recovered, watchdog.kills)
			}
		})
	}
}

func TestDesktopLaunchFailuresRecoverWithoutExposingBackendDetails(t *testing.T) {
	for _, variant := range []string{"stage", "spawn", "start", "restart", "no-dots", "open"} {
		t.Run(variant, func(t *testing.T) {
			s, c, deps, watchdog := desktopFixture(t)
			recovered := 0
			opens := 0
			secret := errors.New("server body contains private material")
			deps.recover = func(context.Context, *Settings) error { recovered++; return nil }
			deps.open = func(context.Context, string) error {
				opens++
				if variant == "open" {
					return secret
				}
				return nil
			}
			switch variant {
			case "stage":
				deps.stage = func(*Settings, string) (string, error) { return "", secret }
			case "spawn":
				deps.spawn = func(*Settings, string, string, []string, []string) (desktopWatchdog, error) { return nil, secret }
			case "start":
				deps.start = func(context.Context, Controller) (map[string]any, error) { return nil, secret }
			case "restart":
				deps.start = func(context.Context, Controller) (map[string]any, error) {
					return map[string]any{"restart_required": true}, nil
				}
			case "no-dots":
				deps.start = func(context.Context, Controller) (map[string]any, error) {
					return map[string]any{"started": false, "reason": secret.Error()}, nil
				}
			}
			err := runDesktopLaunch(context.Background(), s, c, deps)
			if err == nil || strings.Contains(err.Error(), "private material") {
				t.Fatalf("unsafe startup result %v", err)
			}
			if recovered != 1 {
				t.Fatalf("recovery calls %d", recovered)
			}
			if variant != "open" && opens != 0 {
				t.Fatal("invalid startup opened app before recovery")
			}
			if variant != "stage" && variant != "spawn" && watchdog.kills != 1 {
				t.Fatal("successful recovery left unnecessary watchdog")
			}
		})
	}
}

func TestDesktopRecoveryWaitsForStartupAndHonoursItsOwnStop(t *testing.T) {
	s, c, deps, _ := desktopFixture(t)
	calls := 0
	deps.wait = func(ctx context.Context, delay time.Duration) error {
		if delay != desktopRecoveryDelay {
			t.Fatalf("recovery delay %v", delay)
		}
		calls++
		return nil
	}
	deps.recover = func(context.Context, *Settings) error { calls++; return nil }
	if err := runDesktopRecovery(context.Background(), s, c, deps); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("watchdog did not wait then recover")
	}
	deps.wait = func(context.Context, time.Duration) error { return context.Canceled }
	deps.recover = func(context.Context, *Settings) error { t.Fatal("stopped watchdog reopened app"); return nil }
	if err := runDesktopRecovery(context.Background(), s, c, deps); err != context.Canceled {
		t.Fatalf("watchdog cancellation %v", err)
	}
	s.IntegrationMode = "isolated"
	if err := runDesktopRecovery(context.Background(), s, c, desktopDependencies{}); err == nil {
		t.Fatal("isolated profile recovery accepted")
	}
	if err := runDesktopLaunch(context.Background(), s, Controller{}, desktopDependencies{}); err == nil {
		t.Fatal("isolated desktop launcher accepted")
	}
}

func TestDesktopRecoveryRepairsOwnedWorkersBeforeFallback(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "detached"}[detached], func(t *testing.T) {
			s, c, deps, watchdog := desktopFixture(t)
			repairs := 0
			opens := 0
			deps.start = func(context.Context, Controller) (map[string]any, error) {
				return nil, context.DeadlineExceeded
			}
			deps.repair = func(ctx context.Context, got Controller) error {
				repairs++
				deadline, ok := ctx.Deadline()
				if got.Root != c.Root || ctx.Err() != nil || !ok || time.Until(deadline) > nativeStartupTimeout {
					t.Fatal("repair lost identity or independent bounded context")
				}
				return nil
			}
			deps.recover = func(context.Context, *Settings) error {
				t.Fatal("healthy repaired app triggered fallback warning")
				return nil
			}
			deps.open = func(context.Context, string) error {
				opens++
				if detached {
					t.Fatal("background repair activated app")
				}
				return nil
			}
			var err error
			if detached {
				err = runDesktopRecovery(context.Background(), s, c, deps)
			} else {
				err = runDesktopLaunch(context.Background(), s, c, deps)
			}
			if err != nil || repairs != 1 || !detached && watchdog.kills != 1 {
				t.Fatalf("repair err=%v calls=%d watchdog kills=%d", err, repairs, watchdog.kills)
			}
			if !detached && opens != 1 {
				t.Fatal("successful launcher repair did not activate app")
			}
		})
	}
}

func TestDesktopRecoveryCancellationDuringRepairDoesNotWarn(t *testing.T) {
	s, c, deps, _ := desktopFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps.repair = func(repair context.Context, _ Controller) error {
		cancel()
		return repair.Err()
	}
	deps.recover = func(context.Context, *Settings) error {
		t.Fatal("cancelled watchdog showed fallback warning")
		return nil
	}
	if err := runDesktopRecovery(ctx, s, c, deps); err != context.Canceled {
		t.Fatalf("watchdog cancellation %v", err)
	}
}

func TestDesktopExistingStagedExecutableIsNeverReplacedDuringLaunch(t *testing.T) {
	runtimeRoot := t.TempDir()
	s := &Settings{RuntimeRoot: runtimeRoot}
	path := helperExecutable(s)
	if err := os.WriteFile(path, []byte("already running immutable helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ensureDesktopExecutable(s, "/missing/source")
	if err != nil || got != path {
		t.Fatalf("existing helper %q %v", got, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "already running immutable helper" {
		t.Fatal("live staged executable replaced")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureDesktopExecutable(s, "/missing/source"); err == nil {
		t.Fatal("unsafe existing helper accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/missing/source", path); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureDesktopExecutable(s, "/missing/source"); err == nil {
		t.Fatal("staged helper symlink accepted")
	}
}
