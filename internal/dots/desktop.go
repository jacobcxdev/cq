package dots

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const desktopRecoveryDelay = 40 * time.Second
const desktopStartTimeout = 35 * time.Second

type desktopWatchdog interface{ Kill() error }

// Narrow external boundaries keep desktop lifecycle tests independent of the
// user's actual app, workers, LaunchServices and installation.
type desktopDependencies struct {
	stage   func(*Settings, string) (string, error)
	spawn   func(*Settings, string, string, []string, []string) (desktopWatchdog, error)
	start   func(context.Context, Controller) (map[string]any, error)
	open    func(context.Context, string) error
	recover func(context.Context, *Settings) error
	wait    func(context.Context, time.Duration) error
}

func desktopDefaults() desktopDependencies {
	return desktopDependencies{
		stage: ensureDesktopExecutable,
		spawn: func(s *Settings, executable, role string, args, env []string) (desktopWatchdog, error) {
			return SpawnHelper(s, executable, role, args, env)
		},
		start: func(ctx context.Context, c Controller) (map[string]any, error) {
			return c.Execute(ctx, Request{Operation: "start"})
		},
		open: func(ctx context.Context, app string) error {
			query, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := exec.CommandContext(query, "/usr/bin/open", "-a", app).Run(); err != nil {
				return errors.New("Native app activation failed")
			}
			return nil
		},
		recover: RecoverApp,
		wait:    waitContext,
	}
}

func ensureDesktopExecutable(s *Settings, source string) (string, error) {
	path := filepath.Join(s.RuntimeRoot, "cq-dots-runtime")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o700 {
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
	const failure = "CQ could not start Dots. Run cq codex dots status for details."
	recoverNow := func() bool {
		// Startup cancellation must not cancel recovery of the user's other chats.
		recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return deps.recover(recovery, s) == nil
	}
	executable, err := deps.stage(s, c.Executable)
	if err != nil {
		recoverNow()
		return errors.New(failure)
	}
	env := os.Environ()
	if c.BaseURL != "" {
		env = setEnvironment(env, "OPENAI_BASE_URL", c.BaseURL)
	}
	env = setEnvironment(env, "CQ_DOTS_CQ_PATH", c.Executable)
	watchdog, err := deps.spawn(s, executable, "desktop-recover", nil, env)
	if err != nil {
		recoverNow()
		return errors.New(failure)
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
		stopWatchdog = recoverNow()
		return errors.New(failure)
	}
	if result["restart_required"] == true {
		stopWatchdog = recoverNow()
		return errors.New("Quit ChatGPT, then reopen ChatGPT via CQ. Active work has not been interrupted.")
	}
	if result["started"] == false {
		stopWatchdog = recoverNow()
		return errors.New("No active Dots are available. Run cq codex dots status for details.")
	}
	if err := deps.open(ctx, s.AppPath); err != nil {
		stopWatchdog = recoverNow()
		return errors.New(failure)
	}
	stopWatchdog = true
	return nil
}

// RunDesktopRecovery is a detached helper role, not a goroutine tied to the
// launcher process. It never stops or relaunches an already running app.
func RunDesktopRecovery(ctx context.Context, s *Settings) error {
	return runDesktopRecovery(ctx, s, desktopDefaults())
}

func runDesktopRecovery(ctx context.Context, s *Settings, deps desktopDependencies) error {
	if s == nil || s.IntegrationMode != "native" {
		return errors.New("Spotlight recovery requires a native Dots installation")
	}
	if err := deps.wait(ctx, desktopRecoveryDelay); err != nil {
		return err
	}
	if err := deps.recover(ctx, s); err != nil {
		return errors.New("Native app recovery failed")
	}
	return nil
}
