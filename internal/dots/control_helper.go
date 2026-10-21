package dots

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func helperBaseURL(role string, settings *Settings, current string, load func() (*proxy.Config, error), recoverApp func(context.Context, *Settings) error) (string, error) {
	if role != "desktop-launch" && role != "account-watch" {
		return current, nil
	}
	cfg, err := load()
	if err != nil {
		// Configuration fails before the launcher's watchdog exists. Keep the
		// ordinary app available even when its CQ entry point cannot start.
		if role == "desktop-launch" && settings.IntegrationMode == "native" {
			recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = recoverApp(recovery, settings)
		}
		return "", errors.New("CQ proxy configuration unavailable")
	}
	return "http://127.0.0.1:" + strconv.Itoa(cfg.Port), nil
}

// RunHelper dispatches private runtime workers in this same Go executable.
func RunHelper(ctx context.Context, role, root string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !cleanAbsolute(root) {
		return errors.New("Dots helper root must be a clean absolute path")
	}
	settings, err := LoadSettings(filepath.Join(root, "settings.json"))
	if err != nil {
		return err
	}
	if settings.RuntimeRoot != filepath.Join(root, "runtime") {
		return errors.New("Dots helper runtime differs from installation")
	}
	executable := os.Getenv("CQ_DOTS_CQ_PATH")
	if executable == "" {
		executable, err = exec.LookPath(os.Args[0])
		if err != nil {
			return err
		}
		executable, err = filepath.Abs(executable)
		if err != nil {
			return err
		}
	}
	baseURL, err := helperBaseURL(role, settings, os.Getenv("OPENAI_BASE_URL"), proxy.LoadExistingConfig, RecoverApp)
	if err != nil {
		return err
	}
	controller := Controller{Root: root, Executable: executable, BaseURL: baseURL, Stdout: stdout, Stderr: stderr}
	catalogue := controller.client(settings)
	native := controller.evaluator(settings)
	// The broker owns its app's CDP descriptors until that app exits, including
	// after a termination signal. Other workers use normal graceful cancellation.
	if role == "native-broker" {
		if settings.IntegrationMode == "native" {
			return LaunchNativeBroker(ctx, settings, filepath.Join(settings.AppPath, "Contents", "MacOS", "ChatGPT"), args, os.Environ())
		}
		return LaunchIsolatedApp(ctx, settings, args, os.Environ())
	}
	worker, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch role {
	case "probe":
		return RunProbe(worker, *settings)
	case "realtime":
		return RunRealtimeProxy(worker, settings)
	case "relay":
		return RunRoomCoordination(worker, settings, catalogue, native)
	case "executors":
		return RunSecondaryExecutors(worker, settings, catalogue, native)
	case "local-executor":
		return RunLocalExecutor(worker, settings, catalogue, args)
	case "native-executor":
		return RunNativeExecutorIO(worker, settings, catalogue, args, stdin, stdout, stderr)
	case "cloud-stdio", "cloud-stdio-bridge":
		inventory, err := catalogue.Inventory()
		if err != nil {
			inventory = Inventory{}
		}
		if role == "cloud-stdio-bridge" {
			local, err := cloudBridgeLocal()
			if err != nil {
				return err
			}
			defer local.stop()
			deps := cloudStdioDefaults(settings)
			deps.launch = func(context.Context, string, []string, []string, io.Writer) (*cloudLocal, error) { return local, nil }
			return runCloudStdio(worker, settings, inventory.Accounts, args, stdin, stdout, stderr, deps)
		}
		return RunCloudStdio(worker, settings, inventory.Accounts, args, stdin, stdout, stderr, native)
	case "account-watch":
		return RunAccountWatch(worker, settings, controller)
	case "desktop-launch":
		return RunDesktopLaunch(worker, settings, controller)
	case "desktop-recover":
		return RunDesktopRecovery(worker, settings)
	default:
		return errors.New("Unsupported Dots helper role")
	}
}
