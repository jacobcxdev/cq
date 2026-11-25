package dots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"
)

const desktopStartTimeout = nativeStartupTimeout + 30*time.Second
const desktopRecoveryDelay = desktopStartTimeout + 5*time.Second

type desktopWatchdog interface{ Kill() error }

// Narrow external boundaries keep desktop lifecycle tests independent of the
// user's actual app, workers, LaunchServices and installation.
type desktopDependencies struct {
	stage   func(*Settings, string) (string, error)
	spawn   func(*Settings, string, string, []string, []string) (desktopWatchdog, error)
	start   func(context.Context, Controller) (map[string]any, error)
	repair  func(context.Context, Controller) error
	open    func(context.Context, string) error
	recover func(context.Context, *Settings) error
	wait    func(context.Context, time.Duration) error
	appPath func(*Settings) (string, error)
}

func desktopDefaults() desktopDependencies {
	return desktopDependencies{
		stage: func(s *Settings, source string) (string, error) {
			if runtime.GOOS == "windows" {
				// Windows cannot replace the helper once the watchdog runs it.
				return StageExecutable(source, s.RuntimeRoot)
			}
			return ensureDesktopExecutable(s, source)
		},
		spawn: func(s *Settings, executable, role string, args, env []string) (desktopWatchdog, error) {
			return SpawnHelper(s, executable, role, args, env)
		},
		start: func(ctx context.Context, c Controller) (map[string]any, error) {
			return c.Execute(ctx, Request{Operation: "start"})
		},
		repair: func(ctx context.Context, c Controller) error {
			result, err := c.Execute(ctx, Request{Operation: "repair"})
			if err != nil {
				return err
			}
			if result["native_catalog_ready"] != true || result["workers_ready"] != true {
				return errors.New("Native Dots startup is incomplete")
			}
			return nil
		},
		open: func(ctx context.Context, app string) error {
			query, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := openDesktopApp(query, app); err != nil {
				return errors.New("Native app activation failed")
			}
			return nil
		},
		recover: RecoverApp,
		wait:    waitContext,
		appPath: currentDesktopAppPath,
	}
}

func ensureDesktopExecutable(s *Settings, source string) (string, error) {
	path := helperExecutable(s)
	if info, err := os.Lstat(path); err == nil {
		if runtime.GOOS == "windows" {
			if err := windowsValidatePrivateFile(path); err != nil {
				return "", errors.New("Staged Dots executable must be an owned private regular file")
			}
		} else if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o700 {
			return "", errors.New("Staged Dots executable must be owned and mode 0700")
		}
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return StageExecutable(source, s.RuntimeRoot)
}

// RunDesktopLaunch starts Dots before activating the installed native app.
// Independent recovery survives a failed or cancelled CLI startup.
func RunDesktopLaunch(ctx context.Context, s *Settings, c Controller) error {
	return runDesktopLaunch(ctx, s, c, desktopDefaults())
}

func runDesktopLaunch(ctx context.Context, s *Settings, c Controller, deps desktopDependencies) error {
	if s == nil || s.IntegrationMode != "native" {
		return errors.New("Spotlight launcher requires a native Dots installation")
	}
	failure := func(action string) error {
		return fmt.Errorf("CQ could not %s. Run cq codex dots status to check the adapter", action)
	}
	openCurrent := func() error {
		app := s.AppPath
		if deps.appPath != nil {
			var err error
			app, err = deps.appPath(s)
			if err != nil {
				return err
			}
		}
		return deps.open(ctx, app)
	}
	activateRepaired := func() error {
		if ctx.Err() != nil {
			return nil
		}
		if err := openCurrent(); err != nil {
			return errors.New("Dots recovered, but native app activation failed")
		}
		return nil
	}
	repairNow := func() bool {
		// Repair survives launcher cancellation, but cannot restart the app.
		recovery, cancel := context.WithTimeout(context.Background(), nativeStartupTimeout)
		defer cancel()
		return deps.repair(recovery, c) == nil
	}
	recoverNow := func() bool {
		// Startup cancellation must not cancel recovery of the user's other chats.
		recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return deps.recover(recovery, s) == nil
	}
	executable, err := deps.stage(s, c.Executable)
	if err != nil {
		if repairNow() {
			return activateRepaired()
		}
		recoverNow()
		return failure("prepare the Dots helper executable")
	}
	env := os.Environ()
	if c.BaseURL != "" {
		env = setEnvironment(env, "OPENAI_BASE_URL", c.BaseURL)
	}
	env = setEnvironment(env, "CQ_DOTS_CQ_PATH", c.Executable)
	watchdog, err := deps.spawn(s, executable, "desktop-recover", nil, env)
	if err != nil {
		if repairNow() {
			return activateRepaired()
		}
		recoverNow()
		return failure("start the Dots recovery helper")
	}
	stopWatchdog := false
	defer func() {
		if stopWatchdog && watchdog != nil {
			_ = watchdog.Kill()
		}
	}()
	startup, cancel := context.WithTimeout(ctx, desktopStartTimeout)
	result, err := deps.start(startup, c)
	cancel()
	if err != nil {
		if repairNow() {
			stopWatchdog = true
			return activateRepaired()
		}
		stopWatchdog = recoverNow()
		return failure("start the Dots adapter")
	}
	if result["restart_required"] == true {
		stopWatchdog = recoverNow()
		return errors.New("Quit ChatGPT, then reopen ChatGPT via CQ. Active work has not been interrupted.")
	}
	if result["started"] == false {
		stopWatchdog = recoverNow()
		return errors.New("No active Dots are available. Run cq codex dots status for details.")
	}
	if err := openCurrent(); err != nil {
		stopWatchdog = recoverNow()
		return failure("activate the ChatGPT window")
	}
	stopWatchdog = true
	return nil
}

// RunDesktopRecovery is a detached helper role, not a goroutine tied to the
// launcher process. It never stops or relaunches an already running app.
func RunDesktopRecovery(ctx context.Context, s *Settings, c Controller) error {
	return runDesktopRecovery(ctx, s, c, desktopDefaults())
}

func runDesktopRecovery(ctx context.Context, s *Settings, c Controller, deps desktopDependencies) error {
	if s == nil || s.IntegrationMode != "native" {
		return errors.New("Spotlight recovery requires a native Dots installation")
	}
	if err := deps.wait(ctx, desktopRecoveryDelay); err != nil {
		return err
	}
	repair, cancel := context.WithTimeout(ctx, nativeStartupTimeout)
	err := deps.repair(repair, c)
	cancel()
	if err == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := deps.recover(ctx, s); err != nil {
		return errors.New("Native app recovery failed")
	}
	return nil
}
